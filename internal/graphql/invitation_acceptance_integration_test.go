// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/invitelinks"
	"github.com/fydeos/fydeos-notes-server/internal/notifications"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type invitationAcceptanceLinkLookupFake struct {
	targets map[string]*invitelinks.InvitationTarget
}

func (f invitationAcceptanceLinkLookupFake) LookupByID(_ context.Context, inviteID string) (*invitelinks.InvitationTarget, error) {
	return f.targets[inviteID], nil
}

type invitationAcceptancePublisherFake struct {
	workspaceIDs []string
}

func (f *invitationAcceptancePublisherFake) PublishWorkspaceMembersUpdated(workspaceID string) {
	f.workspaceIDs = append(f.workspaceIDs, workspaceID)
}

type invitationAcceptanceCountPublisherFake struct {
	userIDs []string
	counts  []int
	reasons []string
}

func (f *invitationAcceptanceCountPublisherFake) PublishNotificationCountChanged(userID string, count int, reason string) {
	f.userIDs = append(f.userIDs, userID)
	f.counts = append(f.counts, count)
	f.reasons = append(f.reasons, reason)
}

func TestAcceptInviteByIDGraphQLCompatibility(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	for _, statement := range []string{
		`CREATE TYPE "NotificationLevel" AS ENUM ('High','Default','Low','Min','None')`,
		`CREATE TYPE "NotificationType" AS ENUM ('Mention','Invitation','InvitationAccepted','InvitationBlocked','InvitationRejected','InvitationReviewRequest','InvitationReviewApproved','InvitationReviewDeclined','Comment','CommentMention')`,
		`CREATE TABLE notifications (id text PRIMARY KEY, user_id text NOT NULL, created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(), level "NotificationLevel" NOT NULL, read boolean NOT NULL DEFAULT false, type "NotificationType" NOT NULL, body jsonb NOT NULL)`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("create notification fixture: %v", err)
		}
	}
	var emailInviteeID string
	if err := pool.QueryRow(ctx, `SELECT id FROM users WHERE email=$1`, fixture.hiddenEmail).Scan(&emailInviteeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO user_sessions(id,session_id,user_id,created_at) VALUES ($1,$2,$3,now())`, emailInviteeID+"-session", fixture.sessionID, emailInviteeID); err != nil {
		t.Fatalf("insert email invitee session: %v", err)
	}
	linkUserID, pendingUserID, mismatchUserID := newInvitationTestUser(t, ctx, pool, fixture, uuid.NewString()), newInvitationTestUser(t, ctx, pool, fixture, uuid.NewString()), newInvitationTestUser(t, ctx, pool, fixture, uuid.NewString())
	for _, invite := range []struct {
		id, userID, status string
	}{
		{"email-invite", emailInviteeID, "pending"},
		{"pending-email-invite", pendingUserID, "pending"},
		{"mismatch-invite", mismatchUserID, "pending"},
	} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspace_invitations(id,workspace_id,invitee_user_id,inviter_user_id,requested_role,status,kind) VALUES ($1,$2,$3,$4,'member',$5,'email')`, invite.id, fixture.workspaceID, invite.userID, fixture.sharedUserID, invite.status); err != nil {
			t.Fatalf("insert email invitation: %v", err)
		}
	}

	workspaceRepository := workspaces.NewRepositoryWithDeployment(pool, "selfhosted")
	notificationRepository := notifications.NewRepository(pool, "https://notes.example.test")
	membersPublisher := &invitationAcceptancePublisherFake{}
	countPublisher := &invitationAcceptanceCountPublisherFake{}
	mailer := &invitationApprovalMailerFake{}
	linkLookup := invitationAcceptanceLinkLookupFake{targets: map[string]*invitelinks.InvitationTarget{
		"link-invite":  {WorkspaceID: fixture.workspaceID, InviterID: fixture.userID},
		"pending-link": {WorkspaceID: fixture.workspaceID, InviterID: fixture.userID},
		"unknown-link": nil,
	}}
	handler := NewHandlerWithServices(
		config.Config{Version: "0.27.5", Deployment: "selfhosted", Host: "localhost", Port: 3010},
		auth.NewSessionResolver(pool), nil, workspaceRepository, nil, nil, nil, nil,
		HandlerDependencies{InvitationAcceptance: WorkspaceInvitationAcceptanceDependencies{
			Repository: workspaceRepository, LinkLookup: linkLookup, Notifications: notificationRepository,
			InvitationMailer: mailer, CountPublisher: countPublisher, MembersPublisher: membersPublisher,
		}},
	)
	mutation := `mutation AcceptInvite($inviteId: String!) { acceptInviteById(inviteId: $inviteId) }`

	emailResult := graphRequest(t, handler, mutation, map[string]any{"inviteId": "email-invite"}, fixture.sessionID, emailInviteeID)
	assertMutationTrue(t, emailResult, "email invitation")
	var memberSource, memberRole string
	if err := pool.QueryRow(ctx, `SELECT source,role FROM workspace_members WHERE id='email-invite'`).Scan(&memberSource, &memberRole); err != nil || memberSource != "email" || memberRole != "member" {
		t.Fatalf("activated email invitation member = %q/%q, %v", memberSource, memberRole, err)
	}
	var inviteCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspace_invitations WHERE id='email-invite'`).Scan(&inviteCount); err != nil || inviteCount != 0 {
		t.Fatalf("email invitation remaining rows = %d, %v", inviteCount, err)
	}
	assertInvitationNotification(t, ctx, pool, fixture.sharedUserID, "InvitationAccepted", emailInviteeID, "email-invite")

	linkResult := graphRequest(t, handler, mutation, map[string]any{"inviteId": "link-invite"}, fixture.sessionID, linkUserID)
	assertMutationTrue(t, linkResult, "link invitation")
	var linkStatus, linkKind string
	if err := pool.QueryRow(ctx, `SELECT status,kind FROM workspace_invitations WHERE invitee_user_id=$1`, linkUserID).Scan(&linkStatus, &linkKind); err != nil || linkStatus != "waiting_review" || linkKind != "link" {
		rows, queryErr := pool.Query(ctx, `SELECT id,coalesce(invitee_user_id,''),status,kind FROM workspace_invitations`)
		var remaining []string
		if queryErr == nil {
			for rows.Next() {
				var id, invitee, status, kind string
				_ = rows.Scan(&id, &invitee, &status, &kind)
				remaining = append(remaining, id+":"+invitee+":"+status+":"+kind)
			}
			rows.Close()
		}
		t.Fatalf("link invitation state = %q/%q, user=%q err=%v remaining=%v", linkStatus, linkKind, linkUserID, err, remaining)
	}
	assertInvitationNotification(t, ctx, pool, fixture.userID, "InvitationReviewRequest", linkUserID, "")
	if len(mailer.calls) != 1 || mailer.calls[0].Type != "request" || mailer.calls[0].InviteeID != linkUserID || mailer.calls[0].ActorID != fixture.userID || mailer.calls[0].WorkspaceID != fixture.workspaceID || mailer.calls[0].NotificationID == "" {
		t.Fatalf("invitation review request mail calls = %#v", mailer.calls)
	}

	pendingResult := graphRequest(t, handler, mutation, map[string]any{"inviteId": "pending-link"}, fixture.sessionID, pendingUserID)
	assertMutationTrue(t, pendingResult, "pending email invitation through link")
	var pendingMemberSource string
	if err := pool.QueryRow(ctx, `SELECT source FROM workspace_members WHERE id='pending-email-invite'`).Scan(&pendingMemberSource); err != nil || pendingMemberSource != "email" {
		t.Fatalf("pending email invitation activation source = %q, %v", pendingMemberSource, err)
	}
	assertInvitationNotification(t, ctx, pool, fixture.sharedUserID, "InvitationAccepted", pendingUserID, "pending-email-invite")

	alreadyMember := graphRequest(t, handler, mutation, map[string]any{"inviteId": "link-invite"}, fixture.sessionID, linkUserID)
	if graphErrorName(alreadyMember) != "ALREADY_IN_SPACE" {
		t.Fatalf("already joined link error = %#v", alreadyMember["errors"])
	}
	if graphErrorMessage(alreadyMember) != "You have already joined in Space "+fixture.workspaceID+"." {
		t.Fatalf("already joined link message = %q", graphErrorMessage(alreadyMember))
	}

	mismatch := graphRequest(t, handler, mutation, map[string]any{"inviteId": "mismatch-invite"}, fixture.sessionID, fixture.userID)
	if graphErrorName(mismatch) != "INVITATION_ACCOUNT_MISMATCH" {
		t.Fatalf("email invite account mismatch error = %#v", mismatch["errors"])
	}

	fullSeatUserID := newInvitationTestUser(t, ctx, pool, fixture, uuid.NewString())
	for i := 0; i < 4; i++ {
		if _, err := pool.Exec(ctx, `INSERT INTO workspace_invitations(id,workspace_id,invitee_user_id,requested_role,status,kind) VALUES ($1,$2,$3,'member','waiting_review','link')`, uuid.NewString(), fixture.workspaceID, "reserved-user-"+uuid.NewString()); err != nil {
			t.Fatalf("fill remaining workspace seats: %v", err)
		}
	}
	linkLookup.targets["full-link"] = &invitelinks.InvitationTarget{WorkspaceID: fixture.workspaceID, InviterID: fixture.userID}
	noSeats := graphRequest(t, handler, mutation, map[string]any{"inviteId": "full-link"}, fixture.sessionID, fullSeatUserID)
	if graphErrorName(noSeats) != "NO_MORE_SEAT" || graphErrorMessage(noSeats) != "No more seat available in the Space "+fixture.workspaceID+"." {
		t.Fatalf("full workspace seat error = %#v", noSeats["errors"])
	}
	noSeatData := graphErrorData(noSeats)
	if noSeatData["spaceId"] != fixture.workspaceID {
		t.Fatalf("full workspace seat error data = %#v", noSeatData)
	}
	unknown := graphRequest(t, handler, mutation, map[string]any{"inviteId": "unknown-link"}, fixture.sessionID, linkUserID)
	if graphErrorName(unknown) != "INVALID_INVITATION" {
		t.Fatalf("unknown link error = %#v", unknown["errors"])
	}
	if len(membersPublisher.workspaceIDs) != 3 {
		t.Fatalf("member update publications = %#v", membersPublisher.workspaceIDs)
	}
	if len(countPublisher.userIDs) != 3 {
		t.Fatalf("notification count publications = %#v", countPublisher.userIDs)
	}
}

func newInvitationTestUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool, fixture graphqlFixture, id string) string {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,name,email,created_at,disabled) VALUES ($1,$1,$2,now(),false)`, id, id+"@example.test"); err != nil {
		t.Fatalf("insert invitation test user: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO user_sessions(id,session_id,user_id,created_at) VALUES ($1,$2,$3,now())`, id+"-session", fixture.sessionID, id); err != nil {
		t.Fatalf("insert invitation test user session: %v", err)
	}
	return id
}

func assertMutationTrue(t *testing.T, result map[string]any, label string) {
	t.Helper()
	if result["errors"] != nil || result["data"].(map[string]any)["acceptInviteById"] != true {
		t.Fatalf("%s result = %#v", label, result)
	}
}

func assertInvitationNotification(t *testing.T, ctx context.Context, pool *pgxpool.Pool, userID, notificationType, actorID, inviteID string) {
	t.Helper()
	var body []byte
	err := pool.QueryRow(ctx, `SELECT body FROM notifications WHERE user_id=$1 AND type=$2::"NotificationType" ORDER BY created_at DESC LIMIT 1`, userID, notificationType).Scan(&body)
	if err != nil {
		t.Fatalf("read %s notification: %v", notificationType, err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["createdByUserId"] != actorID || inviteID != "" && payload["inviteId"] != inviteID {
		t.Fatalf("%s notification body = %#v", notificationType, payload)
	}
}

func graphErrorName(result map[string]any) string {
	extensions := graphErrorExtensions(result)
	name, _ := extensions["name"].(string)
	return name
}

func graphErrorMessage(result map[string]any) string {
	errors, ok := result["errors"].([]any)
	if !ok || len(errors) == 0 {
		return ""
	}
	first, ok := errors[0].(map[string]any)
	if !ok {
		return ""
	}
	message, _ := first["message"].(string)
	return message
}

func graphErrorData(result map[string]any) map[string]any {
	data, _ := graphErrorExtensions(result)["data"].(map[string]any)
	return data
}

func graphErrorExtensions(result map[string]any) map[string]any {
	errors, ok := result["errors"].([]any)
	if !ok || len(errors) == 0 {
		return nil
	}
	first, ok := errors[0].(map[string]any)
	if !ok {
		return nil
	}
	extensions, ok := first["extensions"].(map[string]any)
	if !ok {
		return nil
	}
	return extensions
}
