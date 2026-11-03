// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package workspaces

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

type DocumentQuotaGuard interface {
	CheckWorkspaceMutationQuotaInTx(context.Context, pgx.Tx, string) error
}

type DocumentRoleService struct {
	repository *Repository
	quota      DocumentQuotaGuard
}

func NewDocumentRoleService(repository *Repository, quota DocumentQuotaGuard) *DocumentRoleService {
	return &DocumentRoleService{repository: repository, quota: quota}
}

type DocumentPublicationService struct {
	repository *Repository
	quota      DocumentQuotaGuard
}

func NewDocumentPublicationService(repository *Repository, quota DocumentQuotaGuard) *DocumentPublicationService {
	return &DocumentPublicationService{repository: repository, quota: quota}
}

const (
	aclDeadline    = 10 * time.Second
	aclLockTimeout = 5 * time.Second
)

func checkDocInput(actor, workspace, doc string, targets []string) error {
	if len(targets) > MaxDocRoleTargets {
		return ErrDocRoleInputLimit
	}
	for _, id := range append([]string{actor, workspace, doc}, targets...) {
		if len(id) > MaxDocRoleIdentifierBytes {
			return ErrDocRoleInputLimit
		}
	}
	if actor == "" || workspace == "" || doc == "" || doc == workspace {
		return ErrDocRoleInvalid
	}
	return nil
}

// docMutation describes one locked ACL change.
type docMutation struct {
	workspace, doc, actor string
	users                 []string // actor plus targets
	extraLocks            []string // additional exclusive advisory keys
	needQuota             bool
	quota                 DocumentQuotaGuard
	// authorize sees the locked facts of the actor.
	authorize func(f docFacts) error
	apply     func(tx pgx.Tx) error
}

func readGrants(ctx context.Context, q queryer, workspace, doc string, users []string) (map[string]string, error) {
	rows, err := q.Query(ctx, `SELECT principal_id, role FROM doc_grants WHERE workspace_id=$1 AND doc_id=$2 AND principal_type='user' AND principal_id = ANY($3::text[])`,
		workspace, doc, users)
	if err != nil {
		return nil, fmt.Errorf("read doc grants: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, role string
		if err := rows.Scan(&id, &role); err != nil {
			return nil, err
		}
		out[id] = role
	}
	return out, rows.Err()
}

func sameGrants(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// runDocMutation acquires the ACL lock ladder, authorizes, and applies.
func (r *Repository) runDocMutation(ctx context.Context, m docMutation) error {
	ctx, cancel := context.WithTimeout(ctx, aclDeadline)
	defer cancel()
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin doc acl transaction: %w", err)
	}
	defer rollback(ctx, tx)
	if err := setLockTimeout(ctx, tx, aclLockTimeout); err != nil {
		return err
	}
	if err := lockShared(ctx, tx, storageLockKey(m.workspace)); err != nil {
		return err
	}
	if err := lockExclusive(ctx, tx, domainLockKey(m.workspace)); err != nil {
		return err
	}
	for _, key := range m.extraLocks {
		if err := lockExclusive(ctx, tx, key); err != nil {
			return err
		}
	}
	var quotaErr error
	if m.needQuota {
		if m.quota == nil {
			quotaErr = ErrDocQuotaUnavailable
		} else {
			quotaErr = m.quota.CheckWorkspaceMutationQuotaInTx(ctx, tx, m.workspace)
		}
	}
	if err := lockACLRows(ctx, tx, m.workspace, m.doc, m.users); err != nil {
		return err
	}
	if m.authorize != nil {
		commercial, err := r.commercial(ctx, tx, m.workspace)
		if err != nil {
			return err
		}
		b, err := r.loadBundle(ctx, tx, m.actor, m.workspace, []string{m.doc})
		if err != nil {
			return err
		}
		if err := m.authorize(b.doc(m.doc, commercial)); err != nil {
			return err
		}
	}
	if quotaErr != nil {
		return quotaErr
	}
	if err := m.apply(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit doc acl transaction: %w", err)
	}
	return nil
}

func lockACLRows(ctx context.Context, tx pgx.Tx, workspace, doc string, users []string) error {
	if err := lockWorkspaceRow(ctx, tx, workspace); err != nil {
		return err
	}
	users = sortedUnique(users)
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`SELECT 1 FROM workspace_access_policies WHERE workspace_id=$1 FOR UPDATE`, []any{workspace}},
		{`SELECT 1 FROM workspace_members WHERE workspace_id=$1 AND user_id = ANY($2::text[]) ORDER BY id FOR UPDATE`, []any{workspace, users}},
		{`SELECT 1 FROM doc_access_policies WHERE workspace_id=$1 AND doc_id=$2 FOR UPDATE`, []any{workspace, doc}},
		{`SELECT 1 FROM doc_grants WHERE workspace_id=$1 AND doc_id=$2 AND principal_type='user' AND principal_id = ANY($3::text[]) ORDER BY principal_id FOR UPDATE`, []any{workspace, doc, users}},
		{`INSERT INTO workspace_sync_permission_generations(workspace_id, generation) VALUES ($1, 0) ON CONFLICT (workspace_id) DO NOTHING`, []any{workspace}},
		{`SELECT 1 FROM workspace_sync_permission_generations WHERE workspace_id=$1 FOR UPDATE`, []any{workspace}},
	} {
		if _, err := tx.Exec(ctx, stmt.sql, stmt.args...); err != nil {
			return fmt.Errorf("lock doc acl rows: %w", err)
		}
	}
	return nil
}

func requireRole(min docRole) func(docFacts) error {
	return func(f docFacts) error {
		if f.effectiveRole() < min {
			return ErrDocRoleDenied
		}
		return nil
	}
}

func upsertGrant(ctx context.Context, tx pgx.Tx, workspace, doc, user, role, actor string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO doc_grants(workspace_id, doc_id, principal_type, principal_id, role, granted_by)
		VALUES ($1,$2,'user',$3,$4,$5)
		ON CONFLICT (workspace_id, doc_id, principal_type, principal_id) DO UPDATE SET role=EXCLUDED.role, granted_by=EXCLUDED.granted_by, updated_at=CURRENT_TIMESTAMP`,
		workspace, doc, user, role, actor)
	if err != nil {
		return fmt.Errorf("write doc grant: %w", err)
	}
	return nil
}

func grantsUnchanged(ctx context.Context, tx pgx.Tx, workspace, doc string, users []string, before map[string]string) error {
	after, err := readGrants(ctx, tx, workspace, doc, users)
	if err != nil {
		return err
	}
	if !sameGrants(before, after) {
		return ErrPermissionGenerationChanged
	}
	return nil
}

// GrantDocRoles grants one role to several users.
func (s *DocumentRoleService) GrantDocRoles(ctx context.Context, actor, workspace, doc string, targets []string, role string) error {
	if s == nil || s.repository == nil {
		return ErrAuthorizationUnavailable
	}
	if err := checkDocInput(actor, workspace, doc, targets); err != nil {
		return err
	}
	newRole := parseDocRole(role)
	if !validDocRoleName(role) || newRole < roleReader || newRole > roleManager {
		return ErrDocRoleInvalid
	}
	ids := sortedUnique(targets)
	if len(ids) == 0 || len(ids) != countNonEmpty(targets) {
		return ErrDocRoleInvalid
	}
	before, err := readGrants(ctx, s.repository.db, workspace, doc, ids)
	if err != nil {
		return err
	}
	contraction := true
	for _, id := range ids {
		cur, ok := before[id]
		if !ok || parseDocRole(cur) <= newRole {
			contraction = false
		}
	}
	return s.repository.runDocMutation(ctx, docMutation{
		workspace: workspace, doc: doc, actor: actor, users: append([]string{actor}, ids...),
		needQuota: !contraction, quota: s.quota, authorize: requireRole(roleManager),
		apply: func(tx pgx.Tx) error {
			if err := grantsUnchanged(ctx, tx, workspace, doc, ids, before); err != nil {
				return err
			}
			for _, id := range ids {
				if before[id] == "owner" {
					return ErrDocOwnerMustTransfer
				}
			}
			for _, id := range ids {
				if err := upsertGrant(ctx, tx, workspace, doc, id, newRole.String(), actor); err != nil {
					return err
				}
			}
			return nil
		},
	})
}

func countNonEmpty(values []string) int {
	seen := map[string]struct{}{}
	for _, v := range values {
		if v == "" {
			return -1
		}
		seen[v] = struct{}{}
	}
	return len(seen)
}

// TransitionDocRole sets, removes ("") or transfers ("owner") one grant.
func (s *DocumentRoleService) TransitionDocRole(ctx context.Context, actor, workspace, doc, target, role string) error {
	if s == nil || s.repository == nil {
		return ErrAuthorizationUnavailable
	}
	if err := checkDocInput(actor, workspace, doc, []string{target}); err != nil {
		return err
	}
	if target == "" {
		return ErrDocRoleInvalid
	}
	newRole := roleNone
	if role != "" {
		newRole = parseDocRole(role)
		if !validDocRoleName(role) || newRole < roleReader {
			return ErrDocRoleInvalid
		}
	}
	before, err := readGrants(ctx, s.repository.db, workspace, doc, []string{target})
	if err != nil {
		return err
	}
	cur, hasCur := before[target]
	contraction := role == "" || (hasCur && newRole < parseDocRole(cur))
	minRole := roleManager
	if newRole == roleOwner {
		minRole = roleOwner
	}
	return s.repository.runDocMutation(ctx, docMutation{
		workspace: workspace, doc: doc, actor: actor, users: []string{actor, target},
		needQuota: !contraction, quota: s.quota, authorize: requireRole(minRole),
		apply: func(tx pgx.Tx) error {
			if err := grantsUnchanged(ctx, tx, workspace, doc, []string{target}, before); err != nil {
				return err
			}
			if cur == "owner" && newRole != roleOwner {
				return ErrDocOwnerMustTransfer
			}
			if role == "" {
				_, err := tx.Exec(ctx, `DELETE FROM doc_grants WHERE workspace_id=$1 AND doc_id=$2 AND principal_type='user' AND principal_id=$3`, workspace, doc, target)
				return err
			}
			if newRole == roleOwner {
				r, err := activeRole(ctx, tx, workspace, target)
				if err != nil {
					return err
				}
				if r == "" {
					return ErrDocOwnerTargetInactive
				}
				if _, err := tx.Exec(ctx, `UPDATE doc_grants SET role='manager', updated_at=CURRENT_TIMESTAMP
					WHERE workspace_id=$1 AND doc_id=$2 AND principal_type='user' AND role='owner' AND principal_id<>$3`, workspace, doc, target); err != nil {
					return err
				}
			}
			return upsertGrant(ctx, tx, workspace, doc, target, newRole.String(), actor)
		},
	})
}

func effectiveDefault(ctx context.Context, q queryer, workspace, doc string) (string, error) {
	var def string
	err := q.QueryRow(ctx, `SELECT COALESCE(
		(SELECT member_default_role FROM doc_access_policies WHERE workspace_id=$1 AND doc_id=$2),
		(SELECT to_jsonb(p)->>'member_default_doc_role' FROM workspace_access_policies p WHERE p.workspace_id=$1),
		'manager')`, workspace, doc).Scan(&def)
	return def, err
}

// SetDocDefaultRole sets the doc's default role for workspace members.
func (s *DocumentRoleService) SetDocDefaultRole(ctx context.Context, actor, workspace, doc, role string) error {
	if s == nil || s.repository == nil {
		return ErrAuthorizationUnavailable
	}
	if err := checkDocInput(actor, workspace, doc, nil); err != nil {
		return err
	}
	switch role {
	case "none", "reader", "commenter", "editor", "manager":
	default:
		return ErrDocRoleInvalid
	}
	prev, err := effectiveDefault(ctx, s.repository.db, workspace, doc)
	if err != nil {
		return err
	}
	return s.repository.runDocMutation(ctx, docMutation{
		workspace: workspace, doc: doc, actor: actor, users: []string{actor},
		needQuota: !(parseDocRole(role) < parseDocRole(prev)), quota: s.quota, authorize: requireRole(roleManager),
		apply: func(tx pgx.Tx) error {
			now, err := effectiveDefault(ctx, tx, workspace, doc)
			if err != nil {
				return err
			}
			if now != prev {
				return ErrPermissionGenerationChanged
			}
			_, err = tx.Exec(ctx, `
				INSERT INTO doc_access_policies(workspace_id, doc_id, visibility, member_default_role) VALUES ($1,$2,'private',$3)
				ON CONFLICT (workspace_id, doc_id) DO UPDATE SET member_default_role=EXCLUDED.member_default_role, updated_at=CURRENT_TIMESTAMP`,
				workspace, doc, role)
			return err
		},
	})
}

func docHasContent(ctx context.Context, q queryer, workspace, doc string) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM snapshots WHERE workspace_id=$1 AND guid=$2)
		OR EXISTS(SELECT 1 FROM updates WHERE workspace_id=$1 AND guid=$2)`, workspace, doc).Scan(&ok)
	return ok, err
}

// SetDocPublished publishes or unpublishes a document.
func (s *DocumentPublicationService) SetDocPublished(ctx context.Context, actor, workspace, doc string, mode int, publish bool) (PublishedDocumentMetadata, error) {
	var out PublishedDocumentMetadata
	if s == nil || s.repository == nil {
		return out, ErrAuthorizationUnavailable
	}
	if err := checkDocInput(actor, workspace, doc, nil); err != nil {
		return out, err
	}
	if mode != 0 && mode != 1 {
		return out, ErrDocRoleInvalid
	}
	err := s.repository.runDocMutation(ctx, docMutation{
		workspace: workspace, doc: doc, actor: actor, users: []string{actor},
		needQuota: publish, quota: s.quota,
		authorize: func(f docFacts) error {
			if f.effectiveRole() < roleManager || (publish && !f.sharing) {
				return ErrDocRoleDenied
			}
			return nil
		},
		apply: func(tx pgx.Tx) error {
			if publish {
				ok, err := docHasContent(ctx, tx, workspace, doc)
				if err != nil {
					return err
				}
				if !ok {
					return ErrDocumentNotFound
				}
				if _, err := tx.Exec(ctx, `
					INSERT INTO doc_access_policies(workspace_id, doc_id, visibility, public_role, published_at) VALUES ($1,$2,'public','external',CURRENT_TIMESTAMP)
					ON CONFLICT (workspace_id, doc_id) DO UPDATE SET visibility='public', public_role='external', published_at=CURRENT_TIMESTAMP, updated_at=CURRENT_TIMESTAMP`,
					workspace, doc); err != nil {
					return err
				}
				_, err = tx.Exec(ctx, `
					INSERT INTO workspace_pages(workspace_id, page_id, mode, published_at) VALUES ($1,$2,$3,CURRENT_TIMESTAMP)
					ON CONFLICT (workspace_id, page_id) DO UPDATE SET mode=EXCLUDED.mode, published_at=EXCLUDED.published_at`, workspace, doc, mode)
				return err
			}
			var public bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM doc_access_policies WHERE workspace_id=$1 AND doc_id=$2 AND visibility='public' AND public_role='external')`,
				workspace, doc).Scan(&public); err != nil {
				return err
			}
			if !public {
				return ErrDocumentNotPublic
			}
			if _, err := tx.Exec(ctx, `UPDATE doc_access_policies SET visibility='private', public_role=NULL, published_at=NULL, updated_at=CURRENT_TIMESTAMP
				WHERE workspace_id=$1 AND doc_id=$2`, workspace, doc); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE workspace_pages SET published_at=NULL WHERE workspace_id=$1 AND page_id=$2`, workspace, doc)
			return err
		},
	})
	if err != nil {
		return out, err
	}
	return s.repository.readPublishedMetadata(ctx, s.repository.db, workspace, doc)
}

func (r *Repository) readPublishedMetadata(ctx context.Context, q queryer, workspace, doc string) (PublishedDocumentMetadata, error) {
	out := PublishedDocumentMetadata{WorkspaceID: workspace, DocID: doc, DefaultRole: "manager"}
	var def *string
	err := q.QueryRow(ctx, `
		SELECT COALESCE(wp.mode, 0), wp.title, wp.summary,
		       COALESCE(p.visibility='public' AND p.public_role='external', false), p.member_default_role
		FROM (SELECT 1) one
		LEFT JOIN workspace_pages wp ON wp.workspace_id=$1 AND wp.page_id=$2
		LEFT JOIN doc_access_policies p ON p.workspace_id=$1 AND p.doc_id=$2`, workspace, doc).
		Scan(&out.Mode, &out.Title, &out.Summary, &out.Public, &def)
	if err != nil {
		return out, err
	}
	if def != nil {
		out.DefaultRole = *def
	}
	return out, nil
}
