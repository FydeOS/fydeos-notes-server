// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRepositoryListsUnreadEnrichesBodiesAndScopesReadMutations(t *testing.T) {
	pool := notificationsTestPool(t)
	ctx := context.Background()
	for _, statement := range []string{
		`CREATE TYPE "NotificationLevel" AS ENUM ('High','Default','Low','Min','None')`,
		`CREATE TYPE "NotificationType" AS ENUM ('Mention','Invitation','InvitationAccepted','InvitationBlocked','InvitationRejected','InvitationReviewRequest','InvitationReviewApproved','InvitationReviewDeclined','Comment','CommentMention')`,
		`CREATE TABLE users (id varchar PRIMARY KEY, name text NOT NULL, avatar_url text)`,
		`CREATE TABLE workspaces (id varchar PRIMARY KEY, name text)`,
		`CREATE TABLE workspace_pages (workspace_id varchar NOT NULL, page_id varchar NOT NULL, title varchar, PRIMARY KEY(workspace_id,page_id))`,
		`CREATE TABLE notifications (
			id varchar PRIMARY KEY, user_id varchar NOT NULL,
			created_at timestamptz(3) NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at timestamptz(3) NOT NULL DEFAULT CURRENT_TIMESTAMP,
			level "NotificationLevel" NOT NULL, read boolean NOT NULL DEFAULT false,
			type "NotificationType" NOT NULL, body jsonb NOT NULL
		)`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("create notification fixture schema: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO users(id,name,avatar_url) VALUES
			('recipient-1','Recipient',NULL),('actor-1','Actor','https://example.test/actor.png');
		INSERT INTO workspaces(id,name) VALUES ('workspace-1','Workspace One'),('workspace-2',NULL);
		INSERT INTO workspace_pages(workspace_id,page_id,title) VALUES ('workspace-1','doc-1','Latest title');`); err != nil {
		t.Fatalf("insert notification fixture users and workspaces: %v", err)
	}
	base := time.Date(2026, 10, 17, 3, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `
		INSERT INTO notifications(id,user_id,created_at,updated_at,level,read,type,body) VALUES
			('notification-old','recipient-1',$1,$1,'Default',false,'Mention',
			 '{"workspaceId":"workspace-1","createdByUserId":"actor-1","doc":{"id":"doc-1","title":"Draft"}}'),
			('notification-new','recipient-1',$1 + interval '2 seconds',$1 + interval '2 seconds','High',false,'Comment',
			 '{"workspaceId":"workspace-2","createdByUserId":"actor-1","doc":{"id":"doc-2","title":"Client title"}}'),
			('notification-read','recipient-1',$1 + interval '3 seconds',$1 + interval '3 seconds','Low',true,'Mention','{}'),
			('notification-other-user','recipient-2',$1 + interval '4 seconds',$1 + interval '4 seconds','Default',false,'Mention','{}')`, base); err != nil {
		t.Fatalf("insert notification fixtures: %v", err)
	}

	repository := NewRepository(pool, "https://notes.example.test/")
	count, err := repository.CountUnread(ctx, "recipient-1")
	if err != nil || count != 2 {
		t.Fatalf("unread count = %d, %v; want 2", count, err)
	}
	items, err := repository.ListUnread(ctx, "recipient-1", nil, 10, 0)
	if err != nil || len(items) != 2 {
		t.Fatalf("unread notifications = %#v, %v; want two", items, err)
	}
	if items[0].ID != "notification-new" || items[1].ID != "notification-old" || items[0].Read || items[1].Read {
		t.Fatalf("unread ordering or read filter = %#v", items)
	}
	if items[0].Body["type"] != "Comment" || items[0].Body["createdByUser"].(map[string]any)["name"] != "Actor" {
		t.Fatalf("notification actor/type projection = %#v", items[0].Body)
	}
	workspace := items[0].Body["workspace"].(map[string]any)
	if workspace["id"] != "workspace-2" || workspace["name"] != "Untitled Workspace" || workspace["url"] != "https://notes.example.test/workspace/workspace-2" {
		t.Fatalf("notification workspace projection = %#v", workspace)
	}
	missingDocMeta := items[0].Body["doc"].(map[string]any)
	if missingDocMeta["title"] != "Client title" {
		t.Fatalf("notification client document title fallback = %#v", missingDocMeta)
	}
	actor := items[1].Body["createdByUser"].(map[string]any)
	if actor["id"] != "actor-1" || actor["avatarUrl"] != "https://example.test/actor.png" {
		t.Fatalf("notification public user projection = %#v", actor)
	}
	doc := items[1].Body["doc"].(map[string]any)
	if doc["title"] != "Latest title" {
		t.Fatalf("notification latest document title = %#v", doc)
	}

	page, err := repository.ListUnread(ctx, "recipient-1", nil, 1, 0)
	if err != nil || len(page) != 1 || page[0].ID != "notification-new" {
		t.Fatalf("first page = %#v, %v", page, err)
	}
	page, err = repository.ListUnread(ctx, "recipient-1", nil, 1, 1)
	if err != nil || len(page) != 1 || page[0].ID != "notification-old" {
		t.Fatalf("offset page = %#v, %v", page, err)
	}
	after := base.Add(time.Second)
	page, err = repository.ListUnread(ctx, "recipient-1", &after, 10, 0)
	if err != nil || len(page) != 1 || page[0].ID != "notification-old" {
		t.Fatalf("cursor page = %#v, %v", page, err)
	}

	if err := repository.MarkAsRead(ctx, "recipient-2", "notification-old"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other user's notification update error = %v", err)
	}
	count, err = repository.CountUnread(ctx, "recipient-1")
	if err != nil || count != 2 {
		t.Fatalf("unread count after rejected cross-user update = %d, %v", count, err)
	}
	if err := repository.MarkAsRead(ctx, "recipient-1", "notification-old"); err != nil {
		t.Fatalf("mark notification read: %v", err)
	}
	if err := repository.MarkAsRead(ctx, "recipient-1", "notification-old"); err != nil {
		t.Fatalf("mark already-read notification again: %v", err)
	}
	if err := repository.MarkAsRead(ctx, "recipient-1", "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing notification update error = %v", err)
	}
	if err := repository.MarkAllAsRead(ctx, "recipient-1"); err != nil {
		t.Fatalf("mark all recipient notifications read: %v", err)
	}
	count, err = repository.CountUnread(ctx, "recipient-1")
	if err != nil || count != 0 {
		t.Fatalf("recipient unread count after mark-all = %d, %v", count, err)
	}
	count, err = repository.CountUnread(ctx, "recipient-2")
	if err != nil || count != 1 {
		t.Fatalf("other user's unread count after mark-all = %d, %v; want 1", count, err)
	}
	mentionID, err := repository.CreateMention(ctx, "recipient-1", "workspace-1", "actor-1", map[string]any{
		"id": "doc-mention", "title": "Mention target", "mode": "page", "blockId": "block-1",
	})
	if err != nil || mentionID == "" {
		t.Fatalf("create mention notification = %q, %v", mentionID, err)
	}
	count, err = repository.CountUnread(ctx, "recipient-1")
	if err != nil || count != 1 {
		t.Fatalf("recipient unread count after mention = %d, %v; want 1", count, err)
	}
	created, err := repository.ListUnread(ctx, "recipient-1", nil, 10, 0)
	if err != nil || len(created) != 1 || created[0].ID != mentionID || created[0].Type != "Mention" || created[0].Body["createdByUserId"] != "actor-1" {
		t.Fatalf("created mention notification = %#v, %v", created, err)
	}
}

func TestRepositoryCreatesInvitationForPendingTargetOnly(t *testing.T) {
	pool := notificationsTestPool(t)
	ctx := context.Background()
	for _, statement := range []string{
		`CREATE TYPE "NotificationLevel" AS ENUM ('High','Default','Low','Min','None')`,
		`CREATE TYPE "NotificationType" AS ENUM ('Mention','Invitation','InvitationAccepted','InvitationBlocked','InvitationRejected','InvitationReviewRequest','InvitationReviewApproved','InvitationReviewDeclined','Comment','CommentMention')`,
		`CREATE TABLE workspace_members (workspace_id text NOT NULL, user_id text NOT NULL, state text NOT NULL)`,
		`CREATE TABLE notifications (id text PRIMARY KEY, user_id text NOT NULL, created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(), level "NotificationLevel" NOT NULL, read boolean NOT NULL DEFAULT false, type "NotificationType" NOT NULL, body jsonb NOT NULL)`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("create invitation notification fixture: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_members(workspace_id,user_id,state) VALUES('workspace-1','active-user','active')`); err != nil {
		t.Fatal(err)
	}
	repository := NewRepository(pool, "https://notes.example.test")
	input := InvitationNotification{UserID: "pending-user", WorkspaceID: "workspace-1", CreatedByUserID: "inviter", InviteID: "invite-1", Type: "Invitation"}
	notificationID, created, err := repository.CreateInvitation(ctx, input)
	if err != nil || !created || notificationID == "" {
		t.Fatalf("create pending-target invitation notification = %q/%t, %v", notificationID, created, err)
	}
	var storedUser, storedType string
	var body []byte
	if err := pool.QueryRow(ctx, `SELECT user_id,type::text,body FROM notifications WHERE id=$1`, notificationID).Scan(&storedUser, &storedType, &body); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if storedUser != input.UserID || storedType != "Invitation" || payload["workspaceId"] != input.WorkspaceID || payload["createdByUserId"] != input.CreatedByUserID || payload["inviteId"] != input.InviteID {
		t.Fatalf("invitation notification persisted = %q/%q %#v", storedUser, storedType, payload)
	}
	input.UserID = "active-user"
	if notificationID, created, err := repository.CreateInvitation(ctx, input); err != nil || created || notificationID != "" {
		t.Fatalf("active workspace user invitation notification = %q/%t, %v; want no-op", notificationID, created, err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM notifications`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("invitation notification count = %d, %v; want one", count, err)
	}
}

func TestRepositoryCleanupExpiredNotificationsDeletesInBatchesAndReturnsUniqueUsers(t *testing.T) {
	pool := notificationsTestPool(t)
	ctx := context.Background()
	for _, statement := range []string{
		`CREATE TABLE notifications (id varchar PRIMARY KEY, user_id varchar NOT NULL, created_at timestamptz NOT NULL)`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("create notification cleanup fixture schema: %v", err)
		}
	}
	cutoff := time.Date(2026, 10, 17, 3, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `
		INSERT INTO notifications(id,user_id,created_at) VALUES
			('expired-a-1','user-a',$1::timestamptz - interval '2 hours'),
			('expired-a-2','user-a',$1::timestamptz - interval '1 hour'),
			('expired-b','user-b',$1::timestamptz),
			('not-expired','user-a',$1::timestamptz + interval '1 microsecond')`, cutoff); err != nil {
		t.Fatalf("insert notification cleanup fixtures: %v", err)
	}

	repository := NewRepository(pool, "")
	if _, err := repository.CleanupExpiredNotifications(ctx, cutoff, 0); err == nil {
		t.Fatal("zero cleanup limit unexpectedly succeeded")
	}
	first, err := repository.CleanupExpiredNotifications(ctx, cutoff, 2)
	if err != nil || first.Count != 2 || len(first.UserIDs) != 1 || first.UserIDs[0] != "user-a" {
		t.Fatalf("first cleanup batch = %#v, %v; want two rows for user-a", first, err)
	}
	second, err := repository.CleanupExpiredNotifications(ctx, cutoff, 2)
	if err != nil || second.Count != 1 || len(second.UserIDs) != 1 || second.UserIDs[0] != "user-b" {
		t.Fatalf("second cleanup batch = %#v, %v; want the row at cutoff for user-b", second, err)
	}
	empty, err := repository.CleanupExpiredNotifications(ctx, cutoff, 2)
	if err != nil || empty.Count != 0 || len(empty.UserIDs) != 0 {
		t.Fatalf("empty cleanup batch = %#v, %v", empty, err)
	}
	var remaining int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM notifications`).Scan(&remaining); err != nil || remaining != 1 {
		t.Fatalf("remaining notification count = %d, %v; want only the unexpired row", remaining, err)
	}
}

func notificationsTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("NOTES_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set NOTES_TEST_DATABASE_URL to a disposable local notes_test database")
	}
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatalf("parse NOTES_TEST_DATABASE_URL: %v", err)
	}
	if parsed.Hostname() != "localhost" && parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "::1" {
		t.Fatal("integration tests require a loopback PostgreSQL host")
	}
	if strings.Trim(parsed.Path, "/") != "notes_test" {
		t.Fatal("integration tests require a disposable database named notes_test")
	}
	adminPool, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		t.Fatalf("connect integration PostgreSQL: %v", err)
	}
	schema := "notes_notifications_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := adminPool.Exec(context.Background(), `CREATE SCHEMA `+schema); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated schema: %v", err)
	}
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		_, _ = adminPool.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
		adminPool.Close()
		t.Fatalf("parse integration pool config: %v", err)
	}
	poolConfig.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(context.Background(), poolConfig)
	if err != nil {
		_, _ = adminPool.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
		adminPool.Close()
		t.Fatalf("open schema-scoped integration pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = adminPool.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
		adminPool.Close()
	})
	return pool
}
