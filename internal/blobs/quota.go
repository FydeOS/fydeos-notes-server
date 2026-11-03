// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package blobs

import (
	"context"
	"math"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/entitlements"
	"github.com/jackc/pgx/v5"
)

// membership returns the preferred membership row of a user.
func membership(ctx context.Context, q queryer, actorID, workspaceID string) (role string, active bool, err error) {
	var state string
	err = q.QueryRow(ctx, `SELECT role, state FROM workspace_members
		WHERE workspace_id = $1 AND user_id = $2
		ORDER BY (state = 'active') DESC, updated_at DESC LIMIT 1`, workspaceID, actorID).Scan(&role, &state)
	if isNoRows(err) {
		return "", false, nil
	}
	return role, state == "active", err
}

func requireUploader(ctx context.Context, q queryer, actorID, workspaceID string) error {
	role, active, err := membership(ctx, q, actorID, workspaceID)
	if err != nil {
		return err
	}
	if !active || (role != "owner" && role != "admin" && role != "member") {
		return errUploadForbidden
	}
	return nil
}

func (s *Service) requireManager(ctx context.Context, q queryer, actorID, workspaceID string) error {
	var exists bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM workspaces WHERE id = $1)`, workspaceID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return errWorkspaceNotFound
	}
	role, active, err := membership(ctx, q, actorID, workspaceID)
	if err != nil {
		return err
	}
	if active && role == "owner" {
		return nil
	}
	if active && role == "admin" {
		grant, err := s.workspaceGrant(ctx, q, workspaceID, time.Now())
		if err != nil {
			return err
		}
		if grant.Commercial {
			return nil
		}
	}
	return errManageForbidden
}

func activeOwner(ctx context.Context, q queryer, workspaceID string) (string, error) {
	var owner string
	err := q.QueryRow(ctx, `SELECT user_id FROM workspace_members
		WHERE workspace_id = $1 AND role = 'owner' AND state = 'active'
		ORDER BY updated_at DESC, user_id LIMIT 1`, workspaceID).Scan(&owner)
	if isNoRows(err) {
		return "", nil
	}
	return owner, err
}

func loadFacts(ctx context.Context, q queryer, targetType, targetID string) ([]entitlements.Fact, error) {
	rows, err := q.Query(ctx, `SELECT source, status, plan, quantity, starts_at, expires_at, grace_until, signed_payload
		FROM entitlements WHERE target_type = $1 AND target_id = $2`, targetType, targetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var facts []entitlements.Fact
	for rows.Next() {
		var f entitlements.Fact
		if err := rows.Scan(&f.Source, &f.Status, &f.Plan, &f.Quantity, &f.StartsAt, &f.ExpiresAt, &f.GraceUntil, &f.SignedPayload); err != nil {
			return nil, err
		}
		facts = append(facts, f)
	}
	return facts, rows.Err()
}

func (s *Service) resolveGrant(ctx context.Context, q queryer, targetType, targetID, workspaceID string, now time.Time) (entitlements.Grant, error) {
	facts, err := loadFacts(ctx, q, targetType, targetID)
	if err != nil {
		return entitlements.Grant{}, err
	}
	return entitlements.Resolve(entitlements.Context{Deployment: s.deployment, TargetType: targetType, WorkspaceID: workspaceID, Now: now}, facts), nil
}

func (s *Service) workspaceGrant(ctx context.Context, q queryer, workspaceID string, now time.Time) (entitlements.Grant, error) {
	return s.resolveGrant(ctx, q, "workspace", workspaceID, workspaceID, now)
}

// quotaState is the quota snapshot for one workspace taken inside a
// transaction that holds the owner and workspace row locks.
type quotaState struct {
	ownerID  string
	now      time.Time
	grant    entitlements.Grant
	used     int64
	readOnly bool
}

// lockQuota locks the quota subject rows and computes current usage.
func (s *Service) lockQuota(ctx context.Context, tx pgx.Tx, workspaceID string) (quotaState, error) {
	owner, err := activeOwner(ctx, tx, workspaceID)
	if err != nil {
		return quotaState{}, err
	}
	if owner == "" {
		return quotaState{}, errWorkspaceNotFound
	}
	var id string
	if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id = $1 FOR UPDATE`, owner).Scan(&id); err != nil {
		if isNoRows(err) {
			return quotaState{}, errWorkspaceNotFound
		}
		return quotaState{}, err
	}
	if err := tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id = $1 FOR UPDATE`, workspaceID).Scan(&id); err != nil {
		if isNoRows(err) {
			return quotaState{}, errWorkspaceNotFound
		}
		return quotaState{}, err
	}
	if s.afterOwnerRead != nil {
		s.afterOwnerRead(ctx)
	}
	again, err := activeOwner(ctx, tx, workspaceID)
	if err != nil {
		return quotaState{}, err
	}
	if again != owner {
		return quotaState{}, errQuotaSubjectChanged
	}
	state := quotaState{ownerID: owner}
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&state.now); err != nil {
		return quotaState{}, err
	}
	wsGrant, err := s.workspaceGrant(ctx, tx, workspaceID, state.now)
	if err != nil {
		return quotaState{}, err
	}
	charged := []string{workspaceID}
	if wsGrant.Commercial {
		state.grant = wsGrant
	} else {
		if state.grant, err = s.resolveGrant(ctx, tx, "user", owner, workspaceID, state.now); err != nil {
			return quotaState{}, err
		}
		if charged, err = s.ownedFreeWorkspaces(ctx, tx, owner, workspaceID, state.now); err != nil {
			return quotaState{}, err
		}
	}
	if err := tx.QueryRow(ctx, `SELECT
		COALESCE((SELECT SUM(size) FROM blobs WHERE workspace_id = ANY($1) AND deleted_at IS NULL
			AND (status = 'completed' OR (status = 'pending' AND reservation_expires_at > $2))), 0)::bigint +
		COALESCE((SELECT SUM(size) FROM comment_attachments WHERE workspace_id = ANY($1) AND deleted_at IS NULL
			AND (status = 'completed' OR (status = 'pending' AND reservation_expires_at > $2))), 0)::bigint`,
		charged, state.now).Scan(&state.used); err != nil {
		return quotaState{}, err
	}
	var seats int64
	if err := tx.QueryRow(ctx, `SELECT
		(SELECT COUNT(*) FROM workspace_members WHERE workspace_id = $1 AND state = 'active') +
		(SELECT COUNT(*) FROM workspace_invitations WHERE workspace_id = $1 AND status IN ('pending', 'waiting_review', 'waiting_seat'))`,
		workspaceID).Scan(&seats); err != nil {
		return quotaState{}, err
	}
	state.readOnly = seats > int64(state.grant.Limits.SeatLimit) || state.used > state.grant.Limits.StorageQuota
	return state, nil
}

func (s *Service) ownedFreeWorkspaces(ctx context.Context, tx pgx.Tx, owner, current string, now time.Time) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT DISTINCT workspace_id FROM workspace_members
		WHERE user_id = $1 AND role = 'owner' AND state = 'active'`, owner)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	charged := []string{current}
	for _, id := range ids {
		if id == current {
			continue
		}
		grant, err := s.workspaceGrant(ctx, tx, id, now)
		if err != nil {
			return nil, err
		}
		if !grant.Commercial {
			charged = append(charged, id)
		}
	}
	return charged, nil
}

// admit decides whether a reservation that adds increase bytes for a blob
// of size bytes may proceed.
func (q quotaState) admit(size, increase int64) error {
	if size > q.grant.Limits.BlobLimit {
		return errBlobLimit
	}
	if q.readOnly {
		return errStorageLimit
	}
	if increase > 0 && q.used > math.MaxInt64-increase {
		return errStorageLimit
	}
	if q.used+increase > q.grant.Limits.StorageQuota {
		return errStorageLimit
	}
	return nil
}
