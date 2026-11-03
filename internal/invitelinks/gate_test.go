// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package invitelinks

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/entitlements"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestInviteLinkPolicyDecisionMatchesNativeGateOrdering(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	actor := inviteActorFacts{userID: "user-1", email: "person@example.com", createdAt: now.Add(-48 * time.Hour), registered: true}
	quota := inviteQuotaFacts{known: true, plan: "free"}
	cleanAbuse := inviteAbuseFacts{valid: true}
	workspaceID := "workspace-1"

	if allowed, reason := inviteLinkPolicyDecision(entitlements.Cloud, "private-key", 24*time.Hour, now, actor, workspaceID, quota, cleanAbuse); !allowed || reason != "" {
		t.Fatalf("baseline policy = (%v, %q)", allowed, reason)
	}

	for _, test := range []struct {
		name       string
		deployment entitlements.Deployment
		salt       string
		delay      time.Duration
		actor      inviteActorFacts
		quota      inviteQuotaFacts
		abuse      inviteAbuseFacts
		wantReason string
	}{
		{name: "missing salt", deployment: entitlements.Cloud, delay: 24 * time.Hour, actor: actor, quota: quota, abuse: cleanAbuse, wantReason: "policy_state_invalid"},
		{name: "invalid email", deployment: entitlements.Cloud, salt: "private-key", delay: 24 * time.Hour, actor: inviteActorFacts{email: "bad@@example.com", createdAt: actor.createdAt}, quota: quota, abuse: cleanAbuse, wantReason: "policy_state_invalid"},
		{name: "invalid abuse facts", deployment: entitlements.Cloud, salt: "private-key", delay: 24 * time.Hour, actor: actor, quota: quota, abuse: inviteAbuseFacts{valid: false}, wantReason: "policy_state_invalid"},
		{name: "actor abuse subject", deployment: entitlements.Cloud, salt: "private-key", delay: 24 * time.Hour, actor: actor, quota: quota, abuse: inviteAbuseFacts{valid: true, subjects: []inviteAbuseSubject{{kind: "actor_email", key: actorEmailSubjectKey(actor.email, "private-key"), status: "banned"}}}, wantReason: "abuse_subject"},
		{name: "quarantined workspace", deployment: entitlements.Cloud, salt: "private-key", delay: 24 * time.Hour, actor: actor, quota: quota, abuse: inviteAbuseFacts{valid: true, subjects: []inviteAbuseSubject{{kind: "workspace", key: workspaceSubjectKey(workspaceID), status: "quarantined"}}}, wantReason: "abuse_workspace"},
		{name: "missing quota", deployment: entitlements.Cloud, salt: "private-key", delay: 24 * time.Hour, actor: actor, quota: inviteQuotaFacts{}, abuse: cleanAbuse, wantReason: "quota_state_unavailable"},
		{name: "new account delay", deployment: entitlements.Cloud, salt: "private-key", delay: 24 * time.Hour, actor: inviteActorFacts{userID: actor.userID, email: actor.email, createdAt: now.Add(-time.Hour)}, quota: quota, abuse: cleanAbuse, wantReason: "new_account_action_delay"},
	} {
		t.Run(test.name, func(t *testing.T) {
			allowed, reason := inviteLinkPolicyDecision(test.deployment, test.salt, test.delay, now, test.actor, workspaceID, test.quota, test.abuse)
			if allowed || reason != test.wantReason {
				t.Fatalf("policy = (%v, %q), want denied for %q", allowed, reason, test.wantReason)
			}
		})
	}

	for _, test := range []struct {
		name       string
		deployment entitlements.Deployment
		actorPlan  string
		quotaPlan  string
	}{
		{name: "cloud pro actor", deployment: entitlements.Cloud, actorPlan: "pro", quotaPlan: "free"},
		{name: "cloud team workspace", deployment: entitlements.Cloud, actorPlan: "free", quotaPlan: "team"},
		{name: "self-hosted bypass", deployment: entitlements.SelfHosted, actorPlan: "selfhost_free", quotaPlan: "selfhost_free"},
	} {
		t.Run(test.name, func(t *testing.T) {
			paidActor := actor
			paidActor.createdAt = now.Add(-time.Hour)
			paidActor.plan = test.actorPlan
			paidQuota := quota
			paidQuota.plan = test.quotaPlan
			allowed, reason := inviteLinkPolicyDecision(test.deployment, "private-key", 24*time.Hour, now, paidActor, workspaceID, paidQuota, cleanAbuse)
			if !allowed || reason != "" {
				t.Fatalf("exempt policy = (%v, %q)", allowed, reason)
			}
		})
	}
}

func TestValidatePersistedInviteAbuseRecord(t *testing.T) {
	key := actorEmailSubjectKey("person@example.com", "private-key")
	row := abuseRow{
		key: key, kind: "actor_email", status: "banned",
		action:                 pgtype.Text{String: "ban_actor", Valid: true},
		reason:                 pgtype.Text{String: "high_risk_domain_burst", Valid: true},
		userID:                 pgtype.Text{String: "user-1", Valid: true},
		actorEmailHash:         pgtype.Text{String: key, Valid: true},
		emailDomain:            pgtype.Text{String: "example.com", Valid: true},
		evidenceSubjectKey:     pgtype.Text{String: key, Valid: true},
		evidenceWorkspaceID:    pgtype.Text{String: "workspace-1", Valid: true},
		evidenceUserID:         pgtype.Text{String: "user-1", Valid: true},
		evidenceActorEmailHash: pgtype.Text{String: key, Valid: true},
		evidenceTargetDomains:  []byte(`[{"domain":"example.com","count":1}]`),
		evidenceCounters:       []byte(`{"requested":1}`),
		evidenceAction:         pgtype.Text{String: "ban_actor", Valid: true},
		evidenceReason:         pgtype.Text{String: "high_risk_domain_burst", Valid: true},
	}
	if !validateAbuseRow(row) {
		t.Fatal("valid persisted abuse record was rejected")
	}
	row.evidenceReason.String = "workspace_high_risk_domain_burst"
	if validateAbuseRow(row) {
		t.Fatal("inconsistent persisted reason was accepted")
	}
	row.evidenceReason.String = "high_risk_domain_burst"
	row.evidenceTargetDomains = []byte(`[]`)
	if validateAbuseRow(row) {
		t.Fatal("inconsistent target counters were accepted")
	}
}

func TestInviteSubjectHashesUseLegacyFormat(t *testing.T) {
	actorDigest := sha256.Sum256([]byte("private-key:person@example.com"))
	wantActor := "actor_email_sha256:v1:" + hex.EncodeToString(actorDigest[:])
	if got := actorEmailSubjectKey(" Person@Example.com ", "private-key"); got != wantActor {
		t.Fatalf("actor subject = %q, want %q", got, wantActor)
	}
	workspaceDigest := sha256.Sum256([]byte("workspace-1"))
	wantWorkspace := "workspace:v1:" + hex.EncodeToString(workspaceDigest[:])[:24]
	if got := workspaceSubjectKey("workspace-1"); got != wantWorkspace {
		t.Fatalf("workspace subject = %q, want %q", got, wantWorkspace)
	}
}
