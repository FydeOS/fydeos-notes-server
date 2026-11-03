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
)

type memberInvitationPolicyFake struct{ reason string }

func (f memberInvitationPolicyFake) InvitationReviewMailSkipReason(context.Context, string, string) (string, error) {
	return f.reason, nil
}

func (f memberInvitationPolicyFake) MemberLeaveMailSkipReason(context.Context, string, string) (string, error) {
	return f.reason, nil
}

func TestQueueMemberInvitationOutboxDedupePolicyAndQuota(t *testing.T) {
	t.Setenv("MAILER_HOST", "smtp.example.test")
	t.Setenv("MAILER_PORT", "25")
	t.Setenv("MAILER_SENDER", "Notes <notes@example.test>")
	t.Setenv("MAILER_SECURE", "false")
	t.Setenv("MAILER_IGNORE_TLS", "false")
	pool := mailDeliveryTestPool(t)
	ctx := context.Background()
	for _, statement := range []string{
		`CREATE TABLE app_configs (id text PRIMARY KEY, value jsonb NOT NULL)`,
		`CREATE TABLE users (id text PRIMARY KEY, name text, email text NOT NULL, disabled boolean NOT NULL DEFAULT false)`,
		`CREATE TABLE user_settings (user_id text PRIMARY KEY, payload jsonb NOT NULL)`,
		`CREATE TABLE workspaces (id text PRIMARY KEY, name text)`,
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
			priority text NOT NULL, status text NOT NULL, dedupe_key text, recipient_email text,
			recipient_hash text NOT NULL, recipient_domain text NOT NULL, recipient_user_id text, actor_user_id text,
			workspace_id text, notification_id text, quota_decision jsonb, payload jsonb, send_after timestamptz NOT NULL,
			max_attempts integer NOT NULL, settled_at timestamptz, retention_state text, anonymized_at timestamptz,
			last_error_code text)`,
		`CREATE UNIQUE INDEX mail_deliveries_dedupe_key_idx ON mail_deliveries(dedupe_key) WHERE dedupe_key IS NOT NULL`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("create member-invitation fixture: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,name,email) VALUES
		('invitee','Invitee','invitee@example.test'),('actor','Admin','admin@example.test'),
		('disabled-email','Disabled','disabled@example.test'),('domain-blocked','Domain','domain@example.test'),
		('quota-limited','Quota','quota@example.test')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO user_settings(user_id,payload) VALUES('disabled-email','{"receiveInvitationEmail":false}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id,name) VALUES
		('workspace','Project North Star'),('blocked-workspace','notes.example.test')`); err != nil {
		t.Fatal(err)
	}
	repository := NewRepository(pool, config.Config{ExternalURL: "https://notes.example.test/base"},
		func(context.Context) (string, error) { return "test-private-key", nil }, memberInvitationPolicyFake{})

	for range 2 {
		if err := repository.QueueMemberInvitation(ctx, "invitee", "actor", "workspace", "invite-1", "notification-1"); err != nil {
			t.Fatalf("queue member invitation: %v", err)
		}
	}
	var mailName, mailClass, status, dedupe, recipient, recipientID, actorID, workspaceID, notificationID string
	var payload, quotaDecision []byte
	if err := pool.QueryRow(ctx, `SELECT mail_name,mail_class,status,dedupe_key,recipient_email,recipient_user_id,
		actor_user_id,workspace_id,notification_id,payload,quota_decision FROM mail_deliveries`).Scan(
		&mailName, &mailClass, &status, &dedupe, &recipient, &recipientID, &actorID, &workspaceID, &notificationID, &payload, &quotaDecision); err != nil {
		t.Fatal(err)
	}
	var decodedPayload map[string]any
	var decodedDecision map[string]any
	if err := json.Unmarshal(payload, &decodedPayload); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(quotaDecision, &decodedDecision); err != nil {
		t.Fatal(err)
	}
	props := decodedPayload["props"].(map[string]any)
	workspace := props["workspace"].(map[string]any)
	actor := props["user"].(map[string]any)
	if mailName != "MemberInvitation" || mailClass != "workspace_invitation" || status != "queued" ||
		dedupe != "notification:notification-1:mail:MemberInvitation" || recipient != "invitee@example.test" ||
		recipientID != "invitee" || actorID != "actor" || workspaceID != "workspace" || notificationID != "notification-1" ||
		decodedPayload["name"] != "MemberInvitation" || decodedPayload["to"] != recipient ||
		props["url"] != "https://notes.example.test/base/invite/invite-1" || workspace["$$workspaceId"] != "workspace" || actor["$$userId"] != "actor" || decodedDecision["allowed"] != true {
		t.Fatalf("member-invitation outbox row = %q/%q/%q %q %#v %#v", mailName, mailClass, status, dedupe, decodedPayload, decodedDecision)
	}
	var deliveryCount, quotaScopeCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM mail_deliveries`).Scan(&deliveryCount); err != nil || deliveryCount != 1 {
		t.Fatalf("deduplicated member-invitation rows = %d, %v", deliveryCount, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM runtime_rolling_quota_counters`).Scan(&quotaScopeCount); err != nil || quotaScopeCount != 3 {
		t.Fatalf("member-invitation quota scopes = %d, %v", quotaScopeCount, err)
	}

	t.Setenv("MAILER_HOST", "")
	if err := repository.QueueMemberInvitation(ctx, "disabled-email", "actor", "workspace", "invite-2", "notification-2"); err != nil {
		t.Fatalf("skip member invitation by recipient preference: %v", err)
	}
	if err := repository.QueueMemberInvitation(ctx, "domain-blocked", "actor", "blocked-workspace", "invite-3", "notification-3"); err != nil {
		t.Fatalf("skip member invitation by workspace name policy: %v", err)
	}
	quotaScope := "mail:recipient:" + mailquota.RecipientScopeHash("quota@example.test") + ":class:workspace_invitation"
	now := time.Now().UTC()
	if _, err := pool.Exec(ctx, `INSERT INTO runtime_rolling_quota_counters(scope_key,window_seconds,bucket_start,count,expires_at,updated_at)
		VALUES($1,3600,$2,20,$3,$4)`, quotaScope, time.Unix(now.Unix(), 0).UTC(), now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAILER_HOST", "smtp.example.test")
	if err := repository.QueueMemberInvitation(ctx, "quota-limited", "actor", "workspace", "invite-4", "notification-4"); err != nil {
		t.Fatalf("record quota-denied member invitation: %v", err)
	}
	for _, test := range []struct {
		notificationID string
		reason         string
	}{
		{notificationID: "notification-2", reason: "recipient_notification_disabled"},
		{notificationID: "notification-3", reason: "workspace_name_contains_domain"},
		{notificationID: "notification-4", reason: "mail_class"},
	} {
		var skippedStatus, skippedReason string
		var skippedRecipient *string
		var skippedPayload []byte
		if err := pool.QueryRow(ctx, `SELECT status,last_error_code,recipient_email,payload FROM mail_deliveries WHERE notification_id=$1`, test.notificationID).
			Scan(&skippedStatus, &skippedReason, &skippedRecipient, &skippedPayload); err != nil {
			t.Fatal(err)
		}
		if skippedStatus != "skipped" || skippedReason != test.reason || skippedRecipient != nil || skippedPayload != nil {
			t.Fatalf("skipped invitation %s = %q/%q recipient=%v payload=%s", test.notificationID, skippedStatus, skippedReason, skippedRecipient, skippedPayload)
		}
	}
}
