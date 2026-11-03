// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/users"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

func TestAdminAllSharedLinksGraphQLAuthorizationAndPagination(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	now := time.Now().UTC().Truncate(time.Millisecond)
	if _, err := pool.Exec(ctx, `
		CREATE TABLE workspace_pages (
			workspace_id text NOT NULL, page_id text NOT NULL, mode smallint NOT NULL DEFAULT 0,
			title text, summary text, published_at timestamptz, PRIMARY KEY(workspace_id,page_id)
		);
		CREATE TABLE doc_access_policies (
			workspace_id text NOT NULL, doc_id text NOT NULL, visibility text NOT NULL,
			public_role text, published_at timestamptz, PRIMARY KEY(workspace_id,doc_id)
		);
		CREATE TABLE workspace_doc_view_daily (
			workspace_id text NOT NULL, doc_id text NOT NULL, date date NOT NULL,
			total_views bigint NOT NULL DEFAULT 0, unique_views bigint NOT NULL DEFAULT 0,
			guest_views bigint NOT NULL DEFAULT 0, last_accessed_at timestamptz,
			updated_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY(workspace_id,doc_id,date)
		);`); err != nil {
		t.Fatal(err)
	}
	for _, doc := range []struct {
		id, title string
		updated   time.Time
		published time.Time
		views     int
	}{
		{"doc-a", "Older doc", now.Add(-48 * time.Hour), now.Add(-time.Hour), 4},
		{"doc-b", "Newer doc", now.Add(-24 * time.Hour), now.Add(-2 * time.Hour), 9},
	} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspace_pages(workspace_id,page_id,title,published_at) VALUES($1,$2,$3,$4)`, fixture.workspaceID, doc.id, doc.title, doc.published); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO doc_access_policies(workspace_id,doc_id,visibility,public_role,published_at) VALUES($1,$2,'public','external',$3)`, fixture.workspaceID, doc.id, doc.published); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO snapshots(workspace_id,guid,created_at,updated_at,updated_by) VALUES($1,$2,$3,$4,$5)`, fixture.workspaceID, doc.id, doc.updated.Add(-time.Hour), doc.updated, fixture.userID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO workspace_doc_view_daily(workspace_id,doc_id,date,total_views,unique_views,guest_views,last_accessed_at) VALUES($1,$2,current_date,$3,$4,$5,$6)`, fixture.workspaceID, doc.id, doc.views, doc.views-1, 1, doc.updated); err != nil {
			t.Fatal(err)
		}
	}
	workspaceRepository := workspaces.NewRepositoryWithDeployment(pool, "affine")
	userRepository := users.NewRepository(pool)
	userService := users.NewService(userRepository, workspaces.NewAuthorizationService(workspaceRepository))
	analytics := workspaces.NewDocumentAnalyticsService(workspaceRepository, nil)
	dependencies := HandlerDependencies{DocumentAnalytics: analytics}
	newHandler := func(deployment string) http.Handler {
		return NewHandlerWithServices(
			config.Config{Deployment: deployment, Host: "localhost", ExternalURL: "https://notes.example.test/base"},
			auth.NewSessionResolver(pool), userService, workspaceRepository, auth.NewReadLimiter(auth.DefaultReadConcurrency), nil, nil, nil, dependencies,
		)
	}
	query := `query SharedLinks($pagination: PaginationInput!, $filter: AdminAllSharedLinksFilterInput) {
		adminAllSharedLinks(pagination:$pagination, filter:$filter) {
			totalCount analyticsWindow { timezone requestedSize effectiveSize }
			pageInfo { hasNextPage hasPreviousPage endCursor }
			edges { cursor node { workspaceId docId title shareUrl views uniqueViews guestViews } }
		}
	}`
	first := graphRequest(t, newHandler("affine"), query, map[string]any{
		"pagination": map[string]any{"first": 1, "offset": 0},
		"filter":     map[string]any{"workspaceId": fixture.workspaceID, "includeTotal": false, "analyticsWindowDays": 999},
	}, fixture.sessionID, fixture.userID)
	if first["errors"] != nil {
		t.Fatalf("admin shared links errors=%#v", first["errors"])
	}
	page := first["data"].(map[string]any)["adminAllSharedLinks"].(map[string]any)
	if page["totalCount"] != nil || page["analyticsWindow"].(map[string]any)["effectiveSize"] != float64(90) {
		t.Fatalf("first page fields=%#v", page)
	}
	edges := page["edges"].([]any)
	if len(edges) != 1 {
		t.Fatalf("first edge count=%d", len(edges))
	}
	node := edges[0].(map[string]any)["node"].(map[string]any)
	if node["docId"] != "doc-b" || node["shareUrl"] != "https://notes.example.test/workspace/"+fixture.workspaceID+"/doc-b" || node["views"] != float64(9) {
		t.Fatalf("first node=%#v", node)
	}
	cursor := page["pageInfo"].(map[string]any)["endCursor"].(string)
	second := graphRequest(t, newHandler("affine"), query, map[string]any{
		"pagination": map[string]any{"first": 1, "after": cursor},
		"filter":     map[string]any{"workspaceId": fixture.workspaceID, "includeTotal": true},
	}, fixture.sessionID, fixture.userID)
	if second["errors"] != nil {
		t.Fatalf("second page errors=%#v", second["errors"])
	}
	secondPage := second["data"].(map[string]any)["adminAllSharedLinks"].(map[string]any)
	if secondPage["totalCount"] != float64(2) || secondPage["pageInfo"].(map[string]any)["hasPreviousPage"] != true || secondPage["edges"].([]any)[0].(map[string]any)["node"].(map[string]any)["docId"] != "doc-a" {
		t.Fatalf("second page=%#v", secondPage)
	}
	for _, test := range []struct {
		name       string
		handler    http.Handler
		session    string
		user       string
		statusName string
		status     float64
	}{
		{name: "non-admin", handler: newHandler("affine"), session: fixture.sessionID, user: fixture.sharedUserID, statusName: "action_forbidden", status: http.StatusForbidden},
		{name: "anonymous", handler: newHandler("affine"), statusName: "action_forbidden", status: http.StatusForbidden},
		{name: "self-hosted", handler: newHandler("selfhosted"), session: fixture.sessionID, user: fixture.userID, statusName: "NOT_FOUND", status: http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := graphRequest(t, test.handler, query, map[string]any{"pagination": map[string]any{"first": 1}}, test.session, test.user)
			errors := response["errors"].([]any)
			extensions := errors[0].(map[string]any)["extensions"].(map[string]any)
			if extensions["name"] != test.statusName || extensions["status"] != test.status {
				t.Fatalf("error extensions=%#v", extensions)
			}
		})
	}
}
