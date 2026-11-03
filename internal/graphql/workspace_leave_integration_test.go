// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"strings"
	"testing"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/invitelinks"
	"github.com/fydeos/fydeos-notes-server/internal/maildelivery"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

func TestLeaveWorkspaceGraphQLCompatibility(t *testing.T) {
	t.Setenv("MAILER_HOST", "smtp.example.test")
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	for _, statement := range []string{
		`CREATE TABLE runtime_rolling_quota_counters (scope_key text NOT NULL, window_seconds integer NOT NULL, bucket_start timestamptz NOT NULL, count integer NOT NULL, expires_at timestamptz NOT NULL, updated_at timestamptz NOT NULL, PRIMARY KEY(scope_key,window_seconds,bucket_start))`,
		`CREATE TABLE runtime_rolling_quota_reservations (id uuid NOT NULL DEFAULT gen_random_uuid(), scope_key text NOT NULL, window_seconds integer NOT NULL, bucket_start timestamptz NOT NULL, count integer NOT NULL, status text NOT NULL, expires_at timestamptz NOT NULL, updated_at timestamptz NOT NULL DEFAULT now())`,
		`CREATE TABLE mail_deliveries (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), mail_name text NOT NULL, mail_class text NOT NULL, priority text NOT NULL, status text NOT NULL, recipient_email text, recipient_hash text NOT NULL, recipient_domain text NOT NULL, recipient_user_id text, actor_user_id text, workspace_id text, quota_decision jsonb, payload jsonb, send_after timestamptz NOT NULL DEFAULT now(), max_attempts integer NOT NULL DEFAULT 3, settled_at timestamptz, retention_state text, anonymized_at timestamptz, last_error_code text, created_at timestamptz NOT NULL DEFAULT now())`,
		`CREATE TABLE runtime_invite_abuse_subjects (subject_key text PRIMARY KEY, kind text NOT NULL, status text NOT NULL, action text, action_reason text, user_id text, actor_email_hash text, email_domain text)`,
		`CREATE TABLE runtime_invite_abuse_evidence (id bigserial PRIMARY KEY, subject_key text NOT NULL REFERENCES runtime_invite_abuse_subjects(subject_key), workspace_id text, user_id text, actor_email_hash text, source_prefix_hash text, source_asn bigint, target_domains jsonb NOT NULL, counters jsonb NOT NULL, decision text NOT NULL, reason text NOT NULL)`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("create leave-workspace fixture: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE workspace_members SET role='member' WHERE workspace_id=$1 AND user_id=$2`, fixture.workspaceID, fixture.sharedUserID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_invitations(id,workspace_id,invitee_user_id,requested_role,status,kind) VALUES('leave-invitation',$1,$2,'member','pending','email')`, fixture.workspaceID, fixture.sharedUserID); err != nil {
		t.Fatal(err)
	}

	repository := workspaces.NewRepository(pool)
	publisher := &workspaceRoleMutationPublisherFake{}
	mailer := maildelivery.NewRepository(pool, config.Config{}, func(context.Context) (string, error) {
		return "test-mail-private-key", nil
	}, invitelinks.NewMemberLeaveMailPolicy(pool))
	handler := NewHandlerWithServices(
		config.Config{Version: "0.27.5", Deployment: "selfhosted", Host: "localhost", Port: 3010},
		auth.NewSessionResolver(pool), nil, repository, nil, nil, nil, nil,
		HandlerDependencies{WorkspaceRoles: WorkspaceRoleMutationDependencies{
			Repository: repository, LeaveMailer: mailer, Publisher: publisher,
		}},
	)
	query := `mutation leaveWorkspace($workspaceId: String!) { leaveWorkspace(workspaceId: $workspaceId) }`
	variables := map[string]any{"workspaceId": fixture.workspaceID}
	result := graphRequest(t, handler, query, variables, fixture.sessionID, fixture.sharedUserID)
	if result["errors"] != nil || result["data"].(map[string]any)["leaveWorkspace"] != true {
		t.Fatalf("leaveWorkspace GraphQL result = %#v", result)
	}
	var activeRows, invitations int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspace_members WHERE workspace_id=$1 AND user_id=$2 AND state='active'`, fixture.workspaceID, fixture.sharedUserID).Scan(&activeRows); err != nil || activeRows != 0 {
		t.Fatalf("active membership rows=%d, err=%v", activeRows, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspace_invitations WHERE workspace_id=$1 AND invitee_user_id=$2`, fixture.workspaceID, fixture.sharedUserID).Scan(&invitations); err != nil || invitations != 0 {
		t.Fatalf("pending invitations=%d, err=%v", invitations, err)
	}
	var mailName, mailStatus, recipient, recipientID, actorID, mailWorkspace string
	var payload map[string]any
	if err := pool.QueryRow(ctx, `SELECT mail_name,status,recipient_email,recipient_user_id,actor_user_id,workspace_id,payload FROM mail_deliveries`).
		Scan(&mailName, &mailStatus, &recipient, &recipientID, &actorID, &mailWorkspace, &payload); err != nil {
		t.Fatal(err)
	}
	props := payload["props"].(map[string]any)
	workspaceProp := props["workspace"].(map[string]any)
	userProp := props["user"].(map[string]any)
	if mailName != "MemberLeave" || mailStatus != "queued" || recipient != "web@example.test" || recipientID != fixture.userID || actorID != fixture.sharedUserID ||
		mailWorkspace != fixture.workspaceID || workspaceProp["$$workspaceId"] != fixture.workspaceID || userProp["$$userId"] != fixture.sharedUserID {
		t.Fatalf("member-leave mail row = %q/%q to=%q recipient=%q actor=%q workspace=%q payload=%#v", mailName, mailStatus, recipient, recipientID, actorID, mailWorkspace, payload)
	}
	if len(publisher.workspaceIDs) != 1 || publisher.workspaceIDs[0] != fixture.workspaceID {
		t.Fatalf("members-updated events = %#v", publisher.workspaceIDs)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_members(id,workspace_id,user_id,role,state) VALUES('member-leave-again',$1,$2,'member','active')`, fixture.workspaceID, fixture.sharedUserID); err != nil {
		t.Fatal(err)
	}
	actorHash := "actor_email_sha256:v1:" + strings.Repeat("a", 64)
	if _, err := pool.Exec(ctx, `
		INSERT INTO runtime_invite_abuse_subjects(subject_key,kind,status,action,action_reason,user_id,actor_email_hash,email_domain)
		VALUES($1,'actor_email','quarantined','quarantine_actor','high_risk_domain_burst',$2,$1,'example.test')`, actorHash, fixture.sharedUserID); err != nil {
		t.Fatalf("insert quarantined actor subject: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO runtime_invite_abuse_evidence(subject_key,workspace_id,user_id,actor_email_hash,target_domains,counters,decision,reason)
		VALUES($1,$3,$2,$1,'[{"domain":"example.test","count":1}]'::jsonb,'{"requested":1}'::jsonb,'quarantine_actor','high_risk_domain_burst')`, actorHash, fixture.sharedUserID, fixture.workspaceID); err != nil {
		t.Fatalf("insert quarantined actor fixture: %v", err)
	}
	quarantinedResult := graphRequest(t, handler, query, variables, fixture.sessionID, fixture.sharedUserID)
	if quarantinedResult["errors"] != nil || quarantinedResult["data"].(map[string]any)["leaveWorkspace"] != true {
		t.Fatalf("quarantined member leave result = %#v", quarantinedResult)
	}
	var skippedStatus, skipReason string
	var hasSkippedEmail, hasSkippedPayload bool
	if err := pool.QueryRow(ctx, `SELECT status,quota_decision->>'reason',recipient_email IS NOT NULL,payload IS NOT NULL FROM mail_deliveries WHERE status='skipped'`).Scan(&skippedStatus, &skipReason, &hasSkippedEmail, &hasSkippedPayload); err != nil {
		t.Fatalf("load policy-skipped member-leave delivery: %v", err)
	}
	if skippedStatus != "skipped" || skipReason != "actor_quarantined" || hasSkippedEmail || hasSkippedPayload {
		t.Fatalf("quarantine mail ledger = status %q reason %q hasRecipient %t hasPayload %t", skippedStatus, skipReason, hasSkippedEmail, hasSkippedPayload)
	}

	ownerResult := graphRequest(t, handler, query, variables, fixture.sessionID, fixture.userID)
	if ownerResult["errors"] == nil {
		t.Fatalf("owner leave result = %#v, want owner prohibition", ownerResult)
	}
	ownerError := ownerResult["errors"].([]any)[0].(map[string]any)
	ownerExtensions := ownerError["extensions"].(map[string]any)
	if ownerExtensions["name"] != "owner_can_not_leave_workspace" || ownerError["message"] != "Owner can not leave the workspace." {
		t.Fatalf("owner leave error contract = %#v", ownerError)
	}
	var ownerRows, deliveries int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspace_members WHERE workspace_id=$1 AND user_id=$2 AND state='active'`, fixture.workspaceID, fixture.userID).Scan(&ownerRows); err != nil || ownerRows != 1 {
		t.Fatalf("owner membership rows=%d, err=%v", ownerRows, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM mail_deliveries`).Scan(&deliveries); err != nil || deliveries != 2 {
		t.Fatalf("mail deliveries=%d, err=%v", deliveries, err)
	}
	if len(publisher.workspaceIDs) != 2 {
		t.Fatalf("owner rejection published a membership event: %#v", publisher.workspaceIDs)
	}

	unauthenticated := graphRequest(t, handler, query, variables, "", "")
	if unauthenticated["errors"] == nil {
		t.Fatalf("anonymous leaveWorkspace result = %#v, want authentication error", unauthenticated)
	}
}
