// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

// Package workspaces is the Postgres-backed domain layer for workspaces,
// their members and invitations, and the documents they contain.
package workspaces

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/fydeos/fydeos-notes-server/internal/entitlements"
	entpg "github.com/fydeos/fydeos-notes-server/internal/entitlements/postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Querier is the subset of a pgx pool the repository needs.
type Querier interface {
	Begin(context.Context) (pgx.Tx, error)
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// queryer is satisfied by both a Querier and a pgx.Tx.
type queryer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// Repository owns workspace persistence.
type Repository struct {
	db         Querier
	deployment string
	now        func() time.Time
}

func NewRepository(db Querier) *Repository {
	return NewRepositoryWithDeployment(db, "selfhosted")
}

func NewRepositoryWithDeployment(db Querier, deployment string) *Repository {
	if strings.TrimSpace(deployment) == "" {
		deployment = "selfhosted"
	}
	return &Repository{db: db, deployment: deployment, now: time.Now}
}

func NewRepositoryWithEntitlementConfig(db Querier, deployment string) *Repository {
	return NewRepositoryWithDeployment(db, deployment)
}

func (r *Repository) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

func (r *Repository) sqlCommercialShortcut() bool {
	d := strings.ToLower(strings.TrimSpace(r.deployment))
	return d == "cloud" || d == "affine"
}

// inTx runs fn in a transaction, committing on success.
func (r *Repository) inTx(ctx context.Context, opts *pgx.TxOptions, fn func(pgx.Tx) error) error {
	var (
		tx  pgx.Tx
		err error
	)
	if opts != nil {
		starter, ok := r.db.(interface {
			BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
		})
		if ok {
			tx, err = starter.BeginTx(ctx, *opts)
		} else {
			tx, err = r.db.Begin(ctx)
			if err == nil {
				_, err = tx.Exec(ctx, "SET TRANSACTION ISOLATION LEVEL "+string(opts.IsoLevel)+accessModeSQL(opts.AccessMode))
			}
		}
	} else {
		tx, err = r.db.Begin(ctx)
	}
	if err != nil {
		return fmt.Errorf("begin workspace transaction: %w", err)
	}
	defer rollback(ctx, tx)
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit workspace transaction: %w", err)
	}
	return nil
}

func accessModeSQL(mode pgx.TxAccessMode) string {
	if mode == pgx.ReadOnly {
		return " READ ONLY"
	}
	return ""
}

var readOnlySnapshot = &pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}

// rollback uses a detached context so a cancelled request still releases
// locks promptly.
func rollback(ctx context.Context, tx pgx.Tx) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(rctx)
}

func noRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

func advisoryKey(prefix, id string) string { return prefix + id }

func lockExclusive(ctx context.Context, q queryer, key string) error {
	if _, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, key); err != nil {
		return fmt.Errorf("acquire lock %q: %w", key, err)
	}
	return nil
}

func lockShared(ctx context.Context, q queryer, key string) error {
	if _, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock_shared(hashtextextended($1, 0))`, key); err != nil {
		return fmt.Errorf("acquire shared lock %q: %w", key, err)
	}
	return nil
}

func tryLockExclusive(ctx context.Context, q queryer, key string) (bool, error) {
	var ok bool
	if err := q.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1, 0))`, key).Scan(&ok); err != nil {
		return false, fmt.Errorf("try lock %q: %w", key, err)
	}
	return ok, nil
}

func setLockTimeout(ctx context.Context, q queryer, d time.Duration) error {
	_, err := q.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", d.Milliseconds()))
	return err
}

func storageLockKey(ws string) string { return "storage-workspace:" + ws }
func domainLockKey(ws string) string  { return "domain:workspace:" + ws }

func dbNow(ctx context.Context, q queryer) (time.Time, error) {
	var now time.Time
	if err := q.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
		return time.Time{}, fmt.Errorf("read database clock: %w", err)
	}
	return now, nil
}

// grantFor resolves the entitlement grant of one target at a given time.
func (r *Repository) grantFor(ctx context.Context, q queryer, targetType, id string, now time.Time) (entitlements.Grant, error) {
	facts, err := entpg.LoadFacts(ctx, q, targetType, []string{id})
	if err != nil {
		return entitlements.Grant{}, err
	}
	ectx := entitlements.Context{Deployment: entitlements.ParseDeployment(r.deployment), TargetType: targetType, Now: now}
	if targetType == "workspace" {
		ectx.WorkspaceID = id
	}
	return entitlements.Resolve(ectx, facts[id]), nil
}

func (r *Repository) commercial(ctx context.Context, q queryer, workspaceID string) (bool, error) {
	now, err := dbNow(ctx, q)
	if err != nil {
		return false, err
	}
	grant, err := r.grantFor(ctx, q, "workspace", workspaceID, now)
	if err != nil {
		return false, err
	}
	return grant.Commercial, nil
}

// WorkspaceCommercialEntitlement reports whether the workspace has a
// commercial grant according to the entitlement resolver.
func (r *Repository) WorkspaceCommercialEntitlement(ctx context.Context, workspaceID string) (bool, error) {
	return r.commercial(ctx, r.db, workspaceID)
}

func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }

func sortedUnique(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func workspaceExists(ctx context.Context, q queryer, id string) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspaces WHERE id=$1)`, id).Scan(&ok)
	return ok, err
}

// activeRole returns the stored role of the user's active membership, or "".
func activeRole(ctx context.Context, q queryer, workspaceID, userID string) (string, error) {
	var role string
	err := q.QueryRow(ctx, `SELECT role FROM workspace_members WHERE workspace_id=$1 AND user_id=$2 AND state='active'`, workspaceID, userID).Scan(&role)
	if noRows(err) {
		return "", nil
	}
	return role, err
}

func earliestOwner(ctx context.Context, q queryer, workspaceID string) (string, error) {
	var owner string
	err := q.QueryRow(ctx, `SELECT user_id FROM workspace_members WHERE workspace_id=$1 AND role='owner' AND state='active' ORDER BY created_at, id LIMIT 1`, workspaceID).Scan(&owner)
	if noRows(err) {
		return "", nil
	}
	return owner, err
}

// newRowID returns an id in the style of the schema's permission ids.
func newRowID() string { return "perm_" + strings.ReplaceAll(uuid.NewString(), "-", "") }

func ptr[T any](v T) *T { return &v }

func nonEmptyPtr(s *string) *string {
	if s == nil || *s == "" {
		return nil
	}
	return s
}
