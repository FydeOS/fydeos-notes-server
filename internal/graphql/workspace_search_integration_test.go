// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/search"
	"github.com/fydeos/fydeos-notes-server/internal/serverconfig"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

type searchDocsReaderSpy struct {
	*workspaces.DocumentReadService
	singlePermissionCalls int
	batchPermissionCalls  int
	singleAuthorCalls     int
	batchAuthorCalls      int
	batchPermissionErr    error
}

func (s *searchDocsReaderSpy) ReadDocumentPermissions(ctx context.Context, actor, workspace, doc string) (map[string]bool, error) {
	s.singlePermissionCalls++
	return s.DocumentReadService.ReadDocumentPermissions(ctx, actor, workspace, doc)
}

func (s *searchDocsReaderSpy) ReadDocumentPermissionsBatch(ctx context.Context, actor, workspace string, docIDs []string) (map[string]map[string]bool, error) {
	s.batchPermissionCalls++
	if s.batchPermissionErr != nil {
		return nil, s.batchPermissionErr
	}
	return s.DocumentReadService.ReadDocumentPermissionsBatch(ctx, actor, workspace, docIDs)
}

func (s *searchDocsReaderSpy) ReadDocumentAuthors(ctx context.Context, workspace, doc string) (workspaces.DocumentAuthors, error) {
	s.singleAuthorCalls++
	return s.DocumentReadService.ReadDocumentAuthors(ctx, workspace, doc)
}

func (s *searchDocsReaderSpy) ReadDocumentAuthorsBatch(ctx context.Context, workspace string, docIDs []string) (map[string]workspaces.DocumentAuthors, error) {
	s.batchAuthorCalls++
	return s.DocumentReadService.ReadDocumentAuthorsBatch(ctx, workspace, docIDs)
}

func TestWorkspaceSearchDocsFiltersIndexedCandidatesThroughCanonicalDocACL(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	if _, err := pool.Exec(ctx, `UPDATE workspace_members SET role='member' WHERE workspace_id=$1 AND user_id=$2`, fixture.workspaceID, fixture.sharedUserID); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`ALTER TABLE workspace_access_policies ADD COLUMN member_default_doc_role text NOT NULL DEFAULT 'none'`,
		`CREATE TABLE workspace_sync_permission_generations(workspace_id text PRIMARY KEY,generation bigint NOT NULL DEFAULT 0)`,
		`CREATE TABLE workspace_pages(workspace_id text NOT NULL,page_id text NOT NULL,mode smallint NOT NULL DEFAULT 0,title text,summary text,published_at timestamptz,PRIMARY KEY(workspace_id,page_id))`,
		`CREATE TABLE doc_access_policies(workspace_id text NOT NULL,doc_id text NOT NULL,visibility text NOT NULL DEFAULT 'private',public_role text,member_default_role text NOT NULL DEFAULT 'none',published_at timestamptz,url_preview_enabled boolean NOT NULL DEFAULT false,PRIMARY KEY(workspace_id,doc_id))`,
		`CREATE TABLE doc_grants(workspace_id text NOT NULL,doc_id text NOT NULL,principal_type text NOT NULL,principal_id text NOT NULL,role text NOT NULL,created_at timestamptz NOT NULL DEFAULT now(),updated_at timestamptz NOT NULL DEFAULT now())`,
		`CREATE TABLE search_projection_units(workspace_id text NOT NULL,doc_id text NOT NULL,block_id text NOT NULL,unit_id text NOT NULL,flavour text NOT NULL,title text NOT NULL DEFAULT '',summary text NOT NULL DEFAULT '',content text NOT NULL,blob text[] NOT NULL DEFAULT '{}',ref_doc_id text[] NOT NULL DEFAULT '{}',ref text[] NOT NULL DEFAULT '{}',parent_flavour text NOT NULL DEFAULT '',parent_block_id text NOT NULL DEFAULT '',additional text NOT NULL DEFAULT '{}',markdown_preview text NOT NULL DEFAULT '',updated_at timestamptz NOT NULL,content_vector tsvector GENERATED ALWAYS AS(to_tsvector('simple',coalesce(title,'') || ' ' || coalesce(summary,'') || ' ' || coalesce(content,''))) STORED,PRIMARY KEY(workspace_id,doc_id,unit_id))`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_access_policies(workspace_id,visibility,sharing_enabled,url_preview_enabled,member_default_doc_role) VALUES($1,'private',true,false,'none')`, fixture.workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO entitlements(target_type,target_id,source,plan,quantity,status) VALUES('workspace',$1,'cloud_subscription','team',10,'active')`, fixture.workspaceID); err != nil {
		t.Fatal(err)
	}
	docIDs := []string{"allowed-doc", "denied-doc"}
	for i := 0; i < 100; i++ {
		docIDs = append(docIDs, fmt.Sprintf("aaa-denied-%03d", i))
	}
	for _, docID := range docIDs {
		if _, err := pool.Exec(ctx, `INSERT INTO snapshots(workspace_id,guid,blob,created_at,updated_at,created_by,updated_by) VALUES($1,$2,'\x0000',now(),now(),$3,$3)`, fixture.workspaceID, docID, fixture.userID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO workspace_pages(workspace_id,page_id,title) VALUES($1,$2,$2)`, fixture.workspaceID, docID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO doc_access_policies(workspace_id,doc_id,member_default_role) VALUES($1,$2,'none')`, fixture.workspaceID, docID); err != nil {
			t.Fatal(err)
		}
		content := "needle indexed text"
		if docID == "allowed-doc" {
			content = `needle indexed <img src=x onerror=alert(1)>`
		}
		if _, err := pool.Exec(ctx, `INSERT INTO search_projection_units(workspace_id,doc_id,block_id,unit_id,flavour,title,content,updated_at) VALUES($1,$2,'paragraph','paragraph','affine:paragraph',$2,$3,now())`, fixture.workspaceID, docID, content); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO doc_grants(workspace_id,doc_id,principal_type,principal_id,role) VALUES($1,'allowed-doc','user',$2,'reader')`, fixture.workspaceID, fixture.sharedUserID); err != nil {
		t.Fatal(err)
	}
	repository := workspaces.NewRepositoryWithDeployment(pool, "cloud")
	reader := &searchDocsReaderSpy{DocumentReadService: workspaces.NewDocumentReadService(repository, nil)}
	handler := NewHandlerWithServices(config.Config{Deployment: "cloud"}, auth.NewSessionResolver(pool), nil, repository, nil, nil, nil, nil, HandlerDependencies{
		DocumentReader:            reader,
		WorkspacePermissionReader: workspaces.NewAuthorizationService(repository),
		SearchDocs:                search.NewDocsService(pool),
	})
	query := `query($workspace:String!,$input:SearchDocsInput!){workspace(id:$workspace){searchDocs(input:$input){docId title blockId highlight createdAt updatedAt createdByUser{id name} updatedByUser{id name}}}}`
	result := graphRequest(t, handler, query, map[string]any{
		"workspace": fixture.workspaceID,
		"input":     map[string]any{"keyword": "needle indexed", "limit": 20},
	}, fixture.sessionID, fixture.sharedUserID)
	if result["errors"] != nil {
		t.Fatalf("searchDocs query=%#v", result)
	}
	items := result["data"].(map[string]any)["workspace"].(map[string]any)["searchDocs"].([]any)
	if len(items) != 1 {
		t.Fatalf("searchDocs returned %d rows, expected only the ACL-readable hit: %#v", len(items), items)
	}
	item := items[0].(map[string]any)
	if item["docId"] != "allowed-doc" || item["blockId"] != "paragraph" {
		t.Fatalf("searchDocs visible hit=%#v", item)
	}
	highlight := item["highlight"].(string)
	if strings.Contains(highlight, "<img") || !strings.Contains(highlight, "&lt;img") || !strings.Contains(highlight, "<b>") {
		t.Fatalf("searchDocs highlight contains unsafe or missing markup: %q", highlight)
	}
	if item["createdByUser"].(map[string]any)["name"] != "Web User" || item["updatedByUser"].(map[string]any)["name"] != "Web User" {
		t.Fatalf("searchDocs author projection=%#v", item)
	}
	if reader.batchPermissionCalls != 2 || reader.batchAuthorCalls != 2 || reader.singlePermissionCalls != 0 || reader.singleAuthorCalls != 0 {
		t.Fatalf("searchDocs used non-batched reads: %#v", reader)
	}
	reader.batchPermissionErr = workspaces.ErrNotFound
	missingWorkspace := graphRequest(t, handler, query, map[string]any{
		"workspace": fixture.workspaceID,
		"input":     map[string]any{"keyword": "needle indexed", "limit": 20},
	}, fixture.sessionID, fixture.sharedUserID)
	if missingWorkspace["errors"] == nil {
		t.Fatalf("missing workspace after the initial access check returned success: %#v", missingWorkspace)
	}
	workspaceError := missingWorkspace["errors"].([]any)[0].(map[string]any)["extensions"].(map[string]any)
	if workspaceError["type"] != "resource_not_found" || workspaceError["name"] != "space_not_found" {
		t.Fatalf("missing workspace search error = %#v", workspaceError)
	}
}

func TestWorkspaceSearchAndAggregateFilterByCanonicalDocACL(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	if _, err := pool.Exec(ctx, `INSERT INTO app_configs(id,value) VALUES ('indexer.enabled','true'::jsonb) ON CONFLICT (id) DO UPDATE SET value=EXCLUDED.value`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspace_members SET role='member' WHERE workspace_id=$1 AND user_id=$2`, fixture.workspaceID, fixture.sharedUserID); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`ALTER TABLE workspace_access_policies ADD COLUMN member_default_doc_role text NOT NULL DEFAULT 'none'`,
		`CREATE TABLE workspace_sync_permission_generations(workspace_id text PRIMARY KEY,generation bigint NOT NULL DEFAULT 0)`,
		`CREATE TABLE workspace_pages(workspace_id text NOT NULL,page_id text NOT NULL,mode smallint NOT NULL DEFAULT 0,title text,summary text,published_at timestamptz,PRIMARY KEY(workspace_id,page_id))`,
		`CREATE TABLE doc_access_policies(workspace_id text NOT NULL,doc_id text NOT NULL,visibility text NOT NULL DEFAULT 'private',public_role text,member_default_role text NOT NULL DEFAULT 'none',published_at timestamptz,url_preview_enabled boolean NOT NULL DEFAULT false,PRIMARY KEY(workspace_id,doc_id))`,
		`CREATE TABLE doc_grants(workspace_id text NOT NULL,doc_id text NOT NULL,principal_type text NOT NULL,principal_id text NOT NULL,role text NOT NULL,created_at timestamptz NOT NULL DEFAULT now(),updated_at timestamptz NOT NULL DEFAULT now())`,
		`CREATE TABLE search_projection_units(workspace_id text NOT NULL,doc_id text NOT NULL,block_id text NOT NULL,unit_id text NOT NULL,flavour text NOT NULL,title text NOT NULL DEFAULT '',summary text NOT NULL DEFAULT '',content text NOT NULL,blob text[] NOT NULL DEFAULT '{}',ref_doc_id text[] NOT NULL DEFAULT '{}',ref text[] NOT NULL DEFAULT '{}',parent_flavour text NOT NULL DEFAULT '',parent_block_id text NOT NULL DEFAULT '',additional text NOT NULL DEFAULT '{}',markdown_preview text NOT NULL DEFAULT '',updated_at timestamptz NOT NULL,content_vector tsvector GENERATED ALWAYS AS(to_tsvector('simple',coalesce(title,'') || ' ' || coalesce(summary,'') || ' ' || coalesce(content,''))) STORED,PRIMARY KEY(workspace_id,doc_id,unit_id))`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_access_policies(workspace_id,visibility,sharing_enabled,url_preview_enabled,member_default_doc_role) VALUES($1,'private',true,false,'none')`, fixture.workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO entitlements(target_type,target_id,source,plan,quantity,status) VALUES('workspace',$1,'cloud_subscription','team',10,'active')`, fixture.workspaceID); err != nil {
		t.Fatal(err)
	}
	for _, docID := range []string{"allowed-doc", "denied-doc"} {
		if _, err := pool.Exec(ctx, `INSERT INTO snapshots(workspace_id,guid,blob,created_at,updated_at,created_by,updated_by) VALUES($1,$2,'\x0000',now(),now(),$3,$3)`, fixture.workspaceID, docID, fixture.userID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO workspace_pages(workspace_id,page_id,title) VALUES($1,$2,$2)`, fixture.workspaceID, docID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO doc_access_policies(workspace_id,doc_id,member_default_role) VALUES($1,$2,'none')`, fixture.workspaceID, docID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO search_projection_units(workspace_id,doc_id,block_id,unit_id,flavour,title,content,updated_at) VALUES($1,$2,'block-1','unit-1',$3,$2,'needle indexed text',now())`, fixture.workspaceID, docID, map[string]string{"allowed-doc": "affine:paragraph", "denied-doc": "affine:code"}[docID]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO doc_grants(workspace_id,doc_id,principal_type,principal_id,role) VALUES($1,'allowed-doc','user',$2,'reader')`, fixture.workspaceID, fixture.sharedUserID); err != nil {
		t.Fatal(err)
	}
	repository := workspaces.NewRepositoryWithDeployment(pool, "cloud")
	reader := workspaces.NewDocumentReadService(repository, nil)
	handler := NewHandlerWithServices(config.Config{Deployment: "cloud"}, auth.NewSessionResolver(pool), nil, repository, nil, nil, nil, nil, HandlerDependencies{
		DocumentReader:            reader,
		WorkspacePermissionReader: workspaces.NewAuthorizationService(repository),
		SearchProjection:          search.NewProjectionService(pool),
		SearchDocs:                search.NewDocsService(pool),
		ServerConfig:              serverconfig.New(config.Config{Deployment: "cloud"}, pool),
	})
	features := queryPublicServerConfig(t, handler, `{serverConfig{features}}`)["features"]
	if !equalServerFeatureSet(features, "Indexer", "LocalWorkspace") {
		t.Fatalf("server config omitted the active PostgreSQL indexer feature: %#v", features)
	}
	searchQuery := `query($workspace:String!,$input:SearchInput!){workspace(id:$workspace){search(input:$input){nodes{fields highlights} pagination{count hasMore nextCursor}}}}`
	searchResult := graphRequest(t, handler, searchQuery, map[string]any{
		"workspace": fixture.workspaceID,
		"input": map[string]any{
			"table": "block", "query": map[string]any{"type": "match", "field": "content", "match": "needle"},
			"options": map[string]any{"fields": []string{"docId", "flavour"}, "highlights": []any{map[string]any{"field": "content", "before": "<b>", "end": "</b>"}}},
		},
	}, fixture.sessionID, fixture.sharedUserID)
	if searchResult["errors"] != nil {
		t.Fatalf("search query=%#v", searchResult)
	}
	searchData := searchResult["data"].(map[string]any)["workspace"].(map[string]any)["search"].(map[string]any)
	searchNodes := searchData["nodes"].([]any)
	if len(searchNodes) != 1 {
		t.Fatalf("search returned %d rows, expected one ACL-readable hit: %#v", len(searchNodes), searchData)
	}
	searchFields := searchNodes[0].(map[string]any)["fields"].(map[string]any)
	if searchFields["docId"].([]any)[0] != "allowed-doc" || searchFields["flavour"].([]any)[0] != "affine:paragraph" {
		t.Fatalf("search fields=%#v", searchFields)
	}
	searchHighlights := searchNodes[0].(map[string]any)["highlights"].(map[string]any)
	if searchHighlights["content"].([]any)[0] != "<b>needle</b> indexed text" {
		t.Fatalf("search highlights=%#v", searchHighlights)
	}

	aggregateQuery := `query($workspace:String!,$input:AggregateInput!){workspace(id:$workspace){aggregate(input:$input){buckets{key count hits{nodes{fields}}} pagination{count hasMore}}}}`
	aggregateResult := graphRequest(t, handler, aggregateQuery, map[string]any{
		"workspace": fixture.workspaceID,
		"input": map[string]any{
			"table": "block", "field": "flavour", "query": map[string]any{"type": "all"},
			"options": map[string]any{"hits": map[string]any{"fields": []string{"docId", "flavour"}}},
		},
	}, fixture.sessionID, fixture.sharedUserID)
	if aggregateResult["errors"] != nil {
		t.Fatalf("aggregate query=%#v", aggregateResult)
	}
	aggregate := aggregateResult["data"].(map[string]any)["workspace"].(map[string]any)["aggregate"].(map[string]any)
	buckets := aggregate["buckets"].([]any)
	if len(buckets) != 1 || buckets[0].(map[string]any)["key"] != "affine:paragraph" || buckets[0].(map[string]any)["count"] != float64(1) {
		t.Fatalf("aggregate exposed denied document in bucket counts: %#v", aggregate)
	}
	runWorkspaceSearchCapacity(t, pool, handler, fixture)
}
