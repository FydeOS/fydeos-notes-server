// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"errors"
	"testing"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/notifications"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

type revokeMemberMailerFake struct {
	userIDs      []string
	workspaceIDs []string
}

func (f *revokeMemberMailerFake) QueueMemberRemoved(_ context.Context, userID, workspaceID string) error {
	f.userIDs = append(f.userIDs, userID)
	f.workspaceIDs = append(f.workspaceIDs, workspaceID)
	return nil
}

type revokeMemberNotificationsFake struct {
	invitations []notifications.InvitationNotification
	unreadCount int
	countErr    error
}

func (f *revokeMemberNotificationsFake) CreateInvitation(_ context.Context, input notifications.InvitationNotification) (string, bool, error) {
	f.invitations = append(f.invitations, input)
	return "notification-1", true, nil
}

func (f *revokeMemberNotificationsFake) CountUnread(context.Context, string) (int, error) {
	return f.unreadCount, f.countErr
}

func TestRevokeMemberGraphQLCompatibility(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	if _, err := pool.Exec(ctx, `UPDATE workspace_members SET role='member' WHERE workspace_id=$1 AND user_id=$2`, fixture.workspaceID, fixture.sharedUserID); err != nil {
		t.Fatal(err)
	}
	repository := workspaces.NewRepositoryWithDeployment(pool, "cloud")
	mailer := &revokeMemberMailerFake{}
	invitationMailer := &invitationApprovalMailerFake{}
	notifications := &revokeMemberNotificationsFake{unreadCount: 4}
	countPublisher := &workspaceRoleNotificationCountPublisherFake{}
	publisher := &workspaceRoleMutationPublisherFake{}
	handler := NewHandlerWithServices(
		config.Config{Version: "0.27.5", Deployment: "cloud", Host: "localhost", Port: 3010},
		auth.NewSessionResolver(pool), nil, repository, nil, nil, nil, nil,
		HandlerDependencies{WorkspaceRoles: WorkspaceRoleMutationDependencies{
			Repository: repository, Notifications: notifications, InvitationMailer: invitationMailer, RemovalMailer: mailer,
			CountPublisher: countPublisher, Publisher: publisher,
		}},
	)
	query := `mutation revokeMember($userId: String!, $workspaceId: String!) {
		revokeMember(userId: $userId, workspaceId: $workspaceId)
	}`
	variables := map[string]any{"userId": fixture.sharedUserID, "workspaceId": fixture.workspaceID}
	result := graphRequest(t, handler, query, variables, fixture.sessionID, fixture.userID)
	if result["errors"] != nil || result["data"].(map[string]any)["revokeMember"] != true {
		t.Fatalf("active member revoke result = %#v", result)
	}
	var activeRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspace_members WHERE workspace_id=$1 AND user_id=$2 AND state='active'`, fixture.workspaceID, fixture.sharedUserID).Scan(&activeRows); err != nil || activeRows != 0 {
		t.Fatalf("active membership rows=%d, err=%v", activeRows, err)
	}
	if len(mailer.userIDs) != 1 || mailer.userIDs[0] != fixture.sharedUserID || mailer.workspaceIDs[0] != fixture.workspaceID {
		t.Fatalf("member-removal mail calls = %#v / %#v", mailer.userIDs, mailer.workspaceIDs)
	}
	if len(publisher.workspaceIDs) != 1 || publisher.workspaceIDs[0] != fixture.workspaceID {
		t.Fatalf("active removal workspace events = %#v", publisher.workspaceIDs)
	}

	if _, err := pool.Exec(ctx, `INSERT INTO workspace_invitations(id,workspace_id,invitee_user_id,inviter_user_id,requested_role,status,kind)
		VALUES('review-request',$1,$2,$3,'member','waiting_review','email')`, fixture.workspaceID, fixture.disabledUserID, fixture.userID); err != nil {
		t.Fatal(err)
	}
	variables["userId"] = fixture.disabledUserID
	result = graphRequest(t, handler, query, variables, fixture.sessionID, fixture.userID)
	if result["errors"] != nil || result["data"].(map[string]any)["revokeMember"] != true {
		t.Fatalf("review request revoke result = %#v", result)
	}
	var remainingInvitations int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspace_invitations WHERE id='review-request'`).Scan(&remainingInvitations); err != nil || remainingInvitations != 0 {
		t.Fatalf("review requests remaining=%d, err=%v", remainingInvitations, err)
	}
	if len(notifications.invitations) != 1 {
		t.Fatalf("review-declined notifications = %#v", notifications.invitations)
	}
	if len(invitationMailer.calls) != 1 || invitationMailer.calls[0].Type != "declined" || invitationMailer.calls[0].InviteeID != fixture.disabledUserID || invitationMailer.calls[0].ActorID != fixture.userID || invitationMailer.calls[0].WorkspaceID != fixture.workspaceID || invitationMailer.calls[0].NotificationID == "" {
		t.Fatalf("review-declined mail calls = %#v", invitationMailer.calls)
	}
	reviewDeclined := notifications.invitations[0]
	if reviewDeclined.UserID != fixture.disabledUserID || reviewDeclined.WorkspaceID != fixture.workspaceID || reviewDeclined.CreatedByUserID != fixture.userID || reviewDeclined.Type != "InvitationReviewDeclined" {
		t.Fatalf("review-declined notification = %+v", reviewDeclined)
	}
	if len(countPublisher.userIDs) != 1 || countPublisher.userIDs[0] != fixture.disabledUserID || countPublisher.counts[0] != notifications.unreadCount || countPublisher.reasons[0] != "created" {
		t.Fatalf("notification count events = %+v", countPublisher)
	}
	if len(mailer.userIDs) != 1 || len(publisher.workspaceIDs) != 2 {
		t.Fatalf("review removal mail/events = %#v / %#v", mailer.userIDs, publisher.workspaceIDs)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_invitations(id,workspace_id,invitee_user_id,inviter_user_id,requested_role,status,kind)
		VALUES('review-request-count-failure',$1,$2,$3,'member','waiting_review','email')`, fixture.workspaceID, fixture.disabledUserID, fixture.userID); err != nil {
		t.Fatal(err)
	}
	notifications.countErr = errors.New("count query unavailable")
	result = graphRequest(t, handler, query, variables, fixture.sessionID, fixture.userID)
	if result["errors"] != nil || result["data"].(map[string]any)["revokeMember"] != true {
		t.Fatalf("review revoke with unread-count failure = %#v", result)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspace_invitations WHERE id='review-request-count-failure'`).Scan(&remainingInvitations); err != nil || remainingInvitations != 0 {
		t.Fatalf("review request after unread-count failure remaining=%d, err=%v", remainingInvitations, err)
	}
	if len(publisher.workspaceIDs) != 3 || len(countPublisher.userIDs) != 1 {
		t.Fatalf("unread-count failure interrupted member update: workspace=%#v count=%+v", publisher.workspaceIDs, countPublisher)
	}

	variables["userId"] = fixture.userID
	selfRevoke := graphRequest(t, handler, query, variables, fixture.sessionID, fixture.userID)
	if selfRevoke["errors"] == nil {
		t.Fatalf("self-revoke result = %#v, want error", selfRevoke)
	}
	selfError := selfRevoke["errors"].([]any)[0].(map[string]any)
	selfExtensions := selfError["extensions"].(map[string]any)
	if selfExtensions["type"] != "action_forbidden" || selfExtensions["name"] != "can_not_revoke_yourself" {
		t.Fatalf("self-revoke error = %#v", selfExtensions)
	}

	variables["userId"] = "missing-member"
	missing := graphRequest(t, handler, query, variables, fixture.sessionID, fixture.userID)
	if missing["errors"] == nil {
		t.Fatalf("missing-member result = %#v, want error", missing)
	}
	missingError := missing["errors"].([]any)[0].(map[string]any)
	missingExtensions := missingError["extensions"].(map[string]any)
	missingData := missingExtensions["data"].(map[string]any)
	if missingExtensions["type"] != "action_forbidden" || missingExtensions["name"] != "member_not_found_in_space" || missingData["spaceId"] != fixture.workspaceID {
		t.Fatalf("missing-member error = %#v", missingExtensions)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO workspace_members(id,workspace_id,user_id,role,state) VALUES
		('revoke-actor',$1,$2,'member','active'),('revoke-target',$1,$3,'member','active')`, fixture.workspaceID, fixture.sharedUserID, fixture.disabledUserID); err != nil {
		t.Fatal(err)
	}
	variables["userId"] = fixture.disabledUserID
	denied := graphRequest(t, handler, query, variables, fixture.sessionID, fixture.sharedUserID)
	if denied["errors"] == nil {
		t.Fatalf("member revoke result = %#v, want permission error", denied)
	}
	deniedExtensions := denied["errors"].([]any)[0].(map[string]any)["extensions"].(map[string]any)
	deniedData := deniedExtensions["data"].(map[string]any)
	if deniedExtensions["type"] != "no_permission" || deniedExtensions["name"] != "space_access_denied" || deniedData["spaceId"] != fixture.workspaceID {
		t.Fatalf("member revoke permission error = %#v", deniedExtensions)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspace_members WHERE workspace_id=$1 AND user_id=$2 AND state='active'`, fixture.workspaceID, fixture.disabledUserID).Scan(&activeRows); err != nil || activeRows != 1 {
		t.Fatalf("unauthorized revoke left active rows=%d, err=%v", activeRows, err)
	}
	if len(mailer.userIDs) != 1 || len(publisher.workspaceIDs) != 3 {
		t.Fatalf("rejected revoke emitted side effects: mail=%#v events=%#v", mailer.userIDs, publisher.workspaceIDs)
	}
}
