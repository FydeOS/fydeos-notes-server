// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package entitlements

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrAdminGrantUnavailable = errors.New("admin entitlement grant service is unavailable")

type AdminGrantError struct {
	Reason string
}

func (e *AdminGrantError) Error() string { return e.Reason }

type AdminGrantInput struct {
	TargetType string
	TargetID   string
	Plan       string
	Quantity   *int32
}

type AdminGrantChange struct {
	TargetType string
	TargetID   string
	OwnerIDs   []string
}

type adminGrantDB interface {
	Begin(context.Context) (pgx.Tx, error)
}

type AdminGrantStore struct {
	db         adminGrantDB
	deployment string
}

func NewAdminGrantStore(db adminGrantDB, deployment string) *AdminGrantStore {
	return &AdminGrantStore{db: db, deployment: deployment}
}

func (s *AdminGrantStore) Upsert(ctx context.Context, input AdminGrantInput) (AdminGrantChange, error) {
	if s == nil || s.db == nil {
		return AdminGrantChange{}, ErrAdminGrantUnavailable
	}
	input, err := validateAdminGrant(s.deployment, input)
	if err != nil {
		return AdminGrantChange{}, err
	}
	target, subject := adminGrantIdentity(input)
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return AdminGrantChange{}, fmt.Errorf("begin admin entitlement grant: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := lockAdminGrantSource(ctx, tx, subject); err != nil {
		return AdminGrantChange{}, err
	}
	owners, err := lockAdminGrantTarget(ctx, tx, target)
	if err != nil {
		return AdminGrantChange{}, err
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return AdminGrantChange{}, fmt.Errorf("read admin entitlement decision time: %w", err)
	}
	wasCommercial := false
	if input.TargetType == "workspace" {
		wasCommercial, err = hasCommercialWorkspaceGrant(ctx, tx, input.TargetID, now)
		if err != nil {
			return AdminGrantChange{}, err
		}
	}

	var id string
	err = tx.QueryRow(ctx, `SELECT id FROM entitlements
		WHERE source='admin_grant' AND subject_id=$1 ORDER BY updated_at DESC LIMIT 1`, subject).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		id = uuid.NewString()
	} else if err != nil {
		return AdminGrantChange{}, fmt.Errorf("find admin entitlement grant: %w", err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO entitlements(
		id,target_type,target_id,source,subject_id,plan,status,quantity,signed_payload,metadata,
		starts_at,expires_at,grace_until,validated_at,updated_at
	) VALUES($1,$2,$3,'admin_grant',$4,$5,'active',$6,NULL,'{}'::jsonb,NULL,NULL,NULL,$7,$7)
	ON CONFLICT(id) DO UPDATE SET target_type=EXCLUDED.target_type,target_id=EXCLUDED.target_id,
		plan=EXCLUDED.plan,status=EXCLUDED.status,quantity=EXCLUDED.quantity,signed_payload=NULL,
		metadata=EXCLUDED.metadata,starts_at=NULL,expires_at=NULL,grace_until=NULL,
		validated_at=EXCLUDED.validated_at,updated_at=EXCLUDED.updated_at`,
		id, input.TargetType, input.TargetID, subject, input.Plan, input.Quantity, now)
	if err != nil {
		return AdminGrantChange{}, fmt.Errorf("upsert admin entitlement grant: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE entitlements SET status='revoked',updated_at=$3
		WHERE source='admin_grant' AND target_type=$1 AND target_id=$2
		  AND status IN ('active','grace') AND id<>$4`, input.TargetType, input.TargetID, now, id); err != nil {
		return AdminGrantChange{}, fmt.Errorf("replace active admin entitlement grants: %w", err)
	}
	stillCommercial := false
	if input.TargetType == "workspace" {
		stillCommercial, err = hasCommercialWorkspaceGrant(ctx, tx, input.TargetID, now)
		if err != nil {
			return AdminGrantChange{}, err
		}
	}
	if wasCommercial && !stillCommercial {
		if _, err := tx.Exec(ctx, `DELETE FROM workspace_invitations WHERE workspace_id=$1
			AND status IN ('pending','waiting_review','waiting_seat')`, input.TargetID); err != nil {
			return AdminGrantChange{}, fmt.Errorf("revoke invitations after commercial entitlement downgrade: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE workspace_members SET role='member',updated_at=$2
			WHERE workspace_id=$1 AND role='admin' AND state='active'`, input.TargetID, now); err != nil {
			return AdminGrantChange{}, fmt.Errorf("demote workspace admins after commercial entitlement downgrade: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return AdminGrantChange{}, fmt.Errorf("commit admin entitlement grant: %w", err)
	}
	return AdminGrantChange{TargetType: input.TargetType, TargetID: input.TargetID, OwnerIDs: owners}, nil
}

func (s *AdminGrantStore) Revoke(ctx context.Context, targetType, targetID string) (AdminGrantChange, error) {
	if s == nil || s.db == nil {
		return AdminGrantChange{}, ErrAdminGrantUnavailable
	}
	if (targetType != "user" && targetType != "workspace") || targetID == "" {
		return AdminGrantChange{}, &AdminGrantError{Reason: "targetType must be user or workspace and targetId is required"}
	}
	subject := "admin_grant:" + targetType + ":" + targetID
	target := AdminGrantChange{TargetType: targetType, TargetID: targetID}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return AdminGrantChange{}, fmt.Errorf("begin admin entitlement revoke: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := lockAdminGrantSource(ctx, tx, subject); err != nil {
		return AdminGrantChange{}, err
	}
	owners, err := lockAdminGrantTarget(ctx, tx, AdminGrantChange{TargetType: targetType, TargetID: targetID})
	if err != nil {
		return AdminGrantChange{}, err
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return AdminGrantChange{}, fmt.Errorf("read admin entitlement revoke time: %w", err)
	}
	wasCommercial := false
	if targetType == "workspace" && s.deployment != "selfhosted" {
		wasCommercial, err = hasCommercialWorkspaceGrant(ctx, tx, targetID, now)
		if err != nil {
			return AdminGrantChange{}, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE entitlements SET status='revoked',updated_at=$2
		WHERE source='admin_grant' AND subject_id=$1 AND status IN ('active','grace')`, subject, now); err != nil {
		return AdminGrantChange{}, fmt.Errorf("revoke admin entitlement grant: %w", err)
	}
	if wasCommercial {
		stillCommercial, err := hasCommercialWorkspaceGrant(ctx, tx, targetID, now)
		if err != nil {
			return AdminGrantChange{}, err
		}
		if !stillCommercial {
			if _, err := tx.Exec(ctx, `DELETE FROM workspace_invitations WHERE workspace_id=$1
				AND status IN ('pending','waiting_review','waiting_seat')`, targetID); err != nil {
				return AdminGrantChange{}, fmt.Errorf("revoke invitations after commercial entitlement revoke: %w", err)
			}
			if _, err := tx.Exec(ctx, `UPDATE workspace_members SET role='member',updated_at=$2
				WHERE workspace_id=$1 AND role='admin' AND state='active'`, targetID, now); err != nil {
				return AdminGrantChange{}, fmt.Errorf("demote workspace admins after commercial entitlement revoke: %w", err)
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return AdminGrantChange{}, fmt.Errorf("commit admin entitlement revoke: %w", err)
	}
	target.OwnerIDs = owners
	return target, nil
}

func validateAdminGrant(deployment string, input AdminGrantInput) (AdminGrantInput, error) {
	invalid := func(reason string) (AdminGrantInput, error) {
		return AdminGrantInput{}, &AdminGrantError{Reason: reason}
	}
	if deployment == "selfhosted" {
		return invalid("Self-hosted commercial entitlements require a signed license.")
	}
	if input.TargetType != "user" && input.TargetType != "workspace" {
		return invalid("targetType must be user or workspace")
	}
	if input.TargetID == "" {
		return invalid("entitlement target id is required")
	}
	if input.Quantity != nil && (*input.Quantity < 1 || *input.Quantity > 100000) {
		return invalid("quantity must be between 1 and 100000")
	}
	valid := input.TargetType == "user" && (input.Plan == "free" || input.Plan == "pro" || input.Plan == "lifetime_pro" || input.Plan == "ai")
	valid = valid || input.TargetType == "workspace" && (input.Plan == "free" || input.Plan == "team")
	if !valid {
		if !knownAdminGrantPlan(input.Plan) {
			return invalid("unknown entitlement plan")
		}
		return invalid("entitlement plan is not configurable for target type")
	}
	if input.Plan == "team" {
		if input.Quantity == nil {
			quantity := int32(1)
			input.Quantity = &quantity
		}
	} else {
		input.Quantity = nil
	}
	return input, nil
}

func knownAdminGrantPlan(plan string) bool {
	switch plan {
	case "free", "pro", "lifetime_pro", "ai", "team", "selfhost_free", "selfhost_team":
		return true
	default:
		return false
	}
}

func adminGrantIdentity(input AdminGrantInput) (AdminGrantChange, string) {
	return AdminGrantChange{TargetType: input.TargetType, TargetID: input.TargetID},
		"admin_grant:" + input.TargetType + ":" + input.TargetID
}

func lockAdminGrantSource(ctx context.Context, tx pgx.Tx, subject string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,4817))`, "admin_grant:"+subject); err != nil {
		return fmt.Errorf("lock admin entitlement grant source: %w", err)
	}
	return nil
}

func lockAdminGrantTarget(ctx context.Context, tx pgx.Tx, target AdminGrantChange) ([]string, error) {
	if target.TargetType == "user" {
		var id string
		if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, target.TargetID).Scan(&id); errors.Is(err, pgx.ErrNoRows) {
			return nil, &AdminGrantError{Reason: "entitlement target user not found"}
		} else if err != nil {
			return nil, fmt.Errorf("lock admin entitlement user target: %w", err)
		}
		return nil, nil
	}
	owners, err := readActiveWorkspaceOwners(ctx, tx, target.TargetID)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT id FROM users WHERE id=ANY($1::text[]) ORDER BY id FOR UPDATE`, owners)
	if err != nil {
		return nil, fmt.Errorf("lock admin entitlement workspace owners: %w", err)
	}
	lockedOwners := make([]string, 0, len(owners))
	for rows.Next() {
		var ownerID string
		if err := rows.Scan(&ownerID); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan locked admin entitlement workspace owner: %w", err)
		}
		lockedOwners = append(lockedOwners, ownerID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("read locked admin entitlement workspace owners: %w", err)
	}
	rows.Close()
	if !equalAdminGrantStrings(owners, lockedOwners) {
		return nil, &AdminGrantError{Reason: "entitlement target user not found"}
	}
	var workspaceID string
	if err := tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, target.TargetID).Scan(&workspaceID); errors.Is(err, pgx.ErrNoRows) {
		return nil, &AdminGrantError{Reason: "entitlement target workspace not found"}
	} else if err != nil {
		return nil, fmt.Errorf("lock admin entitlement workspace target: %w", err)
	}
	lockedOwners, err = readActiveWorkspaceOwners(ctx, tx, target.TargetID)
	if err != nil {
		return nil, err
	}
	if !equalAdminGrantStrings(owners, lockedOwners) {
		return nil, &AdminGrantError{Reason: "entitlement_target_changed"}
	}
	if len(owners) == 0 {
		return nil, &AdminGrantError{Reason: "entitlement target workspace has no active owner"}
	}
	return owners, nil
}

func readActiveWorkspaceOwners(ctx context.Context, tx pgx.Tx, workspaceID string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT user_id FROM workspace_members
		WHERE workspace_id=$1 AND role='owner' AND state='active' ORDER BY workspace_id,user_id`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("read admin entitlement workspace owners: %w", err)
	}
	defer rows.Close()
	owners := make([]string, 0, 1)
	for rows.Next() {
		var owner string
		if err := rows.Scan(&owner); err != nil {
			return nil, fmt.Errorf("scan admin entitlement workspace owner: %w", err)
		}
		owners = append(owners, owner)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read admin entitlement workspace owners: %w", err)
	}
	return owners, nil
}

func hasCommercialWorkspaceGrant(ctx context.Context, tx pgx.Tx, workspaceID string, now time.Time) (bool, error) {
	var exists bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM entitlements WHERE target_type='workspace' AND target_id=$1 AND plan='team'
		AND source IN ('cloud_subscription','admin_grant')
		AND (starts_at IS NULL OR starts_at <= $2)
		AND ((status='active' AND (expires_at IS NULL OR expires_at > $2))
		  OR (status='grace' AND grace_until > $2))
	)`, workspaceID, now).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("read current workspace commercial entitlement: %w", err)
	}
	return exists, nil
}

func equalAdminGrantStrings(a, b []string) bool {
	left := append([]string(nil), a...)
	right := append([]string(nil), b...)
	sort.Strings(left)
	sort.Strings(right)
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
