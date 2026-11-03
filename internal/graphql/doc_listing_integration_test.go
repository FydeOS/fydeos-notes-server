// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"testing"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

func TestWorkspaceDocumentListingGraphQLParity(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	for _, statement := range []string{
		`ALTER TABLE workspace_access_policies ADD COLUMN member_default_doc_role text NOT NULL DEFAULT 'reader'`,
		`CREATE TABLE workspace_sync_permission_generations(workspace_id text PRIMARY KEY,generation bigint NOT NULL DEFAULT 0)`,
		`CREATE TABLE workspace_pages(workspace_id text NOT NULL,page_id text NOT NULL,mode smallint NOT NULL DEFAULT 0,title text,summary text,published_at timestamptz,PRIMARY KEY(workspace_id,page_id))`,
		`CREATE TABLE doc_access_policies(workspace_id text,doc_id text,visibility text NOT NULL DEFAULT 'private',public_role text,member_default_role text,published_at timestamptz,url_preview_enabled boolean NOT NULL DEFAULT false,PRIMARY KEY(workspace_id,doc_id))`,
		`CREATE TABLE doc_grants(workspace_id text,doc_id text,principal_type text,principal_id text,role text,created_at timestamptz NOT NULL DEFAULT now(),updated_at timestamptz NOT NULL DEFAULT now())`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_access_policies(workspace_id,visibility,sharing_enabled,url_preview_enabled,member_default_doc_role) VALUES($1,'private',true,false,'reader')`, fixture.workspaceID); err != nil {
		t.Fatal(err)
	}
	for _, doc := range []struct {
		id, title, created, updated, visibility, publicRole string
	}{
		{"doc-old", "Old", "2026-01-01T00:00:00Z", "2026-01-03T00:00:00Z", "private", ""},
		{"doc-private", "Private", "2026-01-02T00:00:00Z", "2026-01-05T00:00:00Z", "private", ""},
		{"doc-public", "Public", "2026-01-03T00:00:00Z", "2026-01-04T00:00:00Z", "public", "external"},
	} {
		if _, err := pool.Exec(ctx, `INSERT INTO snapshots(workspace_id,guid,blob,created_at,updated_at,created_by,updated_by) VALUES($1,$2,'\\x0000',$3,$4,$5,$5)`,
			fixture.workspaceID, doc.id, doc.created, doc.updated, fixture.userID); err != nil {
			t.Fatalf("seed snapshot %s: %v", doc.id, err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO workspace_pages(workspace_id,page_id,title,published_at) VALUES($1,$2,$3,CASE WHEN $4='public' THEN $5::timestamptz END)`, fixture.workspaceID, doc.id, doc.title, doc.visibility, doc.updated); err != nil {
			t.Fatalf("seed workspace page %s: %v", doc.id, err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO doc_access_policies(workspace_id,doc_id,visibility,public_role,member_default_role,published_at) VALUES($1,$2,$3,NULLIF($4,''),'none',CASE WHEN $3='public' THEN $5::timestamptz END)`,
			fixture.workspaceID, doc.id, doc.visibility, doc.publicRole, doc.updated); err != nil {
			t.Fatalf("seed document %s: %v", doc.id, err)
		}
	}
	repo := workspaces.NewRepositoryWithDeployment(pool, "cloud")
	reader := workspaces.NewDocumentReadService(repo, nil)
	handler := NewHandlerWithServices(config.Config{Deployment: "cloud"}, auth.NewSessionResolver(pool), nil, repo, nil, nil, nil, nil, HandlerDependencies{
		DocumentReader:            reader,
		WorkspacePermissionReader: workspaces.NewAuthorizationService(repo),
	})

	query := `query($workspace:String!,$pagination:PaginationInput!){workspace(id:$workspace){docs(pagination:$pagination){totalCount pageInfo{hasNextPage hasPreviousPage endCursor} edges{cursor node{id title creatorId lastUpdaterId}}}}}`
	result := graphRequest(t, handler, query, map[string]any{"workspace": fixture.workspaceID, "pagination": map[string]any{"first": 1}}, fixture.sessionID, fixture.userID)
	if result["errors"] != nil {
		t.Fatalf("owner document list=%#v", result)
	}
	page := result["data"].(map[string]any)["workspace"].(map[string]any)["docs"].(map[string]any)
	if page["totalCount"] != float64(3) || page["edges"].([]any)[0].(map[string]any)["node"].(map[string]any)["id"] != "doc-old" {
		t.Fatalf("created-at page=%#v", page)
	}
	cursor := page["pageInfo"].(map[string]any)["endCursor"].(string)
	result = graphRequest(t, handler, query, map[string]any{"workspace": fixture.workspaceID, "pagination": map[string]any{"first": 2, "after": cursor}}, fixture.sessionID, fixture.userID)
	if result["errors"] != nil {
		t.Fatalf("next document page=%#v", result)
	}
	page = result["data"].(map[string]any)["workspace"].(map[string]any)["docs"].(map[string]any)
	if len(page["edges"].([]any)) != 2 || page["edges"].([]any)[0].(map[string]any)["node"].(map[string]any)["id"] != "doc-private" {
		t.Fatalf("cursor page=%#v", page)
	}
	denied := graphRequest(t, handler, query, map[string]any{"workspace": fixture.workspaceID, "pagination": map[string]any{"first": 10}}, fixture.sessionID, fixture.sharedUserID)
	if denied["errors"] == nil {
		t.Fatalf("collaborator document management list unexpectedly succeeded: %#v", denied)
	}

	query = `query($workspace:String!,$pagination:PaginationInput!){workspace(id:$workspace){recentlyUpdatedDocs(pagination:$pagination){totalCount edges{cursor node{id title updatedAt}} pageInfo{hasNextPage}}}}`
	result = graphRequest(t, handler, query, map[string]any{"workspace": fixture.workspaceID, "pagination": map[string]any{"first": 10}}, fixture.sessionID, fixture.sharedUserID)
	if result["errors"] != nil {
		t.Fatalf("readable updated document list=%#v", result)
	}
	page = result["data"].(map[string]any)["workspace"].(map[string]any)["recentlyUpdatedDocs"].(map[string]any)
	if page["totalCount"] != float64(1) || len(page["edges"].([]any)) != 1 || page["edges"].([]any)[0].(map[string]any)["node"].(map[string]any)["id"] != "doc-public" {
		t.Fatalf("readable updated docs=%#v", page)
	}
}
