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
	"github.com/fydeos/fydeos-notes-server/internal/search"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

// Exercise the same sync -> background projection -> authenticated GraphQL path
// as Web. Do not pre-populate workspace_pages: that hid the staging regression.
func TestDocumentSyncPopulatesGraphQLMetadata(t *testing.T) {
	for _, updatesOnly := range []bool{false, true} {
		name := "snapshot"
		if updatesOnly {
			name = "legacy-updates-only"
		}
		t.Run(name, func(t *testing.T) { testDocumentSyncMetadata(t, updatesOnly) })
	}
}

func testDocumentSyncMetadata(t *testing.T, updatesOnly bool) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	_, err := pool.Exec(ctx, `
 ALTER TABLE snapshots ALTER COLUMN created_at SET DEFAULT clock_timestamp();
 ALTER TABLE updates ADD COLUMN blob bytea, ADD COLUMN IF NOT EXISTS created_at timestamptz, ADD COLUMN IF NOT EXISTS created_by text;
 ALTER TABLE workspace_access_policies ADD COLUMN member_default_doc_role text NOT NULL DEFAULT 'reader';
 CREATE TABLE workspace_sync_permission_generations(workspace_id text PRIMARY KEY,generation bigint NOT NULL DEFAULT 0);
 CREATE TABLE workspace_pages(workspace_id text REFERENCES workspaces(id),page_id text,mode smallint NOT NULL DEFAULT 0,title text,summary text,published_at timestamptz,PRIMARY KEY(workspace_id,page_id));
 CREATE TABLE doc_access_policies(workspace_id text,doc_id text,visibility text NOT NULL DEFAULT 'private',public_role text,member_default_role text,published_at timestamptz,url_preview_enabled boolean NOT NULL DEFAULT false,PRIMARY KEY(workspace_id,doc_id));
 CREATE TABLE doc_grants(workspace_id text,doc_id text,principal_type text,principal_id text,role text,created_at timestamptz DEFAULT now(),updated_at timestamptz DEFAULT now());`)
	if err != nil {
		t.Fatal(err)
	}
	applyInitialSchemaObjects(t, ctx, pool, "search_projection_jobs", "search_projection_units",
		"enqueue_search_projection_for_source_change", "snapshots_search_projection_enqueue", "updates_search_projection_enqueue")
	repo := workspaces.NewRepositoryWithDeployment(pool, "cloud")
	reader := workspaces.NewDocumentReadService(repo, nil)
	handler := NewHandlerWithServices(config.Config{Deployment: "cloud"}, auth.NewSessionResolver(pool), nil, repo, nil, nil, nil, nil, HandlerDependencies{DocumentReader: reader, WorkspacePermissionReader: workspaces.NewAuthorizationService(repo)})
	docs := documents.NewWorkspaceRepository(pool)
	worker := search.NewProjectionWorker(pool, docs)
	docID := "synced-doc"
	doc := ygo.NewDoc()
	blocks := ygo.NewMap(doc, "blocks")
	txn := doc.WriteTxn()
	root := blocks.SetMap(txn, "root")
	root.Set(txn, "sys:id", "root")
	root.Set(txn, "sys:flavour", "affine:page")
	root.Set(txn, "prop:title", "Synced title")
	txn.Commit()
	if _, err := docs.PushWorkspace(ctx, fixture.userID, fixture.workspaceID, docID, ygo.EncodeStateAsUpdate(doc), 0); err != nil {
		t.Fatal(err)
	}
	if updatesOnly {
		// Older databases can contain a document entirely in the updates table.
		if _, err := pool.Exec(ctx, `INSERT INTO updates(workspace_id,guid,blob,created_at,created_by)
   SELECT workspace_id,guid,blob,created_at,created_by FROM snapshots WHERE workspace_id=$1 AND guid=$2`, fixture.workspaceID, docID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM snapshots WHERE workspace_id=$1 AND guid=$2`, fixture.workspaceID, docID); err != nil {
			t.Fatal(err)
		}
	}
	if worked, err := worker.RunOne(ctx); err != nil || !worked {
		t.Fatalf("projection: %v %v", worked, err)
	}
	query := `query($w:String!){workspace(id:$w){docs(pagination:{first:10}){totalCount edges{node{id title updatedAt}}} recentlyUpdatedDocs(pagination:{first:10}){totalCount edges{node{id title updatedAt}}} doc(docId:"synced-doc"){title createdAt updatedAt lastUpdaterId meta{updatedAt updatedBy{name}}}}}`
	check := func(title string) map[string]any {
		t.Helper()
		result := graphRequest(t, handler, query, map[string]any{"w": fixture.workspaceID}, fixture.sessionID, fixture.userID)
		if result["errors"] != nil {
			t.Fatalf("metadata query: %#v", result)
		}
		w := result["data"].(map[string]any)["workspace"].(map[string]any)
		for _, field := range []string{"docs", "recentlyUpdatedDocs"} {
			list := w[field].(map[string]any)
			if list["totalCount"] != float64(1) || len(list["edges"].([]any)) != 1 {
				t.Fatalf("%s missing synced document: %#v", field, list)
			}
			node := list["edges"].([]any)[0].(map[string]any)["node"].(map[string]any)
			if node["title"] != title || node["updatedAt"] == nil {
				t.Fatalf("list metadata: %#v", node)
			}
		}
		info := w["doc"].(map[string]any)
		if info["title"] != title || info["createdAt"] == nil || info["updatedAt"] == nil {
			t.Fatalf("document metadata: %#v", info)
		}
		return info
	}
	first := check("Synced title")
	// Publication/ACL fields must survive content projection.
	if _, err := pool.Exec(ctx, `UPDATE workspace_pages SET mode=1,published_at='2026-01-01' WHERE workspace_id=$1 AND page_id=$2`, fixture.workspaceID, docID); err != nil {
		t.Fatal(err)
	}
	txn = doc.WriteTxn()
	root.Set(txn, "prop:title", "Edited title")
	txn.Commit()
	pushed, err := docs.PushWorkspace(ctx, fixture.sharedUserID, fixture.workspaceID, docID, ygo.EncodeStateAsUpdate(doc), 0)
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := worker.RunOne(ctx); err != nil || !worked {
		t.Fatalf("edit projection: %v %v", worked, err)
	}
	second := check("Edited title")
	got, err := time.Parse(time.RFC3339Nano, second["updatedAt"].(string))
	if err != nil || got.UnixMilli() != pushed.Timestamp {
		t.Fatalf("updatedAt=%v push=%v err=%v", second["updatedAt"], pushed.Timestamp, err)
	}
	if second["createdAt"] != first["createdAt"] || second["lastUpdaterId"] != fixture.sharedUserID {
		t.Fatalf("authors/timestamps: %#v", second)
	}
	meta := second["meta"].(map[string]any)
	if meta["updatedAt"] != second["updatedAt"] || meta["updatedBy"].(map[string]any)["name"] != "Shared User" {
		t.Fatalf("meta latest author: %#v", meta)
	}
	var mode int
	var published bool
	if err := pool.QueryRow(ctx, `SELECT mode,published_at IS NOT NULL FROM workspace_pages WHERE workspace_id=$1 AND page_id=$2`, fixture.workspaceID, docID).Scan(&mode, &published); err != nil || mode != 1 || !published {
		t.Fatalf("publication changed: %v %v %v", mode, published, err)
	}
}
