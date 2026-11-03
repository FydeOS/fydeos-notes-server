// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"testing"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/documents"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

func TestWorkspaceDocumentReadGraphQLContractAndPublicationFlow(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	_, err := pool.Exec(ctx, `ALTER TABLE workspace_access_policies ADD COLUMN member_default_doc_role text NOT NULL DEFAULT 'reader';
		CREATE TABLE workspace_sync_permission_generations(workspace_id text PRIMARY KEY,generation bigint NOT NULL DEFAULT 0);
		CREATE TABLE workspace_pages(workspace_id text NOT NULL,page_id text NOT NULL,mode smallint NOT NULL DEFAULT 0,title text,summary text,published_at timestamptz,PRIMARY KEY(workspace_id,page_id));
		CREATE TABLE doc_grants(workspace_id text,doc_id text,principal_type text,principal_id text,role text,created_at timestamptz NOT NULL DEFAULT now(),updated_at timestamptz NOT NULL DEFAULT now());
		CREATE TABLE doc_access_policies(workspace_id text,doc_id text,visibility text NOT NULL DEFAULT 'private',public_role text,member_default_role text,published_at timestamptz,url_preview_enabled boolean NOT NULL DEFAULT false,PRIMARY KEY(workspace_id,doc_id));`)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO workspace_access_policies(workspace_id,visibility,sharing_enabled,url_preview_enabled,member_default_doc_role) VALUES($1,'private',true,false,'reader')`, []any{fixture.workspaceID}},
		{`INSERT INTO entitlements(target_type,target_id,source,plan,quantity,status) VALUES('workspace',$1,'cloud_subscription','team',10,'active')`, []any{fixture.workspaceID}},
		{`INSERT INTO snapshots(workspace_id,guid,blob,created_at,updated_at,created_by,updated_by) VALUES($1,'doc','\x0000','2026-01-01T00:00:00Z','2026-01-02T00:00:00Z',$2,$3)`, []any{fixture.workspaceID, fixture.userID, fixture.sharedUserID}},
		{`INSERT INTO workspace_pages(workspace_id,page_id,title,summary,published_at) VALUES($1,'doc','Read title','Read summary',now())`, []any{fixture.workspaceID}},
		{`INSERT INTO doc_access_policies(workspace_id,doc_id,member_default_role,url_preview_enabled) VALUES($1,'doc','none',false)`, []any{fixture.workspaceID}},
		{`INSERT INTO doc_grants(workspace_id,doc_id,principal_type,principal_id,role,created_at) VALUES($1,'doc','user',$2,'reader','2026-01-03T00:00:00Z'),($1,'doc','user',$3,'editor','2026-01-04T00:00:00Z')`, []any{fixture.workspaceID, fixture.userID, fixture.sharedUserID}},
	} {
		if _, err := pool.Exec(ctx, entry.query, entry.args...); err != nil {
			t.Fatal(err)
		}
	}
	repo := workspaces.NewRepositoryWithDeployment(pool, "cloud")
	service := workspaces.NewDocumentReadService(repo, nil)
	if _, err := service.ReadDocument(ctx, fixture.userID, fixture.workspaceID, "doc"); err != nil {
		t.Fatalf("read document service: %v", err)
	}
	handler := NewHandlerWithServices(config.Config{Deployment: "cloud", Host: "localhost", Port: 3010}, auth.NewSessionResolver(pool), nil, repo, nil, nil, nil, nil, HandlerDependencies{
		DocumentReader:       service,
		DocumentPublications: DocumentPublicationDependencies{Service: workspaces.NewDocumentPublicationService(repo, documents.NewWorkspaceRepositoryWithEntitlementConfig(pool, "cloud"))},
	})
	query := `query($workspace:String!){workspace(id:$workspace){doc(docId:"doc"){id workspaceId mode public defaultRole title summary createdAt updatedAt creatorId lastUpdaterId createdBy{id name} lastUpdatedBy{id name} meta{createdAt updatedAt createdBy{name avatarUrl} updatedBy{name}} permissions{Doc_Read Doc_Copy Doc_Users_Read Doc_History_Read Doc_Update Doc_Publish Doc_TransferOwner Doc_Analytics_Read Doc_Analytics_Viewers_Read Doc_Preview} grantedUsersList(pagination:{first:1}){totalCount edges{cursor node{role user{id name email avatarUrl}}} pageInfo{startCursor endCursor hasNextPage hasPreviousPage}}} publicDocs{id}}}`
	vars := map[string]any{"workspace": fixture.workspaceID}
	result := graphRequest(t, handler, query, vars, fixture.sessionID, fixture.userID)
	if result["errors"] != nil {
		t.Fatalf("document read query=%#v", result)
	}
	workspace := result["data"].(map[string]any)["workspace"].(map[string]any)
	doc := workspace["doc"].(map[string]any)
	if doc["id"] != "doc" || doc["mode"] != "Page" || doc["public"] != false || doc["defaultRole"] != "None" || doc["title"] != "Read title" || doc["createdBy"].(map[string]any)["id"] != fixture.userID {
		t.Fatalf("document projection=%#v", doc)
	}
	meta := doc["meta"].(map[string]any)
	if meta["createdBy"].(map[string]any)["name"] != "Web User" || meta["updatedBy"].(map[string]any)["name"] != "Shared User" {
		t.Fatalf("meta projection=%#v", meta)
	}
	permissions := doc["permissions"].(map[string]any)
	for key, want := range map[string]bool{"Doc_Read": true, "Doc_Copy": true, "Doc_Users_Read": true, "Doc_History_Read": true, "Doc_Update": true, "Doc_Publish": true, "Doc_TransferOwner": true, "Doc_Analytics_Read": true, "Doc_Analytics_Viewers_Read": true, "Doc_Preview": false} {
		if permissions[key] != want {
			t.Errorf("permission %s=%v want %v", key, permissions[key], want)
		}
	}
	grants := doc["grantedUsersList"].(map[string]any)
	if grants["totalCount"].(float64) != 2 {
		t.Fatalf("grant list=%#v", grants)
	}
	if workspace["publicDocs"].([]any) == nil || len(workspace["publicDocs"].([]any)) != 0 {
		t.Fatalf("private document appeared in public list: %#v", workspace["publicDocs"])
	}
	for _, statement := range []string{`UPDATE doc_access_policies SET visibility='public',public_role='external',published_at=now() WHERE workspace_id=$1 AND doc_id='doc'`, `UPDATE workspace_pages SET published_at=now() WHERE workspace_id=$1 AND page_id='doc'`} {
		if _, err := pool.Exec(ctx, statement, fixture.workspaceID); err != nil {
			t.Fatal(err)
		}
	}
	query = `query($workspace:String!){workspace(id:$workspace){doc(docId:"doc"){public permissions{Doc_Read Doc_Update Doc_Preview}} publicDocs{id mode}}}`
	result = graphRequest(t, handler, query, vars, fixture.sessionID, fixture.userID)
	if result["errors"] != nil {
		t.Fatalf("public read=%#v", result)
	}
	workspace = result["data"].(map[string]any)["workspace"].(map[string]any)
	if workspace["doc"].(map[string]any)["public"] != true || len(workspace["publicDocs"].([]any)) != 1 {
		t.Fatalf("published state=%#v", workspace)
	}
	for _, statement := range []string{`UPDATE doc_access_policies SET visibility='private',public_role=NULL,published_at=NULL WHERE workspace_id=$1 AND doc_id='doc'`, `UPDATE workspace_pages SET published_at=NULL WHERE workspace_id=$1 AND page_id='doc'`} {
		if _, err := pool.Exec(ctx, statement, fixture.workspaceID); err != nil {
			t.Fatal(err)
		}
	}
	query = `query($workspace:String!){workspace(id:$workspace){publicDocs{id}}}`
	result = graphRequest(t, handler, query, vars, fixture.sessionID, fixture.userID)
	if result["errors"] != nil || len(result["data"].(map[string]any)["workspace"].(map[string]any)["publicDocs"].([]any)) != 0 {
		t.Fatalf("revoked list=%#v", result)
	}
}

func TestWorkspacePublicReadAndDocumentPermissionDenial(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	_, err := pool.Exec(ctx, `ALTER TABLE workspace_access_policies ADD COLUMN member_default_doc_role text NOT NULL DEFAULT 'none';
		CREATE TABLE workspace_sync_permission_generations(workspace_id text PRIMARY KEY,generation bigint NOT NULL DEFAULT 0);
		CREATE TABLE doc_grants(workspace_id text,doc_id text,principal_type text,principal_id text,role text);
		CREATE TABLE workspace_pages(workspace_id text NOT NULL,page_id text NOT NULL,mode smallint NOT NULL DEFAULT 0,title text,summary text,published_at timestamptz,PRIMARY KEY(workspace_id,page_id));
		CREATE TABLE doc_access_policies(workspace_id text,doc_id text,visibility text NOT NULL DEFAULT 'private',public_role text,member_default_role text,published_at timestamptz,url_preview_enabled boolean NOT NULL DEFAULT false,PRIMARY KEY(workspace_id,doc_id));
		`)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO workspace_access_policies(workspace_id,visibility,sharing_enabled,url_preview_enabled,member_default_doc_role) VALUES($1,'public',true,false,'none')`, []any{fixture.workspaceID}},
		{`INSERT INTO doc_access_policies(workspace_id,doc_id,visibility,member_default_role) VALUES($1,'private','private','none')`, []any{fixture.workspaceID}},
		{`INSERT INTO snapshots(workspace_id,guid,blob) VALUES($1,'private','\\x0000')`, []any{fixture.workspaceID}},
		{`INSERT INTO workspace_pages(workspace_id,page_id) VALUES($1,'private')`, []any{fixture.workspaceID}},
	} {
		if _, err := pool.Exec(ctx, entry.query, entry.args...); err != nil {
			t.Fatal(err)
		}
	}
	repo := workspaces.NewRepositoryWithDeployment(pool, "cloud")
	reader := workspaces.NewDocumentReadService(repo, nil)
	handler := NewHandlerWithServices(config.Config{Deployment: "cloud", Host: "localhost", Port: 3010}, auth.NewSessionResolver(pool), nil, repo, nil, nil, nil, nil, HandlerDependencies{DocumentReader: reader})
	query := `query($workspace:String!){workspace(id:$workspace){id}}`
	result := graphRequest(t, handler, query, map[string]any{"workspace": fixture.workspaceID}, "", "")
	if result["errors"] == nil {
		t.Fatalf("anonymous workspace read unexpectedly succeeded: %#v", result)
	}
	firstError := result["errors"].([]any)[0].(map[string]any)
	if extensions, ok := firstError["extensions"].(map[string]any); ok && extensions["name"] != "AUTHENTICATION_REQUIRED" {
		t.Fatalf("anonymous workspace read=%#v", result)
	}
	result = graphRequest(t, handler, query, map[string]any{"workspace": fixture.workspaceID}, fixture.expiredSessionID, fixture.userID)
	if result["errors"] == nil {
		t.Fatalf("expired session workspace read=%#v", result)
	}
	result = graphRequest(t, handler, query, map[string]any{"workspace": fixture.workspaceID}, fixture.sessionID, fixture.sharedUserID)
	if result["errors"] != nil {
		t.Fatalf("public workspace read=%#v", result)
	}
	query = `query($workspace:String!){workspace(id:$workspace){doc(docId:"private"){id}}}`
	result = graphRequest(t, handler, query, map[string]any{"workspace": fixture.workspaceID}, fixture.sessionID, fixture.sharedUserID)
	if result["errors"] == nil || result["errors"].([]any)[0].(map[string]any)["extensions"].(map[string]any)["name"] != "doc_action_denied" {
		t.Fatalf("private document denial=%#v", result)
	}
}
