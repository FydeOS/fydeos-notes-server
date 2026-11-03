// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package maildelivery

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/mailquota"
	"github.com/jackc/pgx/v5/pgxpool"
)

type invitationApprovalPolicyFake struct{ reason string }

func (f invitationApprovalPolicyFake) InvitationReviewMailSkipReason(context.Context, string, string) (string, error) {
	return f.reason, nil
}

func (f invitationApprovalPolicyFake) MemberLeaveMailSkipReason(context.Context, string, string) (string, error) {
	return f.reason, nil
}

func TestQueueInvitationReviewApprovedUsesDedupeAndCollaborationQuota(t *testing.T) {
	t.Setenv("MAILER_HOST", "smtp.example.test")
	t.Setenv("MAILER_PORT", "25")
	t.Setenv("MAILER_SENDER", "Notes <notes@example.test>")
	t.Setenv("MAILER_SECURE", "false")
	t.Setenv("MAILER_IGNORE_TLS", "false")
	pool := mailDeliveryTestPool(t)
	ctx := context.Background()
	for _, statement := range []string{
		`CREATE TABLE app_configs (id text PRIMARY KEY, value jsonb NOT NULL)`,
		`CREATE TABLE users (id text PRIMARY KEY, email text NOT NULL)`,
		`CREATE TABLE runtime_rolling_quota_counters (
			scope_key text NOT NULL, window_seconds integer NOT NULL, bucket_start timestamptz NOT NULL,
			count integer NOT NULL, expires_at timestamptz NOT NULL, updated_at timestamptz NOT NULL,
			PRIMARY KEY(scope_key,window_seconds,bucket_start))`,
		`CREATE TABLE runtime_rolling_quota_reservations (
			id uuid NOT NULL DEFAULT gen_random_uuid(), scope_key text NOT NULL, window_seconds integer NOT NULL,
			bucket_start timestamptz NOT NULL, count integer NOT NULL, status text NOT NULL,
			expires_at timestamptz NOT NULL, updated_at timestamptz NOT NULL DEFAULT now())`,
		`CREATE TABLE mail_deliveries (
			id uuid PRIMARY KEY DEFAULT gen_random_uuid(), mail_name text NOT NULL, mail_class text NOT NULL,
			priority text NOT NULL, status text NOT NULL, dedupe_key text, recipient_email text, recipient_hash text NOT NULL,
			recipient_domain text NOT NULL, recipient_user_id text, actor_user_id text, workspace_id text,
			notification_id text, quota_decision jsonb, payload jsonb, send_after timestamptz NOT NULL DEFAULT now(),
			max_attempts integer NOT NULL DEFAULT 3, settled_at timestamptz, retention_state text,
			anonymized_at timestamptz, last_error_code text)`,
		`CREATE UNIQUE INDEX mail_deliveries_dedupe_key_idx ON mail_deliveries(dedupe_key) WHERE dedupe_key IS NOT NULL`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("create approval mail fixture: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,email) VALUES('invitee','invitee@example.test'),('reviewer','reviewer@example.test')`); err != nil {
		t.Fatal(err)
	}
	repository := NewRepository(pool, config.Config{ExternalURL: "https://notes.example.test/base"},
		func(context.Context) (string, error) { return "test-private-key", nil }, invitationApprovalPolicyFake{})
	for range 2 {
		if err := repository.QueueInvitationReviewApproved(ctx, "invitee", "reviewer", "workspace-1", "notification-1"); err != nil {
			t.Fatalf("queue invitation approval email: %v", err)
		}
	}
	var mailName, mailClass, status, dedupe, recipient, recipientID, actorID, workspaceID, notificationID string
	var payload []byte
	if err := pool.QueryRow(ctx, `SELECT mail_name,mail_class,status,dedupe_key,recipient_email,recipient_user_id,
		actor_user_id,workspace_id,notification_id,payload FROM mail_deliveries`).Scan(
		&mailName, &mailClass, &status, &dedupe, &recipient, &recipientID, &actorID, &workspaceID, &notificationID, &payload); err != nil {
		t.Fatal(err)
	}
	var mail map[string]any
	if err := json.Unmarshal(payload, &mail); err != nil {
		t.Fatal(err)
	}
	props := mail["props"].(map[string]any)
	workspace := props["workspace"].(map[string]any)
	if mailName != "LinkInvitationApprove" || mailClass != "collaboration_notice" || status != "queued" ||
		dedupe != "notification:notification-1:mail:LinkInvitationApprove" || recipient != "invitee@example.test" ||
		recipientID != "invitee" || actorID != "reviewer" || workspaceID != "workspace-1" || notificationID != "notification-1" ||
		mail["name"] != "LinkInvitationApprove" || mail["to"] != recipient ||
		props["url"] != "https://notes.example.test/base/workspace/workspace-1" || workspace["$$workspaceId"] != "workspace-1" {
		t.Fatalf("approval mail outbox row = %q/%q/%q %q %#v", mailName, mailClass, status, dedupe, mail)
	}
	var deliveryCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM mail_deliveries`).Scan(&deliveryCount); err != nil || deliveryCount != 1 {
		t.Fatalf("deduplicated mail deliveries = %d, %v", deliveryCount, err)
	}
	quotaKey := "mail:recipient:" + mailquota.RecipientScopeHash("invitee@example.test") + ":class:collaboration_notice"
	var quotaTotal int
	if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(count),0)::int FROM runtime_rolling_quota_counters WHERE scope_key=$1`, quotaKey).Scan(&quotaTotal); err != nil || quotaTotal != 1 {
		t.Fatalf("deduplicated mail quota usage = %d, %v", quotaTotal, err)
	}

	if _, err := pool.Exec(ctx, `INSERT INTO users(id,email) VALUES('invitee-limited','limited@example.test')`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	limitedScope := "mail:recipient:" + mailquota.RecipientScopeHash("limited@example.test") + ":class:collaboration_notice"
	bucketStart := time.Unix(now.Unix(), 0).UTC()
	if _, err := pool.Exec(ctx, `INSERT INTO runtime_rolling_quota_counters(scope_key,window_seconds,bucket_start,count,expires_at,updated_at)
		VALUES($1,3600,$2,20,$3,$4)`, limitedScope, bucketStart, now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	if err := repository.QueueInvitationReviewApproved(ctx, "invitee-limited", "reviewer", "workspace-1", "notification-limited"); err != nil {
		t.Fatalf("record quota-limited approval email: %v", err)
	}
	var deniedStatus, deniedCode string
	var deniedEmail *string
	var deniedDecision map[string]any
	if err := pool.QueryRow(ctx, `SELECT status,last_error_code,recipient_email,quota_decision FROM mail_deliveries
		WHERE notification_id='notification-limited'`).Scan(&deniedStatus, &deniedCode, &deniedEmail, &deniedDecision); err != nil {
		t.Fatal(err)
	}
	if deniedStatus != "skipped" || deniedCode != "mail_class" || deniedEmail != nil ||
		deniedDecision["allowed"] != false || deniedDecision["mailClass"] != "collaboration_notice" ||
		deniedDecision["reason"] != "mail_class" || deniedDecision["scopeKey"] != limitedScope ||
		deniedDecision["windowSeconds"] != float64(3600) || deniedDecision["limit"] != float64(20) ||
		deniedDecision["current"] != float64(20) || deniedDecision["requested"] != float64(1) {
		t.Fatalf("quota-denied approval mail = %q/%q %v %#v", deniedStatus, deniedCode, deniedEmail, deniedDecision)
	}

	if err := repository.QueueInvitationReviewRequest(ctx, "reviewer", "invitee", "workspace-1", "notification-request"); err != nil {
		t.Fatalf("queue invitation review request email: %v", err)
	}
	if err := repository.QueueInvitationReviewRequest(ctx, "reviewer", "invitee", "workspace-1", "notification-request"); err != nil {
		t.Fatalf("deduplicate invitation review request email: %v", err)
	}
	if err := repository.QueueInvitationReviewDeclined(ctx, "invitee", "reviewer", "workspace-1", "notification-declined"); err != nil {
		t.Fatalf("queue invitation review declined email: %v", err)
	}
	if err := repository.QueueInvitationReviewDeclined(ctx, "invitee", "reviewer", "workspace-1", "notification-declined"); err != nil {
		t.Fatalf("deduplicate invitation review declined email: %v", err)
	}
	assertInvitationReviewMail(t, ctx, pool, "LinkInvitationReviewRequest", "reviewer@example.test", "reviewer", "invitee", "notification-request", "https://notes.example.test/base/workspace/workspace-1/settings?tab=workspace%3Amembers", "invitee")
	assertInvitationReviewMail(t, ctx, pool, "LinkInvitationDecline", "invitee@example.test", "invitee", "reviewer", "notification-declined", "", "")
}

func assertInvitationReviewMail(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name, recipient, recipientID, actorID, notificationID, expectedURL, expectedInviteeID string) {
	t.Helper()
	var mailClass, status, dedupe string
	var gotRecipient, gotRecipientID, gotActorID, gotWorkspaceID, gotNotificationID string
	var payload []byte
	if err := pool.QueryRow(ctx, `SELECT mail_class,status,dedupe_key,recipient_email,recipient_user_id,actor_user_id,workspace_id,notification_id,payload FROM mail_deliveries WHERE mail_name=$1 AND notification_id=$2`, name, notificationID).
		Scan(&mailClass, &status, &dedupe, &gotRecipient, &gotRecipientID, &gotActorID, &gotWorkspaceID, &gotNotificationID, &payload); err != nil {
		t.Fatalf("read %s delivery: %v", name, err)
	}
	var mail map[string]any
	if err := json.Unmarshal(payload, &mail); err != nil {
		t.Fatalf("decode %s payload: %v", name, err)
	}
	props := mail["props"].(map[string]any)
	workspace := props["workspace"].(map[string]any)
	if mailClass != "collaboration_notice" || status != "queued" || dedupe != "notification:"+notificationID+":mail:"+name ||
		gotRecipient != recipient || gotRecipientID != recipientID || gotActorID != actorID || gotWorkspaceID != "workspace-1" || gotNotificationID != notificationID ||
		mail["name"] != name || mail["to"] != recipient || workspace["$$workspaceId"] != "workspace-1" {
		t.Fatalf("%s delivery = %q/%q/%q %q/%q/%q %#v", name, mailClass, status, dedupe, gotRecipient, gotRecipientID, gotActorID, mail)
	}
	if expectedURL != "" && props["url"] != expectedURL {
		t.Fatalf("%s URL = %#v, want %q", name, props["url"], expectedURL)
	}
	if expectedInviteeID != "" {
		user := props["user"].(map[string]any)
		if user["$$userId"] != expectedInviteeID {
			t.Fatalf("%s invitee user = %#v", name, user)
		}
	}
}

func TestQueueInvitationReviewApprovedRecordsQuarantineWithoutPII(t *testing.T) {
	t.Setenv("MAILER_HOST", "smtp.example.test")
	t.Setenv("MAILER_PORT", "25")
	t.Setenv("MAILER_SENDER", "Notes <notes@example.test>")
	t.Setenv("MAILER_SECURE", "false")
	t.Setenv("MAILER_IGNORE_TLS", "false")
	pool := mailDeliveryTestPool(t)
	ctx := context.Background()
	for _, statement := range []string{
		`CREATE TABLE app_configs (id text PRIMARY KEY, value jsonb NOT NULL)`,
		`CREATE TABLE users (id text PRIMARY KEY, email text NOT NULL)`,
		`CREATE TABLE runtime_rolling_quota_counters (scope_key text NOT NULL, window_seconds integer NOT NULL, bucket_start timestamptz NOT NULL, count integer NOT NULL, expires_at timestamptz NOT NULL, updated_at timestamptz NOT NULL, PRIMARY KEY(scope_key,window_seconds,bucket_start))`,
		`CREATE TABLE runtime_rolling_quota_reservations (id uuid NOT NULL DEFAULT gen_random_uuid(), scope_key text NOT NULL, window_seconds integer NOT NULL, bucket_start timestamptz NOT NULL, count integer NOT NULL, status text NOT NULL, expires_at timestamptz NOT NULL, updated_at timestamptz NOT NULL DEFAULT now())`,
		`CREATE TABLE mail_deliveries (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), mail_name text NOT NULL, mail_class text NOT NULL, priority text NOT NULL, status text NOT NULL, dedupe_key text, recipient_email text, recipient_hash text NOT NULL, recipient_domain text NOT NULL, recipient_user_id text, actor_user_id text, workspace_id text, notification_id text, quota_decision jsonb, payload jsonb, send_after timestamptz NOT NULL DEFAULT now(), max_attempts integer NOT NULL DEFAULT 3, settled_at timestamptz, retention_state text, anonymized_at timestamptz, last_error_code text)`,
		`CREATE UNIQUE INDEX mail_deliveries_dedupe_key_idx ON mail_deliveries(dedupe_key) WHERE dedupe_key IS NOT NULL`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("create quarantined approval mail fixture: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,email) VALUES('invitee','invitee@example.test'),('reviewer','reviewer@example.test')`); err != nil {
		t.Fatal(err)
	}
	repository := NewRepository(pool, config.Config{}, func(context.Context) (string, error) { return "test-private-key", nil }, invitationApprovalPolicyFake{reason: "actor_quarantined"})
	if err := repository.QueueInvitationReviewApproved(ctx, "invitee", "reviewer", "workspace-1", "notification-1"); err != nil {
		t.Fatalf("record quarantined approval email: %v", err)
	}
	if err := repository.QueueInvitationReviewRequest(ctx, "reviewer", "invitee", "workspace-1", "notification-request"); err != nil {
		t.Fatalf("record quarantined request email: %v", err)
	}
	if err := repository.QueueInvitationReviewDeclined(ctx, "invitee", "reviewer", "workspace-1", "notification-declined"); err != nil {
		t.Fatalf("record quarantined declined email: %v", err)
	}
	var skippedWithoutPII int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM mail_deliveries WHERE mail_class='workspace_invitation' AND status='skipped'
		AND last_error_code='actor_quarantined' AND recipient_email IS NULL AND payload IS NULL`).Scan(&skippedWithoutPII); err != nil {
		t.Fatal(err)
	}
	if skippedWithoutPII != 3 {
		t.Fatalf("quarantined invitation mail rows without PII = %d, want 3", skippedWithoutPII)
	}
}

var _ InvitationReviewMailPolicy = invitationApprovalPolicyFake{}
