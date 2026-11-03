// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package invitelinks

import (
	"context"
	"fmt"
)

type MemberLeaveMailPolicy struct {
	db Database
}

func NewMemberLeaveMailPolicy(db Database) *MemberLeaveMailPolicy {
	return &MemberLeaveMailPolicy{db: db}
}

func (p *MemberLeaveMailPolicy) MemberLeaveMailSkipReason(ctx context.Context, actorID, workspaceID string) (string, error) {
	return p.mailSkipReason(ctx, actorID, workspaceID)
}

func (p *MemberLeaveMailPolicy) InvitationReviewMailSkipReason(ctx context.Context, actorID, workspaceID string) (string, error) {
	return p.mailSkipReason(ctx, actorID, workspaceID)
}

func (p *MemberLeaveMailPolicy) mailSkipReason(ctx context.Context, actorID, workspaceID string) (string, error) {
	if p == nil || p.db == nil || actorID == "" || workspaceID == "" {
		return "", fmt.Errorf("workspace mail policy is unavailable")
	}
	tx, err := p.db.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin workspace mail policy snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	abuse, err := loadInviteAbuseFacts(ctx, tx)
	if err != nil {
		return "", err
	}
	if !abuse.valid {
		return "policy_state_invalid", nil
	}
	var actorQuarantined bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM runtime_invite_abuse_subjects
		WHERE user_id=$1 AND status IN ('quarantined','banned')
	)`, actorID).Scan(&actorQuarantined); err != nil {
		return "", fmt.Errorf("check workspace mail actor quarantine: %w", err)
	}
	if actorQuarantined {
		if err := tx.Commit(ctx); err != nil {
			return "", fmt.Errorf("commit workspace mail policy snapshot: %w", err)
		}
		return "actor_quarantined", nil
	}
	var workspaceQuarantined bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1
		FROM runtime_invite_abuse_subjects subject
		JOIN runtime_invite_abuse_evidence evidence ON evidence.subject_key=subject.subject_key
		WHERE evidence.workspace_id=$1 AND subject.kind='workspace' AND subject.status='quarantined'
	)`, workspaceID).Scan(&workspaceQuarantined); err != nil {
		return "", fmt.Errorf("check workspace mail workspace quarantine: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit workspace mail policy snapshot: %w", err)
	}
	if workspaceQuarantined {
		return "workspace_quarantined", nil
	}
	return "", nil
}
