// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/notifications"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
	"github.com/jackc/pgx/v5"
)

func TestInviteMembersGraphQLPersistsPendingEmailInvitation(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	if _, err := pool.Exec(ctx, `ALTER TABLE workspace_invitations ADD COLUMN normalized_email text; ALTER TABLE users ADD COLUMN registered boolean NOT NULL DEFAULT true`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `CREATE TYPE "NotificationLevel" AS ENUM ('High','Default','Low','Min','None'); CREATE TYPE "NotificationType" AS ENUM ('Mention','Invitation','InvitationAccepted','InvitationBlocked','InvitationRejected','InvitationReviewRequest','InvitationReviewApproved','InvitationReviewDeclined','Comment','CommentMention'); CREATE TABLE notifications (id text PRIMARY KEY, user_id text NOT NULL, created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(), level "NotificationLevel" NOT NULL, read boolean NOT NULL DEFAULT false, type "NotificationType" NOT NULL, body jsonb NOT NULL)`); err != nil {
		t.Fatal(err)
	}

	repository := workspaces.NewRepositoryWithDeployment(pool, "selfhosted")
	publisher := &inviteMembersPublisherFake{}
	countPublisher := &inviteMembersCountPublisherFake{}
	quota := &inviteMembersQuotaFake{}
	notificationRepository := notifications.NewRepository(pool, "https://notes.example.test")
	handler := NewHandlerWithServices(
		config.Config{Version: "0.27.5", Deployment: "selfhosted", Host: "localhost", Port: 3010},
		auth.NewSessionResolver(pool), nil, repository, nil, nil, nil, nil,
		HandlerDependencies{WorkspaceInvitations: WorkspaceInviteMutationDependencies{
			Repository:     repository,
			Admission:      quota,
			Notifications:  notificationRepository,
			CountPublisher: countPublisher,
			Publisher:      publisher,
		}},
	)
	mutation := `mutation InviteMembers($workspaceId: String!, $emails: [String!]!) {
		inviteMembers(workspaceId: $workspaceId, emails: $emails) { email inviteId error }
	}`
	variables := map[string]any{
		"workspaceId": fixture.workspaceID,
		"emails":      []string{"not-an-email", fixture.hiddenEmail, " HIDDEN@EXAMPLE.TEST ", fixture.sharedEmail},
	}
	result := graphRequest(t, handler, mutation, variables, fixture.sessionID, fixture.userID)
	var response struct {
		Data struct {
			InviteMembers []struct {
				Email    string          `json:"email"`
				InviteID string          `json:"inviteId"`
				Error    json.RawMessage `json:"error"`
			} `json:"inviteMembers"`
		} `json:"data"`
		Errors []map[string]any `json:"errors"`
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Errors) != 0 {
		t.Fatalf("inviteMembers GraphQL errors = %#v", response.Errors)
	}
	if len(response.Data.InviteMembers) != 4 {
		t.Fatalf("inviteMembers result = %#v", response.Data.InviteMembers)
	}
	invitation := response.Data.InviteMembers[1]
	if invitation.Email != fixture.hiddenEmail || invitation.InviteID == "" || string(invitation.Error) != "null" {
		t.Fatalf("successful inviteMembers item = %#v", invitation)
	}
	for index, wantName := range map[int]string{0: "INVALID_EMAIL", 2: "ACTION_FORBIDDEN", 3: "ALREADY_IN_SPACE"} {
		var itemError map[string]any
		if err := json.Unmarshal(response.Data.InviteMembers[index].Error, &itemError); err != nil {
			t.Fatalf("decode item %d error %q: %v", index, response.Data.InviteMembers[index].Error, err)
		}
		if itemError["name"] != wantName {
			t.Errorf("inviteMembers item %d error = %#v, want %s", index, itemError, wantName)
		}
	}
	if quota.admitCalls != 1 || quota.admittedCount != 1 || len(quota.admittedDomains) != 1 || quota.admittedDomains[0] != (WorkspaceInviteQuotaTargetDomain{Domain: "example.test", Count: 1}) {
		t.Fatalf("quota admission = calls %d count %d domains %#v", quota.admitCalls, quota.admittedCount, quota.admittedDomains)
	}
	if quota.commitCalls != 1 || quota.committedCount != 1 || quota.releaseCalls != 0 {
		t.Fatalf("quota settlement = commit %d/%d release %d", quota.commitCalls, quota.committedCount, quota.releaseCalls)
	}
	var inviteeID, inviterID, normalizedEmail, status, kind string
	if err := pool.QueryRow(ctx, `SELECT invitee_user_id, inviter_user_id, normalized_email, status, kind
		FROM workspace_invitations WHERE id=$1`, invitation.InviteID).
		Scan(&inviteeID, &inviterID, &normalizedEmail, &status, &kind); err != nil {
		t.Fatal(err)
	}
	if inviteeID == "" || inviterID != fixture.userID || normalizedEmail != fixture.hiddenEmail || status != "pending" || kind != "email" {
		t.Fatalf("persisted invitation = invitee %q inviter %q email %q status %q kind %q", inviteeID, inviterID, normalizedEmail, status, kind)
	}
	if len(publisher.workspaceIDs) != 1 || publisher.workspaceIDs[0] != fixture.workspaceID {
		t.Fatalf("member update publications = %#v", publisher.workspaceIDs)
	}
	var notificationType string
	var notificationBody []byte
	if err := pool.QueryRow(ctx, `SELECT type::text,body FROM notifications WHERE user_id=$1`, inviteeID).Scan(&notificationType, &notificationBody); err != nil {
		t.Fatal(err)
	}
	var notificationPayload map[string]any
	if err := json.Unmarshal(notificationBody, &notificationPayload); err != nil {
		t.Fatal(err)
	}
	if notificationType != "Invitation" || notificationPayload["createdByUserId"] != fixture.userID || notificationPayload["workspaceId"] != fixture.workspaceID || notificationPayload["inviteId"] != invitation.InviteID {
		t.Fatalf("invitation notification = %q %#v", notificationType, notificationPayload)
	}
	if len(countPublisher.userIDs) != 1 || countPublisher.userIDs[0] != inviteeID || countPublisher.counts[0] != 1 || countPublisher.reasons[0] != "created" {
		t.Fatalf("notification count publications = %#v", countPublisher)
	}

	denied := graphRequest(t, handler, mutation, variables, fixture.sessionID, fixture.sharedUserID)
	if graphErrorName(denied) != "SPACE_ACCESS_DENIED" {
		t.Fatalf("non-manager invite error = %#v", denied["errors"])
	}
	var invitations int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspace_invitations WHERE workspace_id=$1`, fixture.workspaceID).Scan(&invitations); err != nil {
		t.Fatal(err)
	}
	if invitations != 1 || len(publisher.workspaceIDs) != 1 {
		t.Fatalf("denied invitation caused side effects: invitations=%d publications=%#v", invitations, publisher.workspaceIDs)
	}
	var notificationCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM notifications`).Scan(&notificationCount); err != nil {
		t.Fatal(err)
	}
	if notificationCount != 1 || len(countPublisher.userIDs) != 1 {
		t.Fatalf("denied invitation changed notifications: rows=%d count publications=%#v", notificationCount, countPublisher)
	}
}

func TestInviteMembersGraphQLReleasesQuotaWhenWorkspaceHasNoSeats(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	if _, err := pool.Exec(ctx, `ALTER TABLE workspace_invitations ADD COLUMN normalized_email text; ALTER TABLE users ADD COLUMN registered boolean NOT NULL DEFAULT true`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO entitlements(target_type,target_id,source,plan,quantity,status) VALUES('workspace',$1,'cloud_subscription','team',2,'active')`, fixture.workspaceID); err != nil {
		t.Fatalf("limit workspace to existing members: %v", err)
	}

	repository := workspaces.NewRepositoryWithDeployment(pool, "cloud")
	publisher := &inviteMembersPublisherFake{}
	quota := &inviteMembersQuotaFake{}
	handler := NewHandlerWithServices(
		config.Config{Version: "0.27.5", Deployment: "selfhosted", Host: "localhost", Port: 3010},
		auth.NewSessionResolver(pool), nil, repository, nil, nil, nil, nil,
		HandlerDependencies{WorkspaceInvitations: WorkspaceInviteMutationDependencies{
			Repository: repository,
			Admission:  quota,
			Publisher:  publisher,
		}},
	)
	mutation := `mutation InviteMembers($workspaceId: String!, $emails: [String!]!) {
		inviteMembers(workspaceId: $workspaceId, emails: $emails) { email inviteId error }
	}`
	result := graphRequest(t, handler, mutation, map[string]any{
		"workspaceId": fixture.workspaceID,
		"emails":      []string{"new-seat@example.test"},
	}, fixture.sessionID, fixture.userID)
	if graphErrorName(result) != "NO_MORE_SEAT" {
		t.Fatalf("full workspace seat error = %#v", result["errors"])
	}
	if quota.admitCalls != 1 || quota.commitCalls != 0 || quota.releaseCalls != 1 {
		t.Fatalf("quota settlement on seat exhaustion = admit %d commit %d release %d", quota.admitCalls, quota.commitCalls, quota.releaseCalls)
	}
	var invitations int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspace_invitations WHERE workspace_id=$1 AND kind='email'`, fixture.workspaceID).Scan(&invitations); err != nil {
		t.Fatal(err)
	}
	if invitations != 0 || len(publisher.workspaceIDs) != 0 {
		t.Fatalf("seat rejection side effects = invitations %d publications %#v", invitations, publisher.workspaceIDs)
	}
}

func TestInviteMembersRollsBackInvitationsWhenQuotaSettlementFails(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	if _, err := pool.Exec(ctx, `
		ALTER TABLE workspace_invitations ADD COLUMN normalized_email text;
		ALTER TABLE users ADD COLUMN registered boolean NOT NULL DEFAULT true;
		CREATE TABLE quota_settlement_markers (id integer PRIMARY KEY);`); err != nil {
		t.Fatal(err)
	}

	repository := workspaces.NewRepositoryWithDeployment(pool, "selfhosted")
	quota := &inviteMembersQuotaFake{settlementErr: errors.New("quota settlement failed"), markBeforeError: true}
	publisher := &inviteMembersPublisherFake{}
	handler := NewHandlerWithServices(
		config.Config{Version: "0.27.5", Deployment: "selfhosted", Host: "localhost", Port: 3010},
		auth.NewSessionResolver(pool), nil, repository, nil, nil, nil, nil,
		HandlerDependencies{WorkspaceInvitations: WorkspaceInviteMutationDependencies{
			Repository: repository,
			Admission:  quota,
			Publisher:  publisher,
		}},
	)
	mutation := `mutation InviteMembers($workspaceId: String!, $emails: [String!]!) {
		inviteMembers(workspaceId: $workspaceId, emails: $emails) { email inviteId error }
	}`
	result := graphRequest(t, handler, mutation, map[string]any{
		"workspaceId": fixture.workspaceID,
		"emails":      []string{"atomic-target@example.test"},
	}, fixture.sessionID, fixture.userID)
	if len(result["errors"].([]any)) == 0 {
		t.Fatalf("quota settlement failure response = %#v", result)
	}
	var invitations, users, markers int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspace_invitations WHERE normalized_email='atomic-target@example.test'`).Scan(&invitations); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE lower(email)='atomic-target@example.test'`).Scan(&users); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM quota_settlement_markers`).Scan(&markers); err != nil {
		t.Fatal(err)
	}
	if invitations != 0 || users != 0 || markers != 0 || quota.commitCalls != 1 || quota.releaseCalls != 1 || len(publisher.workspaceIDs) != 0 {
		t.Fatalf("failed settlement left invitations=%d users=%d markers=%d; quota commits/releases=%d/%d publications=%#v", invitations, users, markers, quota.commitCalls, quota.releaseCalls, publisher.workspaceIDs)
	}
}

type inviteMembersQuotaFake struct {
	admitCalls       int
	admittedCount    int
	admittedDomains  []WorkspaceInviteQuotaTargetDomain
	commitCalls      int
	committedCount   int
	committedDomains []WorkspaceInviteQuotaTargetDomain
	releaseCalls     int
	settlementErr    error
	markBeforeError  bool
}

func (f *inviteMembersQuotaFake) AdmitWorkspaceInvites(_ context.Context, _, _ string, count int, domains []WorkspaceInviteQuotaTargetDomain) (string, error) {
	f.admitCalls++
	f.admittedCount = count
	f.admittedDomains = append([]WorkspaceInviteQuotaTargetDomain(nil), domains...)
	return "invite-reservation", nil
}

func (f *inviteMembersQuotaFake) CommitWorkspaceInviteQuotaTx(ctx context.Context, tx pgx.Tx, _ string, count int, domains []WorkspaceInviteQuotaTargetDomain) error {
	f.commitCalls++
	f.committedCount = count
	f.committedDomains = append([]WorkspaceInviteQuotaTargetDomain(nil), domains...)
	if f.settlementErr != nil {
		if f.markBeforeError {
			if _, err := tx.Exec(ctx, `INSERT INTO quota_settlement_markers(id) VALUES(1)`); err != nil {
				return err
			}
		}
		return f.settlementErr
	}
	return nil
}

func (f *inviteMembersQuotaFake) ReleaseWorkspaceInviteQuota(context.Context, string) error {
	f.releaseCalls++
	return nil
}

type inviteMembersPublisherFake struct {
	workspaceIDs []string
}

type inviteMembersCountPublisherFake struct {
	userIDs []string
	counts  []int
	reasons []string
}

func (f *inviteMembersCountPublisherFake) PublishNotificationCountChanged(userID string, count int, reason string) {
	f.userIDs = append(f.userIDs, userID)
	f.counts = append(f.counts, count)
	f.reasons = append(f.reasons, reason)
}

func (f *inviteMembersPublisherFake) PublishWorkspaceMembersUpdated(workspaceID string) {
	f.workspaceIDs = append(f.workspaceIDs, workspaceID)
}
