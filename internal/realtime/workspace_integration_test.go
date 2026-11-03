// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package realtime

import (
	"context"
	"encoding/base64"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/Deln0r/ygo"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/documents"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresWorkspaceRealtimeEnforcesAclAndGeneration(t *testing.T) {
	pool := workspaceRealtimeTestPool(t)
	ctx := context.Background()
	workspaceID := "workspace-" + uuid.NewString()
	docID := "doc-" + uuid.NewString()
	deniedDocID := "doc-denied-" + uuid.NewString()
	base := realtimeFixtureUpdate(t, 9101, "base")
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id) VALUES ($1)`, workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_access_policies(workspace_id,member_default_doc_role) VALUES ($1,'none')`, workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_members(workspace_id,user_id,role,state) VALUES ($1,'user-1','member','active')`, workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO doc_grants(workspace_id,doc_id,principal_type,principal_id,role) VALUES ($1,$2,'user','user-1','editor')`, workspaceID, docID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO snapshots(workspace_id,guid,blob,created_at,updated_at) VALUES ($1,$2,$3,now(),now())`, workspaceID, docID, base); err != nil {
		t.Fatal(err)
	}

	workspaceRepository := workspaces.NewRepositoryWithDeployment(pool, "selfhosted")
	backend, err := NewWithDependencies(
		config.Config{NodeEnv: "test", Host: "localhost"},
		testAuthenticator{},
		testDocumentSync{},
		WorkspaceDependencies{
			Authorizer: workspaces.NewAuthorizationService(workspaceRepository),
			Documents:  documents.NewWorkspaceRepository(pool),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	server := httptest.NewServer(backend.Handler())
	defer server.Close()
	client := newPollingClient(t, server.URL, "valid-session")
	if !strings.Contains(client.connect(), "40{") {
		t.Fatal("expected authenticated Socket.IO connection")
	}
	observer := newPollingClient(t, server.URL, "valid-session")
	if !strings.Contains(observer.connect(), "40{") {
		t.Fatal("expected authenticated observer Socket.IO connection")
	}
	observerJoin := observer.emitAck(t, 1, "space:join-batch", map[string]any{
		"spaces":        []any{map[string]any{"spaceType": "workspace", "spaceId": workspaceID, "docId": docID}},
		"clientVersion": "0.27.5",
	})
	if !strings.Contains(observerJoin, `"success":true`) {
		t.Fatalf("observer workspace join = %q", observerJoin)
	}

	denied := client.emitAck(t, 1, "space:join-batch", map[string]any{
		"spaces": []any{
			map[string]any{"spaceType": "workspace", "spaceId": workspaceID, "docId": docID},
			map[string]any{"spaceType": "workspace", "spaceId": workspaceID, "docId": deniedDocID},
		},
		"clientVersion": "0.27.5",
	})
	if !strings.Contains(denied, `"name":"DOC_ACTION_DENIED"`) {
		t.Fatalf("join with unreadable document = %q", denied)
	}

	joined := client.emitAck(t, 2, "space:join-batch", map[string]any{
		"spaces":        []any{map[string]any{"spaceType": "workspace", "spaceId": workspaceID, "docId": docID}},
		"clientVersion": "0.27.5",
	})
	if !strings.Contains(joined, `"success":true`) {
		t.Fatalf("workspace join = %q", joined)
	}
	loaded := client.emitAck(t, 3, "space:load-doc", map[string]any{
		"spaceType": "workspace", "spaceId": workspaceID, "docId": docID,
	})
	if !strings.Contains(loaded, base64.StdEncoding.EncodeToString(base)) {
		t.Fatalf("workspace document load = %q", loaded)
	}

	newUpdate := realtimeFixtureUpdate(t, 9102, "next")
	pushed := client.emitAck(t, 4, "space:push-doc-update", map[string]any{
		"spaceType": "workspace", "spaceId": workspaceID, "docId": docID,
		"update": base64.StdEncoding.EncodeToString(newUpdate),
	})
	if !strings.Contains(pushed, `"accepted":true`) {
		t.Fatalf("workspace update = %q", pushed)
	}
	if strings.Contains(pushed, "space:broadcast-doc-updates") {
		t.Fatalf("sender received its own workspace document broadcast before ack: %q", pushed)
	}
	observerBroadcast := observer.poll()
	if !strings.Contains(observerBroadcast, "space:broadcast-doc-updates") || !strings.Contains(observerBroadcast, base64.StdEncoding.EncodeToString(newUpdate)) {
		t.Fatalf("active workspace subscriber did not receive document update: %q", observerBroadcast)
	}

	deniedLifecycle := client.emitAck(t, 41, "space:doc-lifecycle", map[string]any{
		"spaceType": "workspace", "spaceId": workspaceID, "docId": docID, "lifecycle": "trash",
	})
	if !strings.Contains(deniedLifecycle, `"name":"DOC_ACTION_DENIED"`) {
		t.Fatalf("editor document lifecycle response = %q, want DOC_ACTION_DENIED", deniedLifecycle)
	}

	if _, err := pool.Exec(ctx, `UPDATE doc_grants SET role='reader' WHERE workspace_id=$1 AND doc_id=$2 AND principal_id='user-1'`, workspaceID, docID); err != nil {
		t.Fatal(err)
	}
	stale := client.emitAck(t, 5, "space:push-doc-update", map[string]any{
		"spaceType": "workspace", "spaceId": workspaceID, "docId": docID,
		"update": base64.StdEncoding.EncodeToString(realtimeFixtureUpdate(t, 9103, "stale")),
	})
	if !strings.Contains(stale, `"name":"SYNC_PERMISSION_GENERATION_CHANGED"`) {
		t.Fatalf("push after permission generation change = %q", stale)
	}
	var updateCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM updates WHERE workspace_id=$1 AND guid=$2`, workspaceID, docID).Scan(&updateCount); err != nil {
		t.Fatal(err)
	}
	if updateCount != 1 {
		t.Fatalf("persisted updates after stale push = %d, want 1", updateCount)
	}

	readOnlyJoin := client.emitAck(t, 6, "space:join-batch", map[string]any{
		"spaces":        []any{map[string]any{"spaceType": "workspace", "spaceId": workspaceID, "docId": docID}},
		"clientVersion": "0.27.5",
	})
	if !strings.Contains(readOnlyJoin, `"success":true`) {
		t.Fatalf("reader rejoin = %q", readOnlyJoin)
	}
	readOnlyPush := client.emitAck(t, 7, "space:push-doc-update", map[string]any{
		"spaceType": "workspace", "spaceId": workspaceID, "docId": docID,
		"update": base64.StdEncoding.EncodeToString(realtimeFixtureUpdate(t, 9104, "denied")),
	})
	if !strings.Contains(readOnlyPush, `"name":"DOC_ACTION_DENIED"`) {
		t.Fatalf("reader push = %q", readOnlyPush)
	}

	if _, err := pool.Exec(ctx, `UPDATE workspace_members SET state='left' WHERE workspace_id=$1 AND user_id='user-1'`, workspaceID); err != nil {
		t.Fatal(err)
	}
	backend.PublishWorkspaceMembersUpdated(workspaceID)
	backend.subscriptions.mu.RLock()
	remainingSubscriptions := len(backend.subscriptions.docs[workspaceDocKey(workspaceID, docID)])
	backend.subscriptions.mu.RUnlock()
	if remainingSubscriptions != 0 {
		t.Fatalf("workspace subscriptions after member leave = %d, want 0", remainingSubscriptions)
	}
	leftPush := client.emitAck(t, 9, "space:push-doc-update", map[string]any{
		"spaceType": "workspace", "spaceId": workspaceID, "docId": docID,
		"update": base64.StdEncoding.EncodeToString(realtimeFixtureUpdate(t, 9105, "left")),
	})
	if !strings.Contains(leftPush, `"name":"NOT_IN_SPACE"`) {
		t.Fatalf("push after membership leave = %q, want subscription rejection", leftPush)
	}
	leftJoin := client.emitAck(t, 8, "space:join-batch", map[string]any{
		"spaces":        []any{map[string]any{"spaceType": "workspace", "spaceId": workspaceID, "docId": docID}},
		"clientVersion": "0.27.5",
	})
	if !strings.Contains(leftJoin, `"name":"SPACE_ACCESS_DENIED"`) {
		t.Fatalf("left member join = %q", leftJoin)
	}
}

func TestPostgresWorkspaceLifecycleRequiresManagerAndBroadcastsRootUpdate(t *testing.T) {
	pool := workspaceRealtimeTestPool(t)
	ctx := context.Background()
	workspaceID := "workspace-" + uuid.NewString()
	docID := "doc-" + uuid.NewString()
	root := realtimeWorkspaceRootUpdate(t, 9201, docID)
	doc := realtimeFixtureUpdate(t, 9202, "content")
	if _, err := pool.Exec(ctx, `CREATE TABLE comments (workspace_id varchar NOT NULL, doc_id varchar NOT NULL);
		CREATE TABLE replies (workspace_id varchar NOT NULL, doc_id varchar NOT NULL);
		CREATE TABLE snapshot_histories (workspace_id varchar NOT NULL, guid varchar NOT NULL);`); err != nil {
		t.Fatalf("create lifecycle row tables: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id) VALUES($1)`, workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_access_policies(workspace_id,member_default_doc_role) VALUES($1,'none')`, workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_members(workspace_id,user_id,role,state) VALUES($1,'user-1','member','active')`, workspaceID); err != nil {
		t.Fatal(err)
	}
	for _, grant := range []struct{ docID, role string }{{workspaceID, "reader"}, {docID, "manager"}} {
		if _, err := pool.Exec(ctx, `INSERT INTO doc_grants(workspace_id,doc_id,principal_type,principal_id,role) VALUES($1,$2,'user','user-1',$3)`, workspaceID, grant.docID, grant.role); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO doc_access_policies(workspace_id,doc_id) VALUES($1,$2)`, workspaceID, docID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_pages(workspace_id,page_id) VALUES($1,$2)`, workspaceID, docID); err != nil {
		t.Fatal(err)
	}
	for _, insert := range []string{
		`INSERT INTO comments(workspace_id,doc_id) VALUES($1,$2)`,
		`INSERT INTO replies(workspace_id,doc_id) VALUES($1,$2)`,
		`INSERT INTO snapshot_histories(workspace_id,guid) VALUES($1,$2)`,
	} {
		if _, err := pool.Exec(ctx, insert, workspaceID, docID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO snapshots(workspace_id,guid,blob,created_at,updated_at) VALUES($1,$1,$2,now(),now())`, workspaceID, root); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO snapshots(workspace_id,guid,blob,created_at,updated_at) VALUES($1,$2,$3,now(),now())`, workspaceID, docID, doc); err != nil {
		t.Fatal(err)
	}

	workspaceRepository := workspaces.NewRepositoryWithDeployment(pool, "selfhosted")
	backend, err := NewWithDependencies(
		config.Config{NodeEnv: "test", Host: "localhost"},
		testAuthenticator{},
		testDocumentSync{},
		WorkspaceDependencies{
			Authorizer: workspaces.NewAuthorizationService(workspaceRepository),
			Documents:  documents.NewWorkspaceRepository(pool),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	server := httptest.NewServer(backend.Handler())
	defer server.Close()
	manager := newPollingClient(t, server.URL, "valid-session")
	if !strings.Contains(manager.connect(), "40{") {
		t.Fatal("expected authenticated manager connection")
	}
	observer := newPollingClient(t, server.URL, "valid-session")
	if !strings.Contains(observer.connect(), "40{") {
		t.Fatal("expected authenticated observer connection")
	}
	joined := observer.emitAck(t, 1, "space:join-batch", map[string]any{
		"spaces":        []any{map[string]any{"spaceType": "workspace", "spaceId": workspaceID, "docId": workspaceID}},
		"clientVersion": "0.27.5",
	})
	if !strings.Contains(joined, `"success":true`) {
		t.Fatalf("root document observer join = %q", joined)
	}

	response := manager.emitAck(t, 1, "space:doc-lifecycle", map[string]any{
		"spaceType": "workspace", "spaceId": workspaceID, "docId": docID, "lifecycle": "trash",
	})
	if !strings.Contains(response, `"rootUpdate":"`) || !strings.Contains(response, `"timestamp":`) {
		t.Fatalf("manager lifecycle acknowledgement = %q", response)
	}
	var broadcast string
	for attempt := 0; attempt < 3 && !strings.Contains(broadcast, "space:broadcast-doc-updates"); attempt++ {
		broadcast += observer.poll()
	}
	if !strings.Contains(broadcast, "space:broadcast-doc-updates") || !strings.Contains(broadcast, `"docId":"`+workspaceID+`"`) {
		t.Fatalf("root lifecycle update was not broadcast to authorized subscriber: %q", broadcast)
	}

	deleted := manager.emitAck(t, 2, "space:delete-doc", map[string]any{
		"spaceType": "workspace", "spaceId": workspaceID, "docId": docID,
	})
	if !strings.Contains(deleted, `"data":{"success":true}`) {
		t.Fatalf("manager delete acknowledgement = %q", deleted)
	}
	var deleteBroadcast string
	for attempt := 0; attempt < 3 && !strings.Contains(deleteBroadcast, "space:broadcast-doc-updates"); attempt++ {
		deleteBroadcast += observer.poll()
	}
	if !strings.Contains(deleteBroadcast, "space:broadcast-doc-updates") || !strings.Contains(deleteBroadcast, `"docId":"`+workspaceID+`"`) {
		t.Fatalf("root delete update was not broadcast after permission generation changed: %q", deleteBroadcast)
	}
	rootUpdate, _, _, err := documents.NewWorkspaceRepository(pool).DiffWorkspace(ctx, workspaceID, workspaceID, nil)
	if err != nil {
		t.Fatal(err)
	}
	rootDoc := ygo.NewDoc()
	if err := ygo.ApplyUpdate(rootDoc, rootUpdate); err != nil {
		t.Fatal(err)
	}
	pages, ok := ygo.NewMap(rootDoc, "meta").Get("pages").(*ygo.Array)
	if !ok {
		t.Fatal("root pages after delete are not an array")
	}
	for index := uint64(0); index < pages.Len(); index++ {
		page, ok := pages.Get(index).(*ygo.Map)
		if !ok {
			t.Fatalf("root page %d is not a map", index)
		}
		if page.Get("id") == docID {
			t.Fatalf("deleted document %q remains in root metadata", docID)
		}
	}
	var deletedRows int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM snapshots WHERE workspace_id=$1 AND guid=$2)
		+ (SELECT count(*) FROM comments WHERE workspace_id=$1 AND doc_id=$2)
		+ (SELECT count(*) FROM replies WHERE workspace_id=$1 AND doc_id=$2)
		+ (SELECT count(*) FROM snapshot_histories WHERE workspace_id=$1 AND guid=$2)
		+ (SELECT count(*) FROM doc_grants WHERE workspace_id=$1 AND doc_id=$2)
		+ (SELECT count(*) FROM doc_access_policies WHERE workspace_id=$1 AND doc_id=$2)
		+ (SELECT count(*) FROM workspace_pages WHERE workspace_id=$1 AND page_id=$2)`, workspaceID, docID).Scan(&deletedRows); err != nil {
		t.Fatal(err)
	}
	if deletedRows != 0 {
		t.Fatalf("related rows after workspace delete = %d, want 0", deletedRows)
	}

	access, err := workspaces.NewAuthorizationService(workspaceRepository).AuthorizeWorkspaceSync(ctx, "user-1", workspaceID, []string{docID})
	if err != nil {
		t.Fatal(err)
	}
	if access.Documents[docID].Read || access.Documents[docID].Manage {
		t.Fatalf("deleted document permissions = %+v, want no access", access.Documents[docID])
	}
}

func realtimeWorkspaceRootUpdate(t *testing.T, clientID uint64, docID string) []byte {
	t.Helper()
	doc := ygo.NewDocWithOptions(ygo.Options{ClientID: clientID})
	meta := ygo.NewMap(doc, "meta")
	txn := doc.WriteTxn()
	pages := meta.SetArray(txn, "pages")
	page := pages.InsertMap(txn, 0)
	page.Set(txn, "id", docID)
	page.Set(txn, "title", "Lifecycle target")
	txn.Commit()
	return ygo.EncodeStateAsUpdate(doc)
}

func realtimeFixtureUpdate(t *testing.T, clientID uint64, value string) []byte {
	t.Helper()
	doc := ygo.NewDocWithOptions(ygo.Options{ClientID: clientID})
	text := ygo.NewText(doc, "body")
	txn := doc.WriteTxn()
	if err := text.Insert(txn, 0, value); err != nil {
		t.Fatal(err)
	}
	txn.Commit()
	return ygo.EncodeStateAsUpdate(doc)
}

func workspaceRealtimeTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := emptyRealtimeTestPool(t)
	if _, err := pool.Exec(context.Background(), `
		CREATE TABLE workspaces (id varchar PRIMARY KEY, public boolean NOT NULL DEFAULT false, enable_url_preview boolean NOT NULL DEFAULT false);
		CREATE TABLE workspace_members (
			id varchar PRIMARY KEY DEFAULT md5(random()::text), workspace_id varchar NOT NULL, user_id varchar NOT NULL,
			role text NOT NULL, state text NOT NULL, source text NOT NULL DEFAULT 'legacy', legacy_permission_id varchar,
			created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now()
		);
		CREATE TABLE workspace_access_policies (
			workspace_id varchar PRIMARY KEY, visibility text NOT NULL DEFAULT 'private', sharing_enabled boolean NOT NULL DEFAULT true,
			url_preview_enabled boolean NOT NULL DEFAULT false, member_default_doc_role text NOT NULL DEFAULT 'manager'
		);
		CREATE TABLE doc_access_policies (
			workspace_id varchar NOT NULL, doc_id varchar NOT NULL, visibility text NOT NULL DEFAULT 'private', public_role text,
			member_default_role text, published_at timestamptz, PRIMARY KEY(workspace_id, doc_id)
		);
		CREATE TABLE doc_grants (
			workspace_id varchar NOT NULL, doc_id varchar NOT NULL, principal_type text NOT NULL,
			principal_id varchar NOT NULL, role text NOT NULL, legacy_workspace_id varchar, legacy_doc_id varchar, legacy_user_id varchar,
			created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now()
		);
		CREATE TABLE workspace_pages (
			workspace_id varchar NOT NULL, page_id varchar NOT NULL, public boolean NOT NULL DEFAULT false,
			mode smallint NOT NULL DEFAULT 0, "defaultRole" smallint, published_at timestamptz
		);
		CREATE TYPE "WorkspaceMemberStatus" AS ENUM ('Pending','NeedMoreSeat','NeedMoreSeatAndReview','UnderReview','Accepted','AllocatingSeat');
		CREATE TABLE workspace_user_permissions (
			id varchar PRIMARY KEY, workspace_id varchar NOT NULL, user_id varchar NOT NULL, type smallint NOT NULL,
			status "WorkspaceMemberStatus" NOT NULL, source text, inviter_id varchar,
			created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now()
		);
		CREATE TABLE workspace_page_user_permissions (
			workspace_id varchar NOT NULL, page_id varchar NOT NULL, user_id varchar NOT NULL,
			type smallint NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
			PRIMARY KEY(workspace_id,page_id,user_id)
		);
		CREATE TABLE entitlements (
			id varchar PRIMARY KEY, target_type text NOT NULL, target_id varchar NOT NULL, source text NOT NULL,
			plan text NOT NULL, quantity integer, status text NOT NULL, starts_at timestamptz,
			expires_at timestamptz, grace_until timestamptz, signed_payload bytea
		);
		CREATE TABLE workspace_sync_permission_generations (workspace_id varchar PRIMARY KEY, generation bigint NOT NULL DEFAULT 0);
		CREATE FUNCTION bump_test_permission_generation() RETURNS trigger AS $$
		DECLARE affected_workspace_id varchar;
		BEGIN
			affected_workspace_id := COALESCE(NEW.workspace_id, OLD.workspace_id);
			INSERT INTO workspace_sync_permission_generations(workspace_id, generation) VALUES (affected_workspace_id, 1)
			ON CONFLICT (workspace_id) DO UPDATE SET generation = workspace_sync_permission_generations.generation + 1;
			RETURN COALESCE(NEW, OLD);
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER workspace_member_generation AFTER INSERT OR UPDATE OR DELETE ON workspace_members
		FOR EACH ROW EXECUTE FUNCTION bump_test_permission_generation();
		CREATE TRIGGER workspace_policy_generation AFTER INSERT OR UPDATE OR DELETE ON workspace_access_policies
		FOR EACH ROW EXECUTE FUNCTION bump_test_permission_generation();
		CREATE TRIGGER document_policy_generation AFTER INSERT OR UPDATE OR DELETE ON doc_access_policies
		FOR EACH ROW EXECUTE FUNCTION bump_test_permission_generation();
		CREATE TRIGGER document_grant_generation AFTER INSERT OR UPDATE OR DELETE ON doc_grants
		FOR EACH ROW EXECUTE FUNCTION bump_test_permission_generation();
		CREATE TABLE snapshots (
			workspace_id varchar NOT NULL, guid varchar NOT NULL, blob bytea NOT NULL,
			created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL,
			PRIMARY KEY (workspace_id, guid)
		);
		CREATE TABLE updates (
			workspace_id varchar NOT NULL, guid varchar NOT NULL, blob bytea NOT NULL,
			created_at timestamptz NOT NULL, created_by varchar,
			PRIMARY KEY (workspace_id, guid, created_at)
		);`); err != nil {
		t.Fatalf("create workspace realtime test schema: %v", err)
	}
	return pool
}

// emptyRealtimeTestPool returns a pool scoped to a new, empty schema.
func emptyRealtimeTestPool(t *testing.T) *pgxpool.Pool {
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
	schema := "notes_workspace_realtime_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := adminPool.Exec(context.Background(), `CREATE SCHEMA `+schema); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated schema: %v", err)
	}
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		_, _ = adminPool.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
		adminPool.Close()
		t.Fatalf("parse integration PostgreSQL config: %v", err)
	}
	poolConfig.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(context.Background(), poolConfig)
	if err != nil {
		_, _ = adminPool.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
		adminPool.Close()
		t.Fatalf("open schema-scoped PostgreSQL pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = adminPool.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
		adminPool.Close()
	})
	return pool
}
