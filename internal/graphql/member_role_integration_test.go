// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"reflect"
	"testing"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/maildelivery"
	"github.com/fydeos/fydeos-notes-server/internal/notifications"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

type workspaceRoleMutationPublisherFake struct {
	workspaceIDs []string
}

func (f *workspaceRoleMutationPublisherFake) PublishWorkspaceMembersUpdated(workspaceID string) {
	f.workspaceIDs = append(f.workspaceIDs, workspaceID)
}

type workspaceRoleNotificationCountPublisherFake struct {
	userIDs []string
	counts  []int
	reasons []string
}

func (f *workspaceRoleNotificationCountPublisherFake) PublishNotificationCountChanged(userID string, count int, reason string) {
	f.userIDs = append(f.userIDs, userID)
	f.counts = append(f.counts, count)
	f.reasons = append(f.reasons, reason)
}

func TestGrantMemberGraphQLCompatibility(t *testing.T) {
	t.Setenv("MAILER_HOST", "smtp.example.test")
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	for _, statement := range []string{
		`CREATE TYPE "NotificationLevel" AS ENUM ('High','Default','Low','Min','None')`,
		`CREATE TYPE "NotificationType" AS ENUM ('Mention','Invitation','InvitationAccepted','InvitationBlocked','InvitationRejected','InvitationReviewRequest','InvitationReviewApproved','InvitationReviewDeclined','Comment','CommentMention')`,
		`CREATE TABLE notifications (id text PRIMARY KEY, user_id text NOT NULL, created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(), level "NotificationLevel" NOT NULL, read boolean NOT NULL DEFAULT false, type "NotificationType" NOT NULL, body jsonb NOT NULL)`,
		`CREATE TABLE runtime_rolling_quota_counters (scope_key text NOT NULL,window_seconds integer NOT NULL,bucket_start timestamptz NOT NULL,count integer NOT NULL,expires_at timestamptz NOT NULL,updated_at timestamptz NOT NULL,PRIMARY KEY(scope_key,window_seconds,bucket_start))`,
		`CREATE TABLE runtime_rolling_quota_reservations (id uuid NOT NULL DEFAULT gen_random_uuid(),scope_key text NOT NULL,window_seconds integer NOT NULL,bucket_start timestamptz NOT NULL,count integer NOT NULL,status text NOT NULL,expires_at timestamptz NOT NULL)`,
		`CREATE TABLE mail_deliveries (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), mail_name text NOT NULL, mail_class text NOT NULL, priority text NOT NULL, status text NOT NULL, recipient_email text, recipient_hash text NOT NULL, recipient_domain text NOT NULL, recipient_user_id text, actor_user_id text, workspace_id text, quota_decision jsonb, payload jsonb, send_after timestamptz NOT NULL DEFAULT now(), max_attempts integer NOT NULL DEFAULT 3)`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("create role mutation notification fixture: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE workspace_members SET role='member' WHERE workspace_id=$1 AND user_id=$2`, fixture.workspaceID, fixture.sharedUserID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO entitlements(target_type,target_id,source,plan,quantity,status) VALUES ('workspace',$1,'cloud_subscription','team',5,'active')`, fixture.workspaceID); err != nil {
		t.Fatal(err)
	}

	repository := workspaces.NewRepositoryWithDeployment(pool, "cloud")
	publisher := &workspaceRoleMutationPublisherFake{}
	countPublisher := &workspaceRoleNotificationCountPublisherFake{}
	notificationRepository := notifications.NewRepository(pool, "https://notes.example.test")
	handler := NewHandlerWithServices(
		config.Config{Version: "0.27.5", Deployment: "cloud", Host: "localhost", Port: 3010},
		auth.NewSessionResolver(pool), nil, repository, nil, nil, nil, nil,
		HandlerDependencies{WorkspaceRoles: WorkspaceRoleMutationDependencies{
			Repository: repository, Notifications: notificationRepository,
			RemovalMailer: maildelivery.NewRepository(pool, config.Config{}, func(context.Context) (string, error) {
				return "test-mail-private-key", nil
			}, nil),
			CountPublisher: countPublisher, Publisher: publisher,
		}},
	)
	query := `mutation grantMember($permission: Permission!, $userId: String!, $workspaceId: String!) {
		grantMember(permission: $permission, userId: $userId, workspaceId: $workspaceId)
	}`
	variables := map[string]any{"permission": "Admin", "userId": fixture.sharedUserID, "workspaceId": fixture.workspaceID}
	result := graphRequest(t, handler, query, variables, fixture.sessionID, fixture.userID)
	if result["errors"] != nil || result["data"].(map[string]any)["grantMember"] != true {
		t.Fatalf("grantMember GraphQL result = %#v", result)
	}
	var role string
	if err := pool.QueryRow(ctx, `SELECT role FROM workspace_members WHERE workspace_id=$1 AND user_id=$2 AND state='active'`, fixture.workspaceID, fixture.sharedUserID).Scan(&role); err != nil || role != "admin" {
		t.Fatalf("persisted member role = %q, %v; want admin", role, err)
	}
	if len(publisher.workspaceIDs) != 1 || publisher.workspaceIDs[0] != fixture.workspaceID {
		t.Fatalf("published workspace IDs = %#v", publisher.workspaceIDs)
	}

	variables["permission"] = "Owner"
	result = graphRequest(t, handler, query, variables, fixture.sessionID, fixture.userID)
	if result["errors"] != nil || result["data"].(map[string]any)["grantMember"] != true {
		t.Fatalf("owner transfer GraphQL result = %#v", result)
	}
	var quotaOwner string
	var known, stale bool
	if err := pool.QueryRow(ctx, `SELECT owner_user_id,known,stale FROM effective_workspace_quota_states WHERE workspace_id=$1`, fixture.workspaceID).Scan(&quotaOwner, &known, &stale); err != nil {
		t.Fatal(err)
	}
	if quotaOwner != fixture.sharedUserID || known || !stale {
		t.Fatalf("quota mapping after owner transfer = %q known=%t stale=%t", quotaOwner, known, stale)
	}
	if len(publisher.workspaceIDs) != 2 {
		t.Fatalf("members updated event count = %d, want 2", len(publisher.workspaceIDs))
	}

	if _, err := pool.Exec(ctx, `INSERT INTO workspace_invitations(id,workspace_id,invitee_user_id,requested_role,status,kind) VALUES ('review-invite',$1,$2,'member','waiting_review','link')`, fixture.workspaceID, fixture.disabledUserID); err != nil {
		t.Fatal(err)
	}
	variables["permission"] = "External"
	variables["userId"] = fixture.disabledUserID
	result = graphRequest(t, handler, query, variables, fixture.sessionID, fixture.userID)
	if result["errors"] != nil || result["data"].(map[string]any)["grantMember"] != true {
		t.Fatalf("review invitation rejection GraphQL result = %#v", result)
	}
	var notificationUser, notificationType string
	var notificationBody map[string]any
	if err := pool.QueryRow(ctx, `SELECT user_id,type::text,body FROM notifications`).Scan(&notificationUser, &notificationType, &notificationBody); err != nil {
		t.Fatal(err)
	}
	if notificationUser != fixture.disabledUserID || notificationType != "InvitationReviewDeclined" || notificationBody["workspaceId"] != fixture.workspaceID || notificationBody["createdByUserId"] != fixture.userID {
		t.Fatalf("review declined notification = user %q type %q body %#v", notificationUser, notificationType, notificationBody)
	}
	if len(countPublisher.userIDs) != 1 || countPublisher.userIDs[0] != fixture.disabledUserID || countPublisher.counts[0] != 1 || countPublisher.reasons[0] != "created" {
		t.Fatalf("review notification count events = %+v", countPublisher)
	}
	if len(publisher.workspaceIDs) != 3 {
		t.Fatalf("members updated event count = %d, want 3", len(publisher.workspaceIDs))
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_members(id,workspace_id,user_id,role,state) VALUES('active-to-remove',$1,$2,'member','active')`, fixture.workspaceID, fixture.disabledUserID); err != nil {
		t.Fatal(err)
	}
	result = graphRequest(t, handler, query, variables, fixture.sessionID, fixture.userID)
	if result["errors"] != nil || result["data"].(map[string]any)["grantMember"] != true {
		t.Fatalf("active member removal GraphQL result = %#v", result)
	}
	var mailName, mailClass, mailStatus, recipient, recipientID, mailWorkspace, recipientHash, recipientDomain string
	var mailPayload map[string]any
	if err := pool.QueryRow(ctx, `SELECT mail_name,mail_class,status,recipient_email,recipient_user_id,workspace_id,recipient_hash,recipient_domain,payload FROM mail_deliveries`).Scan(&mailName, &mailClass, &mailStatus, &recipient, &recipientID, &mailWorkspace, &recipientHash, &recipientDomain, &mailPayload); err != nil {
		t.Fatal(err)
	}
	props := mailPayload["props"].(map[string]any)
	workspaceProp := props["workspace"].(map[string]any)
	if mailName != "MemberRemoved" || mailClass != "workspace_lifecycle" || mailStatus != "queued" || recipient != "disabled@example.test" || recipientID != fixture.disabledUserID || mailWorkspace != fixture.workspaceID || len(recipientHash) != 64 || recipientDomain != "example.test" || workspaceProp["$$workspaceId"] != fixture.workspaceID {
		t.Fatalf("member-removal mail outbox row = %q/%q/%q %q %q %q %q %q %#v", mailName, mailClass, mailStatus, recipient, recipientID, mailWorkspace, recipientHash, recipientDomain, mailPayload)
	}
	var activeRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspace_members WHERE workspace_id=$1 AND user_id=$2 AND state='active'`, fixture.workspaceID, fixture.disabledUserID).Scan(&activeRows); err != nil || activeRows != 0 {
		t.Fatalf("removed active membership rows=%d, err=%v", activeRows, err)
	}
	if len(publisher.workspaceIDs) != 4 {
		t.Fatalf("members updated event count = %d, want 4", len(publisher.workspaceIDs))
	}

	variables["permission"] = "Owner"
	variables["userId"] = fixture.sharedUserID
	unauthenticated := graphRequest(t, handler, query, variables, "", "")
	if unauthenticated["errors"] == nil {
		t.Fatal("anonymous grantMember unexpectedly succeeded")
	}

	variables["userId"] = "missing-member"
	missing := graphRequest(t, handler, query, variables, fixture.sessionID, fixture.userID)
	if missing["errors"] == nil {
		t.Fatalf("missing-member grantMember result = %#v, want error", missing)
	}
	graphError := missing["errors"].([]any)[0].(map[string]any)
	graphqlExtensions := graphError["extensions"].(map[string]any)
	data := graphqlExtensions["data"].(map[string]any)
	if graphqlExtensions["type"] != "action_forbidden" || graphqlExtensions["name"] != "member_not_found_in_space" || data["spaceId"] != fixture.workspaceID {
		t.Fatalf("missing-member error contract = %#v", graphqlExtensions)
	}

	variables["permission"] = "Collaborator"
	variables["userId"] = fixture.userID
	if _, err := pool.Exec(ctx, `DELETE FROM entitlements WHERE target_type='workspace' AND target_id=$1`, fixture.workspaceID); err != nil {
		t.Fatal(err)
	}
	denied := graphRequest(t, handler, query, variables, fixture.sessionID, fixture.userID)
	if denied["errors"] == nil {
		t.Fatalf("unlicensed admin grantMember result = %#v, want error", denied)
	}
	deniedExtensions := denied["errors"].([]any)[0].(map[string]any)["extensions"].(map[string]any)
	deniedData := deniedExtensions["data"].(map[string]any)
	if deniedExtensions["type"] != "no_permission" || deniedExtensions["name"] != "space_access_denied" || deniedData["spaceId"] != fixture.workspaceID {
		t.Fatalf("permission denial error contract = %#v", deniedExtensions)
	}
	variables["userId"] = "unknown-user"
	unknownTarget := graphRequest(t, handler, query, variables, fixture.sessionID, fixture.userID)
	if unknownTarget["errors"] == nil {
		t.Fatalf("unlicensed admin missing-target result = %#v, want error", unknownTarget)
	}
	unknownExtensions := unknownTarget["errors"].([]any)[0].(map[string]any)["extensions"].(map[string]any)
	unknownData := unknownExtensions["data"].(map[string]any)
	if unknownExtensions["type"] != "no_permission" || unknownExtensions["name"] != "space_access_denied" || unknownData["spaceId"] != fixture.workspaceID {
		t.Fatalf("unauthorized missing-target error contract = %#v", unknownExtensions)
	}
	variables["permission"] = "External"
	variables["userId"] = fixture.sharedUserID
	deniedExternalExisting := graphRequest(t, handler, query, variables, fixture.sessionID, fixture.userID)
	if deniedExternalExisting["errors"] == nil {
		t.Fatalf("unlicensed admin External grantMember result = %#v, want error", deniedExternalExisting)
	}
	externalExistingExtensions := deniedExternalExisting["errors"].([]any)[0].(map[string]any)["extensions"].(map[string]any)
	if externalExistingExtensions["type"] != "no_permission" || externalExistingExtensions["name"] != "space_access_denied" {
		t.Fatalf("unauthorized External existing-target error = %#v", externalExistingExtensions)
	}
	variables["userId"] = "missing-external-target"
	deniedExternalMissing := graphRequest(t, handler, query, variables, fixture.sessionID, fixture.userID)
	if deniedExternalMissing["errors"] == nil {
		t.Fatalf("unlicensed admin External grantMember missing-target result = %#v, want error", deniedExternalMissing)
	}
	externalMissingExtensions := deniedExternalMissing["errors"].([]any)[0].(map[string]any)["extensions"].(map[string]any)
	if !reflect.DeepEqual(externalMissingExtensions, externalExistingExtensions) {
		t.Fatalf("unauthorized External target existence leaked: missing=%#v existing=%#v", externalMissingExtensions, externalExistingExtensions)
	}

	variables["workspaceId"] = "missing-workspace"
	missingWorkspace := graphRequest(t, handler, query, variables, fixture.sessionID, fixture.userID)
	if missingWorkspace["errors"] == nil {
		t.Fatalf("missing workspace grantMember result = %#v, want error", missingWorkspace)
	}
	workspaceExtensions := missingWorkspace["errors"].([]any)[0].(map[string]any)["extensions"].(map[string]any)
	if workspaceExtensions["type"] != "resource_not_found" || workspaceExtensions["name"] != "space_not_found" {
		t.Fatalf("missing workspace error contract = %#v", workspaceExtensions)
	}
}
