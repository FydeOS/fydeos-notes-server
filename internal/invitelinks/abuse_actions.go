// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package invitelinks

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type claimedInviteAbuseAction struct {
	id          int64
	action      string
	subjectKey  string
	actorID     string
	workspaceID string
	workerID    string
}

func (a *WorkspaceInviteQuotaAdmission) executeInviteAbuseAction(ctx context.Context, required inviteAbuseActionRequired, actorID, workspaceID, workerID string) error {
	claimed, ok, err := a.claimInviteAbuseAction(ctx, required.ActionID, workerID)
	if err != nil || !ok {
		return err
	}
	claimed.actorID = actorID
	claimed.workspaceID = workspaceID
	return a.applyInviteAbuseAction(ctx, claimed)
}

func (a *WorkspaceInviteQuotaAdmission) RunInviteAbuseWorker(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		a.processRetryableInviteAbuseActions(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *WorkspaceInviteQuotaAdmission) processRetryableInviteAbuseActions(ctx context.Context) {
	workerID := "go-worker:" + a.inlineWorker
	actions, err := a.claimRetryableInviteAbuseActions(ctx, workerID, 20)
	if err != nil {
		slog.ErrorContext(ctx, "claim retryable workspace invite abuse actions", "error", err)
		return
	}
	for _, action := range actions {
		if err := a.applyInviteAbuseAction(ctx, action); err != nil {
			slog.ErrorContext(ctx, "retry workspace invite abuse action", "action", action.action, "action_id", action.id, "workspace_id", action.workspaceID, "actor_id", action.actorID, "error", err)
		}
	}
}

func (a *WorkspaceInviteQuotaAdmission) claimInviteAbuseAction(ctx context.Context, actionID int64, workerID string) (claimedInviteAbuseAction, bool, error) {
	tx, err := a.db.Begin(ctx)
	if err != nil {
		return claimedInviteAbuseAction{}, false, fmt.Errorf("begin invite abuse action claim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	row, actionStatus, err := loadInviteAbuseAction(ctx, tx, actionID, true)
	if errors.Is(err, pgx.ErrNoRows) {
		return claimedInviteAbuseAction{}, false, nil
	}
	if err != nil {
		return claimedInviteAbuseAction{}, false, fmt.Errorf("load invite abuse action: %w", err)
	}
	if !validateAbuseRow(row.abuse) || row.queuedAction != row.abuse.action.String {
		if _, err := tx.Exec(ctx, `UPDATE runtime_invite_abuse_actions SET status='failed',last_error='invalid persisted invite abuse action',locked_by=NULL,locked_until=NULL,next_attempt_at=NULL,updated_at=clock_timestamp() WHERE id=$1`, actionID); err != nil {
			return claimedInviteAbuseAction{}, false, fmt.Errorf("reject invalid invite abuse action: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return claimedInviteAbuseAction{}, false, fmt.Errorf("commit rejected invite abuse action: %w", err)
		}
		return claimedInviteAbuseAction{}, false, nil
	}
	if !inviteAbuseActionReady(actionStatus, row.lockedUntil, time.Now()) {
		return claimedInviteAbuseAction{}, false, nil
	}
	result, err := tx.Exec(ctx, `UPDATE runtime_invite_abuse_actions SET status='running',attempts=attempts+1,
		locked_by=$2,locked_until=clock_timestamp()+interval '5 minutes',last_error=NULL,updated_at=clock_timestamp()
		WHERE id=$1 AND ((status IN ('pending','retry_wait') AND (next_attempt_at IS NULL OR next_attempt_at<=clock_timestamp()))
		OR (status='running' AND locked_until<=clock_timestamp()))`, actionID, workerID)
	if err != nil {
		return claimedInviteAbuseAction{}, false, fmt.Errorf("claim invite abuse action: %w", err)
	}
	if result.RowsAffected() == 0 {
		return claimedInviteAbuseAction{}, false, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return claimedInviteAbuseAction{}, false, fmt.Errorf("commit invite abuse action claim: %w", err)
	}
	return claimedInviteAbuseAction{id: actionID, action: row.queuedAction, subjectKey: row.abuse.key,
		actorID: row.abuse.evidenceUserID.String, workspaceID: row.abuse.evidenceWorkspaceID.String, workerID: workerID}, true, nil
}

func (a *WorkspaceInviteQuotaAdmission) claimRetryableInviteAbuseActions(ctx context.Context, workerID string, limit int) ([]claimedInviteAbuseAction, error) {
	if limit < 1 || limit > 1000 {
		return nil, errors.New("invalid invite abuse action claim limit")
	}
	tx, err := a.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin retryable invite abuse action claim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `SELECT id FROM runtime_invite_abuse_actions WHERE
		(status IN ('pending','retry_wait') AND (next_attempt_at IS NULL OR next_attempt_at<=clock_timestamp())) OR
		(status='running' AND locked_until<=clock_timestamp())
		ORDER BY COALESCE(next_attempt_at,created_at),id LIMIT $1 FOR UPDATE SKIP LOCKED`, limit*4)
	if err != nil {
		return nil, fmt.Errorf("select retryable invite abuse actions: %w", err)
	}
	ids := make([]int64, 0, limit*4)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan retryable invite abuse action: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("read retryable invite abuse actions: %w", err)
	}
	rows.Close()
	claimed := make([]claimedInviteAbuseAction, 0, limit)
	for _, id := range ids {
		if len(claimed) == limit {
			break
		}
		row, _, err := loadInviteAbuseAction(ctx, tx, id, false)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("load retryable invite abuse action: %w", err)
		}
		if !validateAbuseRow(row.abuse) || row.queuedAction != row.abuse.action.String {
			if _, err := tx.Exec(ctx, `UPDATE runtime_invite_abuse_actions SET status='failed',last_error='invalid persisted invite abuse action',locked_by=NULL,locked_until=NULL,next_attempt_at=NULL,updated_at=clock_timestamp() WHERE id=$1`, id); err != nil {
				return nil, fmt.Errorf("reject invalid retryable invite abuse action: %w", err)
			}
			continue
		}
		result, err := tx.Exec(ctx, `UPDATE runtime_invite_abuse_actions SET status='running',attempts=attempts+1,
			locked_by=$2,locked_until=clock_timestamp()+interval '5 minutes',last_error=NULL,updated_at=clock_timestamp() WHERE id=$1`, id, workerID)
		if err != nil {
			return nil, fmt.Errorf("claim retryable invite abuse action: %w", err)
		}
		if result.RowsAffected() == 1 {
			claimed = append(claimed, claimedInviteAbuseAction{id: id, action: row.queuedAction, subjectKey: row.abuse.key,
				actorID: row.abuse.evidenceUserID.String, workspaceID: row.abuse.evidenceWorkspaceID.String, workerID: workerID})
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit retryable invite abuse action claims: %w", err)
	}
	return claimed, nil
}

type inviteAbuseActionRow struct {
	queuedAction string
	status       string
	lockedUntil  *time.Time
	abuse        abuseRow
}

func loadInviteAbuseAction(ctx context.Context, tx pgx.Tx, id int64, lock bool) (inviteAbuseActionRow, string, error) {
	query := `SELECT action.action,action.status,action.locked_until,
		subject.subject_key,subject.kind,subject.status,subject.action,subject.action_reason,subject.user_id,subject.actor_email_hash,subject.email_domain,
		evidence.subject_key,evidence.workspace_id,evidence.user_id,evidence.actor_email_hash,evidence.source_prefix_hash,evidence.source_asn,
		evidence.target_domains,evidence.counters,evidence.decision,evidence.reason
		FROM runtime_invite_abuse_actions action
		JOIN runtime_invite_abuse_subjects subject ON subject.subject_key=action.subject_key
		JOIN runtime_invite_abuse_evidence evidence ON evidence.id=action.evidence_id
		WHERE action.id=$1`
	if lock {
		query += ` FOR UPDATE OF action`
	}
	var row inviteAbuseActionRow
	if err := tx.QueryRow(ctx, query, id).Scan(&row.queuedAction, &row.status, &row.lockedUntil,
		&row.abuse.key, &row.abuse.kind, &row.abuse.status, &row.abuse.action, &row.abuse.reason, &row.abuse.userID,
		&row.abuse.actorEmailHash, &row.abuse.emailDomain, &row.abuse.evidenceSubjectKey, &row.abuse.evidenceWorkspaceID,
		&row.abuse.evidenceUserID, &row.abuse.evidenceActorEmailHash, &row.abuse.evidenceSourceHash,
		&row.abuse.evidenceSourceASN, &row.abuse.evidenceTargetDomains, &row.abuse.evidenceCounters,
		&row.abuse.evidenceAction, &row.abuse.evidenceReason); err != nil {
		return inviteAbuseActionRow{}, "", err
	}
	row.abuse.actionAction = pgtype.Text{String: row.queuedAction, Valid: true}
	return row, row.status, nil
}

func inviteAbuseActionReady(status string, lockedUntil *time.Time, now time.Time) bool {
	switch status {
	case "pending", "retry_wait":
		return true
	case "running":
		return lockedUntil != nil && !lockedUntil.After(now)
	default:
		return false
	}
}

func (a *WorkspaceInviteQuotaAdmission) applyInviteAbuseAction(ctx context.Context, action claimedInviteAbuseAction) error {
	tx, err := a.db.Begin(ctx)
	if err != nil {
		return a.retryInviteAbuseAction(ctx, action, fmt.Errorf("begin invite abuse disposition: %w", err))
	}
	defer func() { _ = tx.Rollback(ctx) }()
	fail := func(cause error) error {
		_ = tx.Rollback(ctx)
		return a.retryInviteAbuseAction(ctx, action, cause)
	}
	switch action.action {
	case "ban_actor", "quarantine_actor":
		if err := cancelInviteAbuseMail(ctx, tx, "actor_user_id=$2", action.actorID, "actor_quarantined"); err != nil {
			return fail(err)
		}
		if err := cancelInviteAbuseMail(ctx, tx, "workspace_id=$2", action.workspaceID, "workspace_quarantined"); err != nil {
			return fail(err)
		}
		if err := cancelInviteAbuseMail(ctx, tx, "abuse_subject_key=$2", action.subjectKey, "abuse_subject_quarantined"); err != nil {
			return fail(err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM workspace_invitations WHERE inviter_user_id=$1 AND status=ANY($2)`, action.actorID, []string{"pending", "waiting_review", "waiting_seat"}); err != nil {
			return fail(fmt.Errorf("cancel actor pending invitations: %w", err))
		}
		if action.action == "ban_actor" {
			if _, err := tx.Exec(ctx, `UPDATE users SET disabled=true,auth_epoch=auth_epoch+1 WHERE id=$1`, action.actorID); err != nil {
				return fail(fmt.Errorf("disable banned invite actor: %w", err))
			}
		}
		reason := "abuse_quarantine"
		if action.action == "ban_actor" {
			reason = "abuse_ban"
		}
		if err := revokeInviteActorSessions(ctx, tx, action.actorID, reason); err != nil {
			return fail(err)
		}
	case "quarantine_workspace":
		if err := cancelInviteAbuseMail(ctx, tx, "workspace_id=$2", action.workspaceID, "workspace_quarantined"); err != nil {
			return fail(err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM workspace_invitations WHERE workspace_id=$1 AND status=ANY($2)`, action.workspaceID, []string{"pending", "waiting_review", "waiting_seat"}); err != nil {
			return fail(fmt.Errorf("cancel workspace pending invitations: %w", err))
		}
	case "quarantine_source_cohort":
		if err := cancelInviteAbuseMail(ctx, tx, "abuse_subject_key=$2", action.subjectKey, "source_cohort_quarantined"); err != nil {
			return fail(err)
		}
	default:
		return fail(fmt.Errorf("unknown invite abuse action %q", action.action))
	}
	result, err := tx.Exec(ctx, `UPDATE runtime_invite_abuse_actions SET status='succeeded',next_attempt_at=NULL,locked_by=NULL,locked_until=NULL,last_error=NULL,updated_at=clock_timestamp() WHERE id=$1 AND status='running' AND locked_by=$2`, action.id, action.workerID)
	if err != nil {
		return fail(fmt.Errorf("mark invite abuse action succeeded: %w", err))
	}
	if result.RowsAffected() != 1 {
		return fail(errors.New("invite abuse action claim was lost"))
	}
	if err := tx.Commit(ctx); err != nil {
		return a.retryInviteAbuseAction(ctx, action, fmt.Errorf("commit invite abuse disposition: %w", err))
	}
	return nil
}

func cancelInviteAbuseMail(ctx context.Context, tx pgx.Tx, condition, value, reason string) error {
	query := `UPDATE mail_deliveries SET status='canceled',canceled_at=clock_timestamp(),settled_at=clock_timestamp(),
		locked_by=NULL,locked_until=NULL,recipient_email=NULL,payload=NULL,retention_state='anonymized',
		anonymized_at=clock_timestamp(),last_error_code=$1,updated_at=clock_timestamp()
		WHERE status IN ('queued','retry_wait','sending') AND ` + condition
	if _, err := tx.Exec(ctx, query, reason, value); err != nil {
		return fmt.Errorf("cancel invite-related mail deliveries: %w", err)
	}
	return nil
}

func revokeInviteActorSessions(ctx context.Context, tx pgx.Tx, actorID, reason string) error {
	rows, err := tx.Query(ctx, `SELECT id FROM user_sessions WHERE user_id=$1 ORDER BY id FOR UPDATE`, actorID)
	if err != nil {
		return fmt.Errorf("lock invite actor sessions: %w", err)
	}
	var userSessionIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("scan invite actor session: %w", err)
		}
		userSessionIDs = append(userSessionIDs, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("read invite actor sessions: %w", err)
	}
	rows.Close()
	if len(userSessionIDs) == 0 {
		return nil
	}
	rows, err = tx.Query(ctx, `SELECT id FROM auth_sessions WHERE user_session_id=ANY($1) ORDER BY id FOR UPDATE`, userSessionIDs)
	if err != nil {
		return fmt.Errorf("lock invite actor auth sessions: %w", err)
	}
	var authSessionIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("scan invite actor auth session: %w", err)
		}
		authSessionIDs = append(authSessionIDs, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("read invite actor auth sessions: %w", err)
	}
	rows.Close()
	if len(authSessionIDs) > 0 {
		if _, err := tx.Exec(ctx, `UPDATE auth_refresh_tokens SET revoked_at=clock_timestamp() WHERE auth_session_id=ANY($1) AND revoked_at IS NULL`, authSessionIDs); err != nil {
			return fmt.Errorf("revoke invite actor refresh tokens: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE auth_sessions SET revoked_at=clock_timestamp(),revoke_reason=$2 WHERE id=ANY($1) AND revoked_at IS NULL`, authSessionIDs, reason); err != nil {
			return fmt.Errorf("revoke invite actor auth sessions: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM user_sessions WHERE id=ANY($1)`, userSessionIDs); err != nil {
		return fmt.Errorf("delete invite actor sessions: %w", err)
	}
	return nil
}

func (a *WorkspaceInviteQuotaAdmission) retryInviteAbuseAction(ctx context.Context, action claimedInviteAbuseAction, cause error) error {
	_ = a.db.QueryRow(ctx, `UPDATE runtime_invite_abuse_actions SET
		status=CASE WHEN attempts>=5 THEN 'failed' ELSE 'retry_wait' END,
		next_attempt_at=CASE WHEN attempts>=5 THEN next_attempt_at ELSE clock_timestamp()+interval '5 minutes' END,
		locked_by=NULL,locked_until=NULL,last_error=$3,updated_at=clock_timestamp()
		WHERE id=$1 AND status='running' AND locked_by=$2 RETURNING id`, action.id, action.workerID, cause.Error()).Scan(&action.id)
	return cause
}
