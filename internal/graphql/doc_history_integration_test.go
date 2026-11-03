// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"testing"
	"time"

	"github.com/Deln0r/ygo"
	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/documents"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

func TestDocumentHistoryGraphQLReadAndRecoverPermissions(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	if _, err := pool.Exec(ctx, `
		ALTER TABLE workspace_access_policies ADD COLUMN member_default_doc_role text NOT NULL DEFAULT 'reader';
		ALTER TABLE snapshots ADD COLUMN state bytea;
		ALTER TABLE updates ADD COLUMN blob bytea NOT NULL DEFAULT '\x'::bytea;
		ALTER TABLE updates ADD COLUMN IF NOT EXISTS created_at timestamptz NOT NULL DEFAULT now();
		ALTER TABLE snapshot_histories ADD COLUMN timestamp timestamptz;
		ALTER TABLE snapshot_histories ADD COLUMN blob bytea;
		ALTER TABLE snapshot_histories ADD COLUMN state bytea;
		ALTER TABLE snapshot_histories ADD COLUMN expired_at timestamptz;
		ALTER TABLE snapshot_histories ADD COLUMN created_by text;
		ALTER TABLE snapshot_histories ADD CONSTRAINT snapshot_histories_unique UNIQUE(workspace_id,guid,timestamp);
		CREATE TABLE doc_grants(workspace_id text,doc_id text,principal_type text,principal_id text,role text);
		CREATE TABLE doc_access_policies(workspace_id text,doc_id text,visibility text DEFAULT 'private',public_role text,member_default_role text,published_at timestamptz,updated_at timestamptz DEFAULT now(),PRIMARY KEY(workspace_id,doc_id));
		CREATE TABLE workspace_pages(workspace_id text,page_id text,mode smallint DEFAULT 0,title text,summary text,published_at timestamptz,PRIMARY KEY(workspace_id,page_id));
		CREATE TABLE workspace_sync_permission_generations(workspace_id text PRIMARY KEY,generation bigint NOT NULL DEFAULT 0);`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspace_members SET role='member' WHERE workspace_id=$1 AND user_id=$2`, fixture.workspaceID, fixture.sharedUserID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_access_policies(workspace_id,visibility,sharing_enabled,url_preview_enabled,member_default_doc_role) VALUES($1,'private',true,false,'reader')`, fixture.workspaceID); err != nil {
		t.Fatal(err)
	}
	targetTimestamp := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `INSERT INTO snapshots(workspace_id,guid,blob,state,created_at,updated_at,created_by,updated_by) VALUES($1,'doc',$2,'current-state',now()-interval '2 hours',now()-interval '1 hour',$3,$3)`, fixture.workspaceID, historyTestUpdate(t, 9501, "current base"), fixture.userID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO updates(workspace_id,guid,blob,created_at) VALUES($1,'doc',$2,now()-interval '30 minutes')`, fixture.workspaceID, historyTestUpdate(t, 9502, "pending")); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO doc_access_policies(workspace_id,doc_id,member_default_role) VALUES($1,'doc','reader')`, fixture.workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_pages(workspace_id,page_id,title) VALUES($1,'doc','History document')`, fixture.workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_sync_permission_generations(workspace_id,generation) VALUES($1,3)`, fixture.workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO snapshot_histories(workspace_id,guid,timestamp,blob,state,expired_at,created_by) VALUES($1,'doc',$2::timestamptz,$3,$4,$2::timestamptz+interval '48 hours',$5)`, fixture.workspaceID, targetTimestamp, historyTestUpdate(t, 9503, "restored"), []byte("restored-state"), fixture.userID); err != nil {
		t.Fatal(err)
	}

	repository := workspaces.NewRepositoryWithDeployment(pool, "selfhosted")
	documentReadService := workspaces.NewDocumentReadService(repository, nil)
	documentRepository := documents.NewWorkspaceRepository(pool)
	handler := NewHandlerWithServices(config.Config{Deployment: "selfhosted", Host: "localhost", Port: 3010}, auth.NewSessionResolver(pool), nil, repository, nil, nil, nil, nil, HandlerDependencies{
		DocumentReader:  documentReadService,
		DocumentHistory: DocumentHistoryDependencies{Repository: documentRepository, Access: documentReadService},
	})

	query := `query($workspace:String!,$guid:String!,$before:DateTime){workspace(id:$workspace){histories(guid:$guid,before:$before,take:10){id workspaceId timestamp editor{name avatarUrl}}}}`
	variables := map[string]any{"workspace": fixture.workspaceID, "guid": "doc", "before": "2026-02-01T00:00:00.000Z"}
	readable := graphRequest(t, handler, query, variables, fixture.sessionID, fixture.sharedUserID)
	if readable["errors"] != nil {
		t.Fatalf("reader history query = %#v", readable)
	}
	histories := readable["data"].(map[string]any)["workspace"].(map[string]any)["histories"].([]any)
	if len(histories) != 1 {
		t.Fatalf("reader history count = %d, want 1", len(histories))
	}
	history := histories[0].(map[string]any)
	if history["id"] != "doc" || history["workspaceId"] != fixture.workspaceID || history["timestamp"] != "2026-01-01T00:00:00.000Z" || history["editor"].(map[string]any)["name"] != "Web User" {
		t.Fatalf("history projection = %#v", history)
	}
	subMillisecondBefore := map[string]any{"workspace": fixture.workspaceID, "guid": "doc", "before": "2026-01-01T00:00:00.000999Z"}
	truncatedBefore := graphRequest(t, handler, query, subMillisecondBefore, fixture.sessionID, fixture.sharedUserID)
	if truncatedBefore["errors"] != nil || len(truncatedBefore["data"].(map[string]any)["workspace"].(map[string]any)["histories"].([]any)) != 0 {
		t.Fatalf("sub-millisecond before should use Date millisecond precision: %#v", truncatedBefore)
	}

	recoverMutation := `mutation($workspace:String!,$guid:String!,$timestamp:DateTime!){recoverDoc(workspaceId:$workspace,guid:$guid,timestamp:$timestamp)}`
	recoverVariables := map[string]any{"workspace": fixture.workspaceID, "guid": "doc", "timestamp": "2026-01-01T00:00:00.000999Z"}
	readerRecovery := graphRequest(t, handler, recoverMutation, recoverVariables, fixture.sessionID, fixture.sharedUserID)
	if readerRecovery["errors"] == nil || readerRecovery["errors"].([]any)[0].(map[string]any)["extensions"].(map[string]any)["name"] != "doc_action_denied" {
		t.Fatalf("reader recovery should require Doc.Update: %#v", readerRecovery)
	}
	if _, err := pool.Exec(ctx, `UPDATE doc_access_policies SET member_default_role='none' WHERE workspace_id=$1 AND doc_id='doc'`, fixture.workspaceID); err != nil {
		t.Fatal(err)
	}
	deniedHistory := graphRequest(t, handler, query, variables, fixture.sessionID, fixture.sharedUserID)
	if deniedHistory["errors"] == nil || deniedHistory["errors"].([]any)[0].(map[string]any)["extensions"].(map[string]any)["name"] != "doc_action_denied" {
		t.Fatalf("member without history permission should be denied: %#v", deniedHistory)
	}

	ownerRecovery := graphRequest(t, handler, recoverMutation, recoverVariables, fixture.sessionID, fixture.userID)
	if ownerRecovery["errors"] != nil || ownerRecovery["data"].(map[string]any)["recoverDoc"] != "2026-01-01T00:00:00.000Z" {
		t.Fatalf("owner recovery = %#v", ownerRecovery)
	}
	var restoredBlob, restoredState []byte
	var pendingUpdates int
	if err := pool.QueryRow(ctx, `SELECT blob,state FROM snapshots WHERE workspace_id=$1 AND guid='doc'`, fixture.workspaceID).Scan(&restoredBlob, &restoredState); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM updates WHERE workspace_id=$1 AND guid='doc'`, fixture.workspaceID).Scan(&pendingUpdates); err != nil {
		t.Fatal(err)
	}
	if len(restoredBlob) == 0 || string(restoredState) != "restored-state" || pendingUpdates != 0 {
		t.Fatalf("recovered document state: blob=%x state=%q updates=%d", restoredBlob, restoredState, pendingUpdates)
	}
}

func historyTestUpdate(t *testing.T, clientID uint64, value string) []byte {
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
