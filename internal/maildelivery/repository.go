// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package maildelivery

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/invitelinks"
	"github.com/fydeos/fydeos-notes-server/internal/mailquota"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

type Database interface {
	Begin(context.Context) (pgx.Tx, error)
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

type RecipientHashKeyProvider func(context.Context) (string, error)

type MemberLeaveMailPolicy interface {
	MemberLeaveMailSkipReason(context.Context, string, string) (string, error)
}

type InvitationReviewMailPolicy interface {
	InvitationReviewMailSkipReason(context.Context, string, string) (string, error)
}

type WorkspaceMailPolicy interface {
	MemberLeaveMailPolicy
	InvitationReviewMailPolicy
}

type Repository struct {
	db                  Database
	config              config.Config
	recipientHashKey    RecipientHashKeyProvider
	workspaceMailPolicy WorkspaceMailPolicy
}

type AdminAggregate struct {
	Bucket    time.Time
	MailName  string
	MailClass string
	Status    string
	Count     int
}

func NewRepository(db Database, cfg config.Config, recipientHashKey RecipientHashKeyProvider, workspaceMailPolicy WorkspaceMailPolicy) *Repository {
	return &Repository{db: db, config: cfg, recipientHashKey: recipientHashKey, workspaceMailPolicy: workspaceMailPolicy}
}

func (r *Repository) AdminAggregate(ctx context.Context, from, to time.Time, bucket string) ([]AdminAggregate, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("mail delivery repository is unavailable")
	}
	if bucket != "hour" && bucket != "day" {
		return nil, errors.New("unsupported mail delivery analytics bucket")
	}
	rows, err := r.db.Query(ctx, `WITH events AS (
		SELECT date_trunc($1, created_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' AS bucket,
			mail_name, mail_class, status, COUNT(*)::int AS count
		FROM mail_deliveries
		WHERE created_at >= $2 AND created_at < $3
		GROUP BY bucket, mail_name, mail_class, status
	)
	SELECT bucket, mail_name, mail_class, status, count
	FROM events
	ORDER BY bucket ASC, count DESC, mail_name ASC`, bucket, from, to)
	if err != nil {
		return nil, fmt.Errorf("aggregate mail deliveries: %w", err)
	}
	defer rows.Close()

	var result []AdminAggregate
	for rows.Next() {
		var item AdminAggregate
		if err := rows.Scan(&item.Bucket, &item.MailName, &item.MailClass, &item.Status, &item.Count); err != nil {
			return nil, fmt.Errorf("scan mail delivery aggregate: %w", err)
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read mail delivery aggregates: %w", err)
	}
	return result, nil
}

func (r *Repository) QueueMemberRemoved(ctx context.Context, userID, workspaceID string) error {
	return r.queueWorkspaceLifecycle(ctx, "MemberRemoved", userID, "", workspaceID, map[string]any{
		"workspace": map[string]string{"$$workspaceId": workspaceID},
	})
}

func (r *Repository) CancelByRecipient(ctx context.Context, recipientEmail string) (int64, error) {
	if r == nil || r.db == nil || r.recipientHashKey == nil {
		return 0, errors.New("mail delivery cancellation is unavailable")
	}
	privateKey, err := r.recipientHashKey(ctx)
	if err != nil {
		return 0, fmt.Errorf("load mail recipient hash key: %w", err)
	}
	if strings.TrimSpace(privateKey) == "" {
		return 0, errors.New("mail recipient hash key is not configured")
	}
	hashKey := sha256.Sum256([]byte(privateKey))
	hash := hmac.New(sha256.New, hashKey[:])
	_, _ = hash.Write([]byte(strings.ToLower(strings.TrimSpace(recipientEmail))))
	return r.cancelWhere(ctx, `recipient_hash=$1`, hex.EncodeToString(hash.Sum(nil)), "recipient_deleted")
}

func (r *Repository) CancelMemberInvitationByActor(ctx context.Context, actorUserID string) (int64, error) {
	if r == nil || r.db == nil || actorUserID == "" {
		return 0, errors.New("mail delivery cancellation is unavailable")
	}
	return r.cancelWhere(ctx, `mail_name='MemberInvitation' AND actor_user_id=$1`, actorUserID, "actor_deleted")
}

func (r *Repository) cancelWhere(ctx context.Context, predicate string, argument any, reason string) (int64, error) {
	result, err := r.db.Exec(ctx, `UPDATE mail_deliveries
		SET status='canceled',canceled_at=now(),settled_at=now(),locked_by=NULL,locked_until=NULL,
		    recipient_email=NULL,payload=NULL,retention_state='anonymized',anonymized_at=now(),
		    last_error_code=$2,updated_at=now()
		WHERE status IN ('queued','retry_wait','sending') AND `+predicate, argument, reason)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected(), nil
}

func (r *Repository) QueueMemberLeave(ctx context.Context, workspaceID, userID string) error {
	if r == nil || r.db == nil || userID == "" || workspaceID == "" {
		return errors.New("mail delivery repository and target are required")
	}
	var ownerID string
	err := r.db.QueryRow(ctx, `SELECT user_id FROM workspace_members
		WHERE workspace_id=$1 AND role='owner' AND state='active'`, workspaceID).Scan(&ownerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return errors.New("workspace owner not found for member-leave email")
	}
	if err != nil {
		return fmt.Errorf("load member-leave recipient: %w", err)
	}
	return r.queueWorkspaceLifecycle(ctx, "MemberLeave", ownerID, userID, workspaceID, map[string]any{
		"workspace": map[string]string{"$$workspaceId": workspaceID},
		"user":      map[string]string{"$$userId": userID},
	})
}

func (r *Repository) QueueInvitationReviewApproved(ctx context.Context, inviteeID, actorID, workspaceID, notificationID string) error {
	if r == nil {
		return errors.New("mail delivery repository is required")
	}
	props := map[string]any{
		"workspace": map[string]string{"$$workspaceId": workspaceID},
		"url":       strings.TrimRight(config.PublicBaseURL(r.config), "/") + "/workspace/" + workspaceID,
	}
	return r.queueInvitationReview(ctx, "LinkInvitationApprove", inviteeID, actorID, workspaceID, notificationID, props)
}

func (r *Repository) QueueInvitationReviewRequest(ctx context.Context, reviewerID, inviteeID, workspaceID, notificationID string) error {
	if r == nil {
		return errors.New("mail delivery repository is required")
	}
	settingsURL := strings.TrimRight(config.PublicBaseURL(r.config), "/") + "/workspace/" + url.PathEscape(workspaceID) + "/settings?tab=" + url.QueryEscape("workspace:members")
	props := map[string]any{
		"workspace": map[string]string{"$$workspaceId": workspaceID},
		"user":      map[string]string{"$$userId": inviteeID},
		"url":       settingsURL,
	}
	return r.queueInvitationReview(ctx, "LinkInvitationReviewRequest", reviewerID, inviteeID, workspaceID, notificationID, props)
}

func (r *Repository) QueueInvitationReviewDeclined(ctx context.Context, inviteeID, actorID, workspaceID, notificationID string) error {
	props := map[string]any{"workspace": map[string]string{"$$workspaceId": workspaceID}}
	return r.queueInvitationReview(ctx, "LinkInvitationDecline", inviteeID, actorID, workspaceID, notificationID, props)
}

func (r *Repository) QueueMemberInvitation(ctx context.Context, recipientID, actorID, workspaceID, inviteID, notificationID string) error {
	if r == nil || r.db == nil || recipientID == "" || actorID == "" || workspaceID == "" || inviteID == "" || notificationID == "" {
		return errors.New("mail delivery repository and member invitation metadata are required")
	}
	var email string
	err := r.db.QueryRow(ctx, `SELECT email FROM users WHERE id=$1`, recipientID).Scan(&email)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load member-invitation recipient: %w", err)
	}
	normalized := strings.ToLower(strings.TrimSpace(email))
	local, domain, found := strings.Cut(normalized, "@")
	if !found || local == "" || domain == "" || strings.Contains(domain, "@") {
		return errors.New("member-invitation recipient email is invalid")
	}
	settings, err := LoadSettings(ctx, r.db, r.config, os.LookupEnv)
	if err != nil {
		return err
	}
	if r.recipientHashKey == nil {
		return errors.New("mail recipient hash key provider is not configured")
	}
	privateKey, err := r.recipientHashKey(ctx)
	if err != nil {
		return fmt.Errorf("load mail recipient hash key: %w", err)
	}
	if strings.TrimSpace(privateKey) == "" {
		return errors.New("mail recipient hash key is not configured")
	}
	hashKey := sha256.Sum256([]byte(privateKey))
	hash := hmac.New(sha256.New, hashKey[:])
	_, _ = hash.Write([]byte(normalized))
	recipientHash := hex.EncodeToString(hash.Sum(nil))
	dedupeKey := "notification:" + notificationID + ":mail:MemberInvitation"

	var workspaceName *string
	err = r.db.QueryRow(ctx, `SELECT name FROM workspaces WHERE id=$1`, workspaceID).Scan(&workspaceName)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("load member-invitation workspace: %w", err)
	}
	skipReason := ""
	if workspaceName != nil && invitelinks.ContainsURLOrDomain(*workspaceName) {
		skipReason = "workspace_name_contains_domain"
	}
	if skipReason == "" {
		var receiveInvitationEmail bool
		if err := r.db.QueryRow(ctx, `SELECT COALESCE((settings.payload->>'receiveInvitationEmail')::boolean,true)
			FROM users recipient LEFT JOIN user_settings settings ON settings.user_id=recipient.id WHERE recipient.id=$1`, recipientID).
			Scan(&receiveInvitationEmail); err != nil {
			return fmt.Errorf("load member-invitation recipient settings: %w", err)
		}
		if !receiveInvitationEmail {
			skipReason = "recipient_notification_disabled"
		}
	}
	if skipReason == "" {
		if r.workspaceMailPolicy == nil {
			return errors.New("member-invitation mail policy is not configured")
		}
		skipReason, err = r.workspaceMailPolicy.InvitationReviewMailSkipReason(ctx, actorID, workspaceID)
		if err != nil {
			return fmt.Errorf("check member-invitation mail policy: %w", err)
		}
	}
	if skipReason == "" && !mailSettingsConfiguredFor(settings, domain) {
		return nil
	}

	props := map[string]any{
		"user":      map[string]string{"$$userId": actorID},
		"workspace": map[string]string{"$$workspaceId": workspaceID},
		"url":       strings.TrimRight(config.PublicBaseURL(r.config), "/") + "/invite/" + url.PathEscape(inviteID),
	}
	payload, err := json.Marshal(map[string]any{"name": "MemberInvitation", "to": email, "props": props})
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin member-invitation mail admission: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('mail:dedupe:' || $1,0))`, dedupeKey); err != nil {
		return fmt.Errorf("lock member-invitation mail dedupe: %w", err)
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM mail_deliveries WHERE dedupe_key=$1)`, dedupeKey).Scan(&exists); err != nil {
		return fmt.Errorf("check member-invitation mail dedupe: %w", err)
	}
	if exists {
		return tx.Commit(ctx)
	}
	if skipReason != "" {
		if err := recordSkippedWorkspaceInvitation(ctx, tx, "MemberInvitation", "workspace_invitation", recipientID, actorID, workspaceID, notificationID, dedupeKey, recipientHash, domain, skipReason, nil, now); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}

	const mailClass = "workspace_invitation"
	scopes := []mailquota.Scope{
		{Key: "mail:recipient:" + mailquota.RecipientScopeHash(normalized) + ":class:" + mailClass, WindowSeconds: 3600, Limit: 20, LegacyBucketSeconds: 300},
		{Key: "mail:recipient_domain:" + domain + ":class:" + mailClass, WindowSeconds: 3600, Limit: 250, LegacyBucketSeconds: 300},
		{Key: "mail:provider_global:default", WindowSeconds: 60, Limit: 500, LegacyBucketSeconds: 10},
	}
	quotaDecision := map[string]any{"allowed": true, "mailClass": mailClass, "requested": 1}
	denial, err := mailquota.CommitDetailed(ctx, tx, scopes, now)
	if err != nil {
		return fmt.Errorf("admit member-invitation mail: %w", err)
	}
	if denial != nil {
		quotaDecision = map[string]any{
			"allowed": false, "mailClass": mailClass, "reason": "mail_class", "scopeKey": denial.ScopeKey,
			"windowSeconds": denial.WindowSeconds, "limit": denial.Limit, "current": denial.Current, "requested": denial.Requested,
		}
		if err := recordSkippedWorkspaceInvitation(ctx, tx, "MemberInvitation", mailClass, recipientID, actorID, workspaceID, notificationID, dedupeKey, recipientHash, domain, "mail_class", quotaDecision, now); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	decision, err := json.Marshal(quotaDecision)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO mail_deliveries(
		mail_name,mail_class,priority,status,dedupe_key,recipient_email,recipient_hash,recipient_domain,
		recipient_user_id,actor_user_id,workspace_id,notification_id,quota_decision,payload,send_after,max_attempts
	) VALUES('MemberInvitation',$1,'normal','queued',$2,$3,$4,$5,$6,$7,$8,$9,$10::jsonb,$11::jsonb,$12,3)
	ON CONFLICT(dedupe_key) WHERE dedupe_key IS NOT NULL DO NOTHING`,
		mailClass, dedupeKey, email, recipientHash, domain, recipientID, actorID, workspaceID, notificationID, decision, payload, now)
	if err != nil {
		return fmt.Errorf("queue member-invitation email: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit member-invitation mail: %w", err)
	}
	return nil
}

func (r *Repository) queueInvitationReview(ctx context.Context, mailName, recipientUserID, actorUserID, workspaceID, notificationID string, props map[string]any) error {
	if r == nil || r.db == nil || recipientUserID == "" || actorUserID == "" || workspaceID == "" || notificationID == "" {
		return errors.New("mail delivery repository and invitation review metadata are required")
	}
	var email string
	err := r.db.QueryRow(ctx, `SELECT email FROM users WHERE id=$1`, recipientUserID).Scan(&email)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load invitation review recipient: %w", err)
	}
	normalized := strings.ToLower(strings.TrimSpace(email))
	local, domain, found := strings.Cut(normalized, "@")
	if !found || local == "" || domain == "" || strings.Contains(domain, "@") {
		return errors.New("invitation review recipient email is invalid")
	}
	if r.recipientHashKey == nil {
		return errors.New("mail recipient hash key provider is not configured")
	}
	privateKey, err := r.recipientHashKey(ctx)
	if err != nil {
		return fmt.Errorf("load mail recipient hash key: %w", err)
	}
	if strings.TrimSpace(privateKey) == "" {
		return errors.New("mail recipient hash key is not configured")
	}
	hashKey := sha256.Sum256([]byte(privateKey))
	hash := hmac.New(sha256.New, hashKey[:])
	_, _ = hash.Write([]byte(normalized))
	recipientHash := hex.EncodeToString(hash.Sum(nil))
	dedupeKey := "notification:" + notificationID + ":mail:" + mailName

	if r.workspaceMailPolicy == nil {
		return errors.New("invitation review mail policy is not configured")
	}
	skipReason, err := r.workspaceMailPolicy.InvitationReviewMailSkipReason(ctx, actorUserID, workspaceID)
	if err != nil {
		return fmt.Errorf("check invitation review mail policy: %w", err)
	}
	settings, err := LoadSettings(ctx, r.db, r.config, os.LookupEnv)
	if err != nil {
		return err
	}
	if skipReason == "" && !mailSettingsConfiguredFor(settings, domain) {
		return nil
	}

	now := time.Now().UTC()
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin invitation review mail admission: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('mail:dedupe:' || $1,0))`, dedupeKey); err != nil {
		return fmt.Errorf("lock invitation review mail dedupe: %w", err)
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM mail_deliveries WHERE dedupe_key=$1)`, dedupeKey).Scan(&exists); err != nil {
		return fmt.Errorf("check invitation review mail dedupe: %w", err)
	}
	if exists {
		return tx.Commit(ctx)
	}
	if skipReason != "" {
		if err := recordSkippedWorkspaceInvitation(ctx, tx, mailName, "workspace_invitation", recipientUserID, actorUserID, workspaceID, notificationID, dedupeKey, recipientHash, domain, skipReason, nil, now); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}

	const mailClass = "collaboration_notice"
	scopes := []mailquota.Scope{
		{Key: "mail:recipient:" + mailquota.RecipientScopeHash(normalized) + ":class:" + mailClass, WindowSeconds: 3600, Limit: 20, LegacyBucketSeconds: 300},
		{Key: "mail:recipient_domain:" + domain + ":class:" + mailClass, WindowSeconds: 3600, Limit: 250, LegacyBucketSeconds: 300},
		{Key: "mail:provider_global:default", WindowSeconds: 60, Limit: 500, LegacyBucketSeconds: 10},
		{Key: "mail:actor:" + actorUserID + ":class:" + mailClass, WindowSeconds: 3600, Limit: 200, LegacyBucketSeconds: 300},
		{Key: "mail:workspace:" + workspaceID + ":class:" + mailClass, WindowSeconds: 3600, Limit: 1000, LegacyBucketSeconds: 300},
	}
	quotaDecision := map[string]any{"allowed": true, "mailClass": mailClass, "requested": 1}
	denial, err := mailquota.CommitDetailed(ctx, tx, scopes, now)
	if err != nil {
		return fmt.Errorf("admit invitation review mail: %w", err)
	}
	if denial != nil {
		quotaDecision = map[string]any{
			"allowed": false, "mailClass": mailClass, "reason": "mail_class", "scopeKey": denial.ScopeKey,
			"windowSeconds": denial.WindowSeconds, "limit": denial.Limit, "current": denial.Current, "requested": denial.Requested,
		}
		if err := recordSkippedWorkspaceInvitation(ctx, tx, mailName, mailClass, recipientUserID, actorUserID, workspaceID, notificationID, dedupeKey, recipientHash, domain, "mail_class", quotaDecision, now); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	decision, err := json.Marshal(quotaDecision)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]any{"name": mailName, "to": email, "props": props})
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO mail_deliveries(
		mail_name,mail_class,priority,status,dedupe_key,recipient_email,recipient_hash,recipient_domain,
		recipient_user_id,actor_user_id,workspace_id,notification_id,quota_decision,payload,send_after,max_attempts
	) VALUES($1,$2,'normal','queued',$3,$4,$5,$6,$7,$8,$9,$10,$11::jsonb,$12::jsonb,$13,3)
	ON CONFLICT(dedupe_key) WHERE dedupe_key IS NOT NULL DO NOTHING`,
		mailName, mailClass, dedupeKey, email, recipientHash, domain, recipientUserID, actorUserID, workspaceID, notificationID, decision, payload, now)
	if err != nil {
		return fmt.Errorf("queue invitation review email: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit invitation review mail: %w", err)
	}
	return nil
}

func recordSkippedWorkspaceInvitation(ctx context.Context, tx pgx.Tx, mailName, mailClass, recipientUserID, actorUserID, workspaceID, notificationID, dedupeKey, recipientHash, domain, reason string, decision map[string]any, now time.Time) error {
	var rawDecision any
	if decision != nil {
		encoded, err := json.Marshal(decision)
		if err != nil {
			return err
		}
		rawDecision = encoded
	}
	_, err := tx.Exec(ctx, `INSERT INTO mail_deliveries(
		mail_name,mail_class,priority,status,dedupe_key,recipient_hash,recipient_domain,recipient_user_id,
		actor_user_id,workspace_id,notification_id,quota_decision,send_after,max_attempts,settled_at,
		retention_state,anonymized_at,last_error_code
	) VALUES($1,$2,'normal','skipped',$3,$4,$5,$6,$7,$8,$9,$10::jsonb,$11,0,$11,'anonymized',$11,$12)
	ON CONFLICT(dedupe_key) WHERE dedupe_key IS NOT NULL DO NOTHING`,
		mailName, mailClass, dedupeKey, recipientHash, domain, recipientUserID, actorUserID, workspaceID, notificationID, rawDecision, now, reason)
	if err != nil {
		return fmt.Errorf("record skipped workspace invitation email: %w", err)
	}
	return nil
}

func mailSettingsConfiguredFor(settings Settings, recipientDomain string) bool {
	if settings.SMTP.Host != "" {
		return true
	}
	if settings.FallbackSMTP.Host == "" {
		return false
	}
	for _, domain := range settings.FallbackDomains {
		if strings.EqualFold(strings.TrimSpace(domain), recipientDomain) {
			return true
		}
	}
	return false
}

func (r *Repository) queueWorkspaceLifecycle(ctx context.Context, name, recipientUserID, actorUserID, workspaceID string, props map[string]any) error {
	if r == nil || r.db == nil || recipientUserID == "" || workspaceID == "" {
		return errors.New("mail delivery repository and target are required")
	}
	var email string
	err := r.db.QueryRow(ctx, `SELECT email FROM users WHERE id=$1`, recipientUserID).Scan(&email)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load workspace-lifecycle recipient: %w", err)
	}
	settings, err := LoadSettings(ctx, r.db, r.config, os.LookupEnv)
	if err != nil {
		return err
	}
	normalized := strings.ToLower(strings.TrimSpace(email))
	local, domain, found := strings.Cut(normalized, "@")
	if !found || local == "" || domain == "" || strings.Contains(domain, "@") {
		return errors.New("workspace-lifecycle recipient email is invalid")
	}
	if !mailSettingsConfiguredFor(settings, domain) {
		return nil
	}
	if name == "MemberLeave" {
		if r.workspaceMailPolicy == nil {
			return errors.New("member-leave mail policy is not configured")
		}
		reason, err := r.workspaceMailPolicy.MemberLeaveMailSkipReason(ctx, actorUserID, workspaceID)
		if err != nil {
			return fmt.Errorf("check member-leave mail policy: %w", err)
		}
		if reason != "" {
			return r.recordSkippedWorkspaceLifecycle(ctx, name, recipientUserID, actorUserID, workspaceID, normalized, domain, reason)
		}
	}
	if r.recipientHashKey == nil {
		return errors.New("mail recipient hash key provider is not configured")
	}
	privateKey, err := r.recipientHashKey(ctx)
	if err != nil {
		return fmt.Errorf("load mail recipient hash key: %w", err)
	}
	if strings.TrimSpace(privateKey) == "" {
		return errors.New("mail recipient hash key is not configured")
	}
	hashKey := sha256.Sum256([]byte(privateKey))
	hash := hmac.New(sha256.New, hashKey[:])
	_, _ = hash.Write([]byte(normalized))
	payload, err := json.Marshal(map[string]any{"name": name, "to": email, "props": props})
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin workspace-lifecycle mail admission: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	scopes := []mailquota.Scope{
		{Key: "mail:recipient:" + mailquota.RecipientScopeHash(normalized) + ":class:workspace_lifecycle", WindowSeconds: 3600, Limit: 20, LegacyBucketSeconds: 300},
		{Key: "mail:recipient_domain:" + domain + ":class:workspace_lifecycle", WindowSeconds: 3600, Limit: 250, LegacyBucketSeconds: 300},
		{Key: "mail:provider_global:default", WindowSeconds: 60, Limit: 500, LegacyBucketSeconds: 10},
		{Key: "mail:workspace:" + workspaceID + ":class:workspace_lifecycle", WindowSeconds: 3600, Limit: 1000, LegacyBucketSeconds: 300},
	}
	quotaDecision := map[string]any{"allowed": true, "mailClass": "workspace_lifecycle", "requested": 1}
	actorID := nullableString(actorUserID)
	if err := mailquota.Commit(ctx, tx, scopes, now); errors.Is(err, mailquota.ErrDenied) {
		quotaDecision["allowed"] = false
		quotaDecision["reason"] = "quota_exceeded"
		decision, marshalErr := json.Marshal(quotaDecision)
		if marshalErr != nil {
			return marshalErr
		}
		_, err = tx.Exec(ctx, `INSERT INTO mail_deliveries(
			mail_name,mail_class,priority,status,recipient_hash,recipient_domain,recipient_user_id,actor_user_id,workspace_id,
			quota_decision,send_after,max_attempts,settled_at,retention_state,anonymized_at,last_error_code
		) VALUES($1,'workspace_lifecycle','normal','skipped',$2,$3,$4,$5,$6,$7::jsonb,$8,3,$8,'anonymized',$8,'quota_denied')`,
			name, hex.EncodeToString(hash.Sum(nil)), domain, recipientUserID, actorID, workspaceID, decision, now)
		if err != nil {
			return fmt.Errorf("record quota-denied workspace-lifecycle email: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit quota-denied workspace-lifecycle email: %w", err)
		}
		return nil
	} else if err != nil {
		return fmt.Errorf("admit workspace-lifecycle mail: %w", err)
	}
	decision, err := json.Marshal(quotaDecision)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO mail_deliveries(
		mail_name,mail_class,priority,status,recipient_email,recipient_hash,recipient_domain,
		recipient_user_id,actor_user_id,workspace_id,quota_decision,payload,send_after,max_attempts
	) VALUES($1,'workspace_lifecycle','normal','queued',$2,$3,$4,$5,$6,$7,$8::jsonb,$9::jsonb,$10,3)`,
		name, email, hex.EncodeToString(hash.Sum(nil)), domain, recipientUserID, actorID, workspaceID, decision, payload, now)
	if err != nil {
		return fmt.Errorf("queue workspace-lifecycle email: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit workspace-lifecycle mail: %w", err)
	}
	return nil
}

func (r *Repository) recordSkippedWorkspaceLifecycle(ctx context.Context, name, recipientUserID, actorUserID, workspaceID, normalized, domain, reason string) error {
	if r.recipientHashKey == nil {
		return errors.New("mail recipient hash key provider is not configured")
	}
	privateKey, err := r.recipientHashKey(ctx)
	if err != nil {
		return fmt.Errorf("load mail recipient hash key: %w", err)
	}
	if strings.TrimSpace(privateKey) == "" {
		return errors.New("mail recipient hash key is not configured")
	}
	hashKey := sha256.Sum256([]byte(privateKey))
	hash := hmac.New(sha256.New, hashKey[:])
	_, _ = hash.Write([]byte(normalized))
	decision, err := json.Marshal(map[string]any{
		"allowed": false, "mailClass": "workspace_lifecycle", "requested": 1, "reason": reason,
	})
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin skipped workspace-lifecycle mail record: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `INSERT INTO mail_deliveries(
		mail_name,mail_class,priority,status,recipient_hash,recipient_domain,recipient_user_id,actor_user_id,workspace_id,
		quota_decision,send_after,max_attempts,settled_at,retention_state,anonymized_at,last_error_code
	) VALUES($1,'workspace_lifecycle','normal','skipped',$2,$3,$4,$5,$6,$7::jsonb,$8,3,$8,'anonymized',$8,$9)`,
		name, hex.EncodeToString(hash.Sum(nil)), domain, recipientUserID, nullableString(actorUserID), workspaceID, decision, now, reason)
	if err != nil {
		return fmt.Errorf("record policy-skipped workspace-lifecycle email: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit skipped workspace-lifecycle mail record: %w", err)
	}
	return nil
}

func (r *Repository) WorkspaceName(ctx context.Context, workspaceID string) (string, bool, error) {
	var name *string
	err := r.db.QueryRow(ctx, `SELECT name FROM workspaces WHERE id=$1`, workspaceID).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if name == nil || strings.TrimSpace(*name) == "" {
		return "Untitled Workspace", true, nil
	}
	return *name, true, nil
}

func (r *Repository) UserEmail(ctx context.Context, userID string) (string, bool, error) {
	var email string
	err := r.db.QueryRow(ctx, `SELECT email FROM users WHERE id=$1`, userID).Scan(&email)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return email, err == nil, err
}

func (r *Repository) UserDisplayName(ctx context.Context, userID string) (string, bool, error) {
	var name *string
	err := r.db.QueryRow(ctx, `SELECT name FROM users WHERE id=$1`, userID).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if name == nil || strings.TrimSpace(*name) == "" {
		return "FydeOS Notes user", true, nil
	}
	return *name, true, nil
}

func (r *Repository) RecipientDisabled(ctx context.Context, email string) (bool, error) {
	var disabled bool
	err := r.db.QueryRow(ctx, `SELECT disabled FROM users WHERE lower(email)=lower($1) ORDER BY id LIMIT 1`, email).Scan(&disabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return disabled, err
}

type Delivery struct {
	ID           string
	MailName     string
	Recipient    *string
	RecipientID  *string
	Payload      []byte
	ExpiresAt    *time.Time
	AttemptCount int
	MaxAttempts  int
}

type Attempt struct {
	Count     int
	Max       int
	ExpiresAt *time.Time
}

func (r *Repository) ClaimReady(ctx context.Context, workerID string, batchSize int, lease time.Duration) ([]Delivery, error) {
	if workerID == "" || batchSize < 1 || batchSize > 1000 || lease < time.Second {
		return nil, errors.New("invalid mail delivery claim options")
	}
	_, err := r.db.Exec(ctx, `WITH expired AS (
		SELECT id FROM mail_deliveries
		WHERE (status IN ('queued','retry_wait') OR (status='sending' AND locked_until<now()))
		  AND expires_at IS NOT NULL AND expires_at<=now()
		ORDER BY expires_at LIMIT $1 FOR UPDATE SKIP LOCKED
	) UPDATE mail_deliveries delivery SET status='failed',failed_at=now(),settled_at=now(),locked_by=NULL,locked_until=NULL,
		recipient_email=NULL,payload=NULL,retention_state='anonymized',anonymized_at=now(),last_error_code='expired',updated_at=now()
	FROM expired WHERE delivery.id=expired.id`, batchSize)
	if err != nil {
		return nil, fmt.Errorf("mark expired mail deliveries: %w", err)
	}
	_, err = r.db.Exec(ctx, `WITH exhausted AS (
		SELECT id FROM mail_deliveries
		WHERE status='sending' AND locked_until<now() AND attempt_count>=max_attempts
		  AND (expires_at IS NULL OR expires_at>now())
		ORDER BY locked_until LIMIT $1 FOR UPDATE SKIP LOCKED
	) UPDATE mail_deliveries delivery SET status='failed',failed_at=now(),settled_at=now(),locked_by=NULL,locked_until=NULL,
		recipient_email=NULL,payload=NULL,retention_state='anonymized',anonymized_at=now(),
		last_error_code='delivery_outcome_unknown',
		provider_response='worker lease expired after final SMTP attempt; delivery outcome is unknown',updated_at=now()
	FROM exhausted WHERE delivery.id=exhausted.id`, batchSize)
	if err != nil {
		return nil, fmt.Errorf("settle exhausted mail delivery leases: %w", err)
	}
	rows, err := r.db.Query(ctx, `UPDATE mail_deliveries SET status='sending',locked_by=$1,
		locked_until=now()+($3::bigint * interval '1 millisecond'),updated_at=now()
	WHERE id IN (
		SELECT id FROM mail_deliveries
		WHERE (status IN ('queued','retry_wait') OR (status='sending' AND locked_until<now()))
		  AND send_after<=now() AND (expires_at IS NULL OR expires_at>now())
		  AND (locked_until IS NULL OR locked_until<now()) AND max_attempts>attempt_count
		ORDER BY CASE priority WHEN 'critical' THEN 0 WHEN 'high' THEN 1 WHEN 'normal' THEN 2 ELSE 3 END,
			send_after,created_at LIMIT $2 FOR UPDATE SKIP LOCKED
	) RETURNING id::text,mail_name,recipient_email,recipient_user_id,payload,expires_at,attempt_count,max_attempts`, workerID, batchSize, lease.Milliseconds())
	if err != nil {
		return nil, fmt.Errorf("claim ready mail deliveries: %w", err)
	}
	defer rows.Close()
	deliveries := make([]Delivery, 0)
	for rows.Next() {
		var item Delivery
		var recipient, recipientID pgtype.Text
		var payload []byte
		var expiresAt pgtype.Timestamptz
		if err := rows.Scan(&item.ID, &item.MailName, &recipient, &recipientID, &payload, &expiresAt, &item.AttemptCount, &item.MaxAttempts); err != nil {
			return nil, err
		}
		if recipient.Valid {
			value := recipient.String
			item.Recipient = &value
		}
		if recipientID.Valid {
			value := recipientID.String
			item.RecipientID = &value
		}
		item.Payload = payload
		if expiresAt.Valid {
			value := expiresAt.Time
			item.ExpiresAt = &value
		}
		deliveries = append(deliveries, item)
	}
	return deliveries, rows.Err()
}

func (r *Repository) StartAttempt(ctx context.Context, deliveryID, workerID string) (Attempt, bool, error) {
	var attempt Attempt
	var expiresAt pgtype.Timestamptz
	err := r.db.QueryRow(ctx, `UPDATE mail_deliveries SET attempt_count=attempt_count+1,
		first_attempt_at=COALESCE(first_attempt_at,now()),last_attempt_at=now(),updated_at=now()
	WHERE id=$1::uuid AND status='sending' AND locked_by=$2 AND (expires_at IS NULL OR expires_at>now())
	AND max_attempts>attempt_count RETURNING attempt_count,max_attempts,expires_at`, deliveryID, workerID).
		Scan(&attempt.Count, &attempt.Max, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Attempt{}, false, nil
	}
	if err != nil {
		return Attempt{}, false, err
	}
	if expiresAt.Valid {
		value := expiresAt.Time
		attempt.ExpiresAt = &value
	}
	return attempt, true, nil
}

type DeliveryResult struct {
	ProviderMessageID string
	ProviderResponse  string
	ErrorCode         string
	Error             string
}

func (r *Repository) MarkSent(ctx context.Context, deliveryID, workerID string, result DeliveryResult) error {
	return r.markTerminal(ctx, deliveryID, workerID, "sent", result)
}

func (r *Repository) MarkSkipped(ctx context.Context, deliveryID, workerID, reason string) error {
	return r.markTerminal(ctx, deliveryID, workerID, "skipped", DeliveryResult{ErrorCode: reason})
}

func (r *Repository) MarkFailed(ctx context.Context, deliveryID, workerID, reason, detail string) error {
	return r.markTerminal(ctx, deliveryID, workerID, "failed", DeliveryResult{ErrorCode: reason, Error: redact(detail)})
}

func (r *Repository) MarkRetry(ctx context.Context, deliveryID, workerID string, sendAfter time.Time, result DeliveryResult) (bool, error) {
	var id string
	err := r.db.QueryRow(ctx, `UPDATE mail_deliveries SET status='retry_wait',send_after=$3,locked_by=NULL,locked_until=NULL,
		last_error_code=$4,last_error=$5,updated_at=now()
	WHERE id=$1::uuid AND status='sending' AND locked_by=$2 AND (expires_at IS NULL OR $3<expires_at)
	AND attempt_count<max_attempts RETURNING id::text`, deliveryID, workerID, sendAfter, nullableString(result.ErrorCode), nullableString(redact(result.Error))).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (r *Repository) DeleteAnonymizedBefore(ctx context.Context, before time.Time, limit int) (int64, error) {
	if limit < 1 || limit > 1000 {
		return 0, errors.New("invalid mail delivery retention batch size")
	}
	result, err := r.db.Exec(ctx, `DELETE FROM mail_deliveries WHERE id IN (
		SELECT id FROM mail_deliveries WHERE retention_state='anonymized' AND settled_at IS NOT NULL
		AND settled_at<$1 ORDER BY settled_at LIMIT $2
	)`, before, limit)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected(), nil
}

func (r *Repository) CleanupExpiredRollingQuota(ctx context.Context, limit int64) (int64, error) {
	return mailquota.CleanupExpired(ctx, r.db, limit)
}

func (r *Repository) markTerminal(ctx context.Context, deliveryID, workerID, status string, result DeliveryResult) error {
	if status != "sent" && status != "skipped" && status != "failed" && status != "canceled" {
		return errors.New("invalid terminal mail delivery status")
	}
	_, err := r.db.Exec(ctx, `UPDATE mail_deliveries SET status=$3,
		sent_at=CASE WHEN $3='sent' THEN now() ELSE sent_at END,
		failed_at=CASE WHEN $3='failed' THEN now() ELSE failed_at END,
		canceled_at=CASE WHEN $3='canceled' THEN now() ELSE canceled_at END,
		settled_at=now(),locked_by=NULL,locked_until=NULL,recipient_email=NULL,payload=NULL,
		retention_state='anonymized',anonymized_at=now(),provider_message_id=$4,provider_response=$5,
		last_error_code=$6,last_error=$7,updated_at=now()
	WHERE id=$1::uuid AND status='sending' AND locked_by=$2`, deliveryID, workerID, status,
		nullableString(result.ProviderMessageID), nullableString(redact(result.ProviderResponse)),
		nullableString(result.ErrorCode), nullableString(redact(result.Error)))
	return err
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
