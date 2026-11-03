// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/users"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

func TestAdminDashboardGraphQLCompatibilityAndAuthorization(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	if _, err := pool.Exec(ctx, `
		CREATE TABLE workspace_admin_stats (snapshot_size bigint NOT NULL, blob_size bigint NOT NULL);
		CREATE TABLE workspace_admin_stats_daily (date date NOT NULL, snapshot_size bigint NOT NULL, blob_size bigint NOT NULL);
		CREATE TABLE sync_active_users_minutely (minute_ts timestamptz PRIMARY KEY, active_users integer NOT NULL, updated_at timestamptz NOT NULL DEFAULT now());
		CREATE TABLE ai_sessions_messages (role text NOT NULL, created_at timestamptz NOT NULL);
		CREATE TABLE workspace_pages (workspace_id text NOT NULL, page_id text NOT NULL, title text, published_at timestamptz, PRIMARY KEY(workspace_id,page_id));
		CREATE TABLE doc_access_policies (workspace_id text NOT NULL, doc_id text NOT NULL, visibility text NOT NULL, public_role text, PRIMARY KEY(workspace_id,doc_id));
		CREATE TABLE workspace_doc_view_daily (workspace_id text NOT NULL, doc_id text NOT NULL, date date NOT NULL, total_views bigint NOT NULL, unique_views bigint NOT NULL, guest_views bigint NOT NULL, last_accessed_at timestamptz, PRIMARY KEY(workspace_id,doc_id,date));`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	for _, row := range []struct {
		date             time.Time
		workspace, blobs int64
	}{
		{today.AddDate(0, 0, -3), 100, 10},
		{today.AddDate(0, 0, -1), 300, 30},
	} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspace_admin_stats_daily(date,snapshot_size,blob_size) VALUES($1,$2,$3)`, row.date, row.workspace, row.blobs); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_admin_stats(snapshot_size,blob_size) VALUES(1200,120)`); err != nil {
		t.Fatal(err)
	}
	syncTo := now.Truncate(time.Minute)
	for _, point := range []struct {
		minute time.Time
		users  int
	}{{syncTo.Add(-61 * time.Minute), 99}, {syncTo.Add(-5 * time.Minute), 7}} {
		if _, err := pool.Exec(ctx, `INSERT INTO sync_active_users_minutely(minute_ts,active_users) VALUES($1,$2)`, point.minute, point.users); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ai_sessions_messages(role,created_at) VALUES('user',$1),('assistant',$1)`, now.Add(-30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	for _, link := range []struct {
		doc                  string
		views, unique, guest int
	}{
		{"doc-a", 20, 4, 2},
		{"doc-b", 20, 8, 3},
		{"doc-c", 5, 4, 1},
	} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspace_pages(workspace_id,page_id,title,published_at) VALUES($1,$2,$3,$4)`, fixture.workspaceID, link.doc, link.doc+" title", now.Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO doc_access_policies(workspace_id,doc_id,visibility,public_role) VALUES($1,$2,'public','external')`, fixture.workspaceID, link.doc); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO workspace_doc_view_daily(workspace_id,doc_id,date,total_views,unique_views,guest_views,last_accessed_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, fixture.workspaceID, link.doc, today, link.views, link.unique, link.guest, now.Add(-time.Minute)); err != nil {
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
	query := `query Dashboard($input: AdminDashboardInput) {
		adminDashboard(input:$input) {
			syncActiveUsers syncActiveUsersTimeline { minute activeUsers }
			syncWindow { from to timezone bucket requestedSize effectiveSize }
			copilotConversations copilotWindow { from to timezone bucket requestedSize effectiveSize }
			workspaceStorageBytes blobStorageBytes
			workspaceStorageHistory { date value } blobStorageHistory { date value }
			storageWindow { from to timezone bucket requestedSize effectiveSize }
			...DashboardSharedLinks
			topSharedLinksWindow { from to timezone bucket requestedSize effectiveSize }
			generatedAt
		}
	}`
	query += ` fragment DashboardSharedLinks on AdminDashboard {
		topSharedLinks { workspaceId docId title shareUrl publishedAt views uniqueViews guestViews lastAccessedAt }
	}`
	response := graphRequest(t, newHandler("affine"), query, map[string]any{"input": map[string]any{
		"timezone": "UTC", "storageHistoryDays": 3, "syncHistoryHours": 2,
		"sharedLinkWindowDays": 3, "copilotWindowDays": 2,
	}}, fixture.sessionID, fixture.userID)
	if response["errors"] != nil {
		t.Fatalf("admin dashboard errors=%#v", response["errors"])
	}
	dashboard := response["data"].(map[string]any)["adminDashboard"].(map[string]any)
	if dashboard["syncActiveUsers"] != float64(7) || dashboard["copilotConversations"] != float64(1) || dashboard["workspaceStorageBytes"] != float64(1200) || dashboard["blobStorageBytes"] != float64(120) {
		t.Fatalf("dashboard totals=%#v", dashboard)
	}
	if dashboard["syncActiveUsersTimeline"].([]any)[0].(map[string]any)["activeUsers"] != float64(0) || dashboard["syncActiveUsersTimeline"].([]any)[len(dashboard["syncActiveUsersTimeline"].([]any))-1].(map[string]any)["activeUsers"] != float64(7) {
		t.Fatalf("sync timeline did not constrain/carry-forward samples: %#v", dashboard["syncActiveUsersTimeline"])
	}
	storageHistory := dashboard["workspaceStorageHistory"].([]any)
	if len(storageHistory) != 3 || storageHistory[0].(map[string]any)["value"] != float64(100) || storageHistory[1].(map[string]any)["value"] != float64(300) || storageHistory[2].(map[string]any)["value"] != float64(1200) {
		t.Fatalf("storage carry-forward/live values=%#v", storageHistory)
	}
	storageWindow := dashboard["storageWindow"].(map[string]any)
	if storageWindow["requestedSize"] != float64(3) || storageWindow["effectiveSize"] != float64(3) || storageWindow["timezone"] != "UTC" {
		t.Fatalf("storage window=%#v", storageWindow)
	}
	links := dashboard["topSharedLinks"].([]any)
	if len(links) != 3 {
		t.Fatalf("top links count=%d", len(links))
	}
	top := links[0].(map[string]any)
	if top["docId"] != "doc-b" || top["shareUrl"] != fmt.Sprintf("https://notes.example.test/workspace/%s/doc-b", fixture.workspaceID) || top["views"] != float64(20) || top["uniqueViews"] != float64(8) {
		t.Fatalf("top shared link=%#v", top)
	}
	if _, err := pool.Exec(ctx, `DROP TABLE workspace_pages`); err != nil {
		t.Fatal(err)
	}
	clamped := graphRequest(t, newHandler("affine"), `query DashboardWindows($input: AdminDashboardInput) {
		adminDashboard(input:$input) {
			syncWindow { requestedSize effectiveSize }
			storageWindow { requestedSize effectiveSize }
			topSharedLinksWindow { requestedSize effectiveSize }
			copilotWindow { requestedSize effectiveSize }
		}
	}`, map[string]any{"input": map[string]any{
		"storageHistoryDays": -10, "syncHistoryHours": 500,
		"sharedLinkWindowDays": 0, "copilotWindowDays": 999,
	}}, fixture.sessionID, fixture.userID)
	if clamped["errors"] != nil {
		t.Fatalf("clamped dashboard errors=%#v", clamped["errors"])
	}
	clampedDashboard := clamped["data"].(map[string]any)["adminDashboard"].(map[string]any)
	for _, value := range []struct {
		field                string
		requested, effective float64
	}{
		{"syncWindow", 500, 72}, {"storageWindow", -10, 1},
		{"topSharedLinksWindow", 0, 1}, {"copilotWindow", 999, 90},
	} {
		window := clampedDashboard[value.field].(map[string]any)
		if window["requestedSize"] != value.requested || window["effectiveSize"] != value.effective {
			t.Fatalf("%s did not preserve/clamp sizes: %#v", value.field, window)
		}
	}

	for _, test := range []struct {
		name, deployment, session, user, errorName string
		status                                     float64
	}{
		{name: "non-admin", deployment: "affine", session: fixture.sessionID, user: fixture.sharedUserID, errorName: "action_forbidden", status: http.StatusForbidden},
		{name: "self-hosted", deployment: "selfhosted", session: fixture.sessionID, user: fixture.userID, errorName: "NOT_FOUND", status: http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			denied := graphRequest(t, newHandler(test.deployment), query, nil, test.session, test.user)
			errors := denied["errors"].([]any)
			extensions := errors[0].(map[string]any)["extensions"].(map[string]any)
			if extensions["name"] != test.errorName || extensions["status"] != test.status {
				t.Fatalf("error extensions=%#v", extensions)
			}
		})
	}
}
