// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/notifications"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
	"github.com/google/uuid"
)

type invitationApprovalMailCall struct {
	Type           string
	InviteeID      string
	ActorID        string
	WorkspaceID    string
	NotificationID string
}

type invitationApprovalMailerFake struct {
	calls []invitationApprovalMailCall
	err   error
}

func (f *invitationApprovalMailerFake) QueueInvitationReviewApproved(_ context.Context, inviteeID, actorID, workspaceID, notificationID string) error {
	f.calls = append(f.calls, invitationApprovalMailCall{Type: "approved", InviteeID: inviteeID, ActorID: actorID, WorkspaceID: workspaceID, NotificationID: notificationID})
	return f.err
}

func (f *invitationApprovalMailerFake) QueueInvitationReviewRequest(_ context.Context, reviewerID, inviteeID, workspaceID, notificationID string) error {
	f.calls = append(f.calls, invitationApprovalMailCall{Type: "request", InviteeID: inviteeID, ActorID: reviewerID, WorkspaceID: workspaceID, NotificationID: notificationID})
	return f.err
}

func (f *invitationApprovalMailerFake) QueueInvitationReviewDeclined(_ context.Context, inviteeID, actorID, workspaceID, notificationID string) error {
	f.calls = append(f.calls, invitationApprovalMailCall{Type: "declined", InviteeID: inviteeID, ActorID: actorID, WorkspaceID: workspaceID, NotificationID: notificationID})
	return f.err
}

func TestApproveMemberGraphQLCompatibility(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	for _, statement := range []string{
		`CREATE TYPE "NotificationLevel" AS ENUM ('High','Default','Low','Min','None')`,
		`CREATE TYPE "NotificationType" AS ENUM ('Mention','Invitation','InvitationAccepted','InvitationBlocked','InvitationRejected','InvitationReviewRequest','InvitationReviewApproved','InvitationReviewDeclined','Comment','CommentMention')`,
		`CREATE TABLE notifications (id text PRIMARY KEY, user_id text NOT NULL, created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(), level "NotificationLevel" NOT NULL, read boolean NOT NULL DEFAULT false, type "NotificationType" NOT NULL, body jsonb NOT NULL)`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("create approval notification fixture: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE workspaces SET name='Review Team' WHERE id=$1`, fixture.workspaceID); err != nil {
		t.Fatal(err)
	}

	approvedID := newInvitationTestUser(t, ctx, pool, fixture, uuid.NewString())
	pendingID := newInvitationTestUser(t, ctx, pool, fixture, uuid.NewString())
	for _, invite := range []struct {
		id, inviteeID, status string
	}{
		{"approval-invite", approvedID, "waiting_review"},
	} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspace_invitations(id,workspace_id,invitee_user_id,inviter_user_id,requested_role,status,kind)
			VALUES($1,$2,$3,$4,'member',$5,'link')`, invite.id, fixture.workspaceID, invite.inviteeID, fixture.userID, invite.status); err != nil {
			t.Fatalf("insert invitation %s: %v", invite.id, err)
		}
	}

	repository := workspaces.NewRepositoryWithDeployment(pool, "cloud")
	notificationRepository := notifications.NewRepository(pool, "https://notes.example.test")
	membersPublisher := &invitationAcceptancePublisherFake{}
	countPublisher := &invitationAcceptanceCountPublisherFake{}
	mailer := &invitationApprovalMailerFake{}
	handler := NewHandlerWithServices(
		config.Config{Version: "0.27.5", Deployment: "cloud", Host: "localhost", Port: 3010},
		auth.NewSessionResolver(pool), nil, repository, nil, nil, nil, nil,
		HandlerDependencies{InvitationAcceptance: WorkspaceInvitationAcceptanceDependencies{
			Repository: repository, Notifications: notificationRepository, InvitationMailer: mailer,
			CountPublisher: countPublisher, MembersPublisher: membersPublisher,
		}},
	)
	mutation := `mutation ApproveMember($userId: String!, $workspaceId: String!) {
		approveMember(userId: $userId, workspaceId: $workspaceId)
	}`
	variables := map[string]any{"userId": approvedID, "workspaceId": fixture.workspaceID}

	result := graphRequest(t, handler, mutation, variables, fixture.sessionID, fixture.userID)
	if result["errors"] != nil || result["data"].(map[string]any)["approveMember"] != true {
		t.Fatalf("approveMember result = %#v", result)
	}
	var memberID, memberRole, memberSource, memberState string
	if err := pool.QueryRow(ctx, `SELECT id,role,source,state FROM workspace_members WHERE workspace_id=$1 AND user_id=$2`, fixture.workspaceID, approvedID).
		Scan(&memberID, &memberRole, &memberSource, &memberState); err != nil {
		t.Fatal(err)
	}
	if memberID != "approval-invite" || memberRole != "member" || memberSource != "link" || memberState != "active" {
		t.Fatalf("approved membership = %q/%q/%q/%q", memberID, memberRole, memberSource, memberState)
	}
	var invitationCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspace_invitations WHERE id='approval-invite'`).Scan(&invitationCount); err != nil || invitationCount != 0 {
		t.Fatalf("approved invitation rows = %d, %v", invitationCount, err)
	}
	assertInvitationNotification(t, ctx, pool, approvedID, "InvitationReviewApproved", fixture.userID, "approval-invite")
	if len(mailer.calls) != 1 || mailer.calls[0].Type != "approved" || mailer.calls[0].InviteeID != approvedID || mailer.calls[0].ActorID != fixture.userID || mailer.calls[0].WorkspaceID != fixture.workspaceID || mailer.calls[0].NotificationID == "" {
		t.Fatalf("approval mail calls = %#v", mailer.calls)
	}
	if len(membersPublisher.workspaceIDs) != 1 || membersPublisher.workspaceIDs[0] != fixture.workspaceID {
		t.Fatalf("workspace member events = %#v", membersPublisher.workspaceIDs)
	}
	if len(countPublisher.userIDs) != 1 || countPublisher.userIDs[0] != approvedID || countPublisher.counts[0] != 1 || countPublisher.reasons[0] != "created" {
		t.Fatalf("notification count events = %#v", countPublisher)
	}

	if _, err := pool.Exec(ctx, `INSERT INTO workspace_invitations(id,workspace_id,invitee_user_id,inviter_user_id,requested_role,status,kind)
		VALUES('pending-invite',$1,$2,$3,'member','pending','link')`, fixture.workspaceID, pendingID, fixture.userID); err != nil {
		t.Fatal(err)
	}
	variables["userId"] = pendingID
	result = graphRequest(t, handler, mutation, variables, fixture.sessionID, fixture.userID)
	if result["errors"] != nil || result["data"].(map[string]any)["approveMember"] != true {
		t.Fatalf("non-review approveMember result = %#v", result)
	}
	if len(mailer.calls) != 1 || len(membersPublisher.workspaceIDs) != 1 || len(countPublisher.userIDs) != 1 {
		t.Fatalf("non-review invitation produced side effects: mail=%#v members=%#v counts=%#v", mailer.calls, membersPublisher.workspaceIDs, countPublisher.userIDs)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspace_invitations WHERE id='pending-invite'`).Scan(&invitationCount); err != nil || invitationCount != 1 {
		t.Fatalf("non-review invitation rows = %d, %v", invitationCount, err)
	}

	forbiddenID := newInvitationTestUser(t, ctx, pool, fixture, uuid.NewString())
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_invitations(id,workspace_id,invitee_user_id,requested_role,status,kind)
		VALUES('forbidden-review',$1,$2,'member','waiting_review','link')`, fixture.workspaceID, forbiddenID); err != nil {
		t.Fatal(err)
	}
	variables["userId"] = forbiddenID
	result = graphRequest(t, handler, mutation, variables, fixture.sessionID, fixture.sharedUserID)
	if result["errors"] == nil {
		t.Fatalf("non-manager approval result = %#v, want error", result)
	}
	if len(mailer.calls) != 1 || len(membersPublisher.workspaceIDs) != 1 || len(countPublisher.userIDs) != 1 {
		t.Fatalf("denied approval produced side effects: mail=%#v members=%#v counts=%#v", mailer.calls, membersPublisher.workspaceIDs, countPublisher.userIDs)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspace_invitations WHERE id='forbidden-review'`).Scan(&invitationCount); err != nil || invitationCount != 1 {
		t.Fatalf("denied invitation rows = %d, %v", invitationCount, err)
	}

	variables["userId"] = "missing-user"
	result = graphRequest(t, handler, mutation, variables, fixture.sessionID, fixture.userID)
	if graphErrorName(result) != "member_not_found_in_space" || graphErrorData(result)["spaceId"] != fixture.workspaceID {
		t.Fatalf("missing member error = %#v", result["errors"])
	}

	seatLimitedID := newInvitationTestUser(t, ctx, pool, fixture, uuid.NewString())
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_invitations(id,workspace_id,invitee_user_id,requested_role,status,kind)
		VALUES('seat-limited-review',$1,$2,'member','waiting_review','link')`, fixture.workspaceID, seatLimitedID); err != nil {
		t.Fatal(err)
	}
	variables["userId"] = seatLimitedID
	result = graphRequest(t, handler, mutation, variables, fixture.sessionID, fixture.userID)
	if graphErrorName(result) != "NO_MORE_SEAT" || graphErrorData(result)["spaceId"] != fixture.workspaceID {
		t.Fatalf("seat-limited approval error = %#v", result["errors"])
	}
	if len(mailer.calls) != 1 || len(membersPublisher.workspaceIDs) != 1 || len(countPublisher.userIDs) != 1 {
		t.Fatalf("seat-limited approval produced side effects: mail=%#v members=%#v counts=%#v", mailer.calls, membersPublisher.workspaceIDs, countPublisher.userIDs)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspace_invitations WHERE id='seat-limited-review'`).Scan(&invitationCount); err != nil || invitationCount != 1 {
		t.Fatalf("seat-limited invitation rows = %d, %v", invitationCount, err)
	}
	var notificationBody []byte
	if err := pool.QueryRow(ctx, `SELECT body FROM notifications WHERE user_id=$1 AND type='InvitationReviewApproved'`, approvedID).Scan(&notificationBody); err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(notificationBody, &body); err != nil || body["inviteId"] != "approval-invite" {
		t.Fatalf("approved notification body = %#v, %v", body, err)
	}
}

var _ WorkspaceInvitationMailer = (*invitationApprovalMailerFake)(nil)
