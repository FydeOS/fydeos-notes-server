// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"reflect"
	"testing"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/users"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

func TestUserQuotaGraphQLUsesAuthenticatedUsersQuotaAndUsage(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	if _, err := pool.Exec(ctx, `CREATE TABLE blobs(workspace_id text NOT NULL,size bigint NOT NULL,status text NOT NULL,deleted_at timestamptz,reservation_expires_at timestamptz)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE comment_attachments(LIKE blobs)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO entitlements(target_type,target_id,source,status,plan,quantity) VALUES ('user',$1,'cloud_subscription','active','pro',NULL)`, fixture.userID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO blobs(workspace_id,size,status) VALUES ($1,30,'completed')`, fixture.workspaceID); err != nil {
		t.Fatal(err)
	}
	repository := workspaces.NewRepositoryWithDeployment(pool, "cloud")
	userService := users.NewService(users.NewRepository(pool), workspaces.NewAuthorizationService(repository))
	handler := NewHandler(config.Config{Deployment: "cloud"}, auth.NewSessionResolver(pool), userService, repository)
	query := `query quota { currentUser { id quota { name blobLimit storageQuota usedStorageQuota historyPeriod memberLimit humanReadable { name blobLimit storageQuota usedStorageQuota historyPeriod memberLimit copilotActionLimit } } quotaUsage { storageQuota } } }`
	result := graphRequest(t, handler, query, nil, fixture.sessionID, fixture.userID)
	if failures, ok := result["errors"].([]any); ok && len(failures) > 0 {
		t.Fatalf("user quota query errors: %#v", failures)
	}
	data := result["data"].(map[string]any)["currentUser"].(map[string]any)
	quota := data["quota"].(map[string]any)
	want := map[string]any{
		"name": "Pro", "blobLimit": float64(100 * 1024 * 1024), "storageQuota": float64(100 * 1024 * 1024 * 1024),
		"usedStorageQuota": float64(30), "historyPeriod": float64(30 * 24 * 60 * 60), "memberLimit": float64(10),
		"humanReadable": map[string]any{
			"name": "Pro", "blobLimit": "100 MB", "storageQuota": "100 GB", "usedStorageQuota": "30 B",
			"historyPeriod": "30 days", "memberLimit": "10", "copilotActionLimit": "10 times",
		},
	}
	if !reflect.DeepEqual(quota, want) {
		t.Fatalf("user quota = %#v, want %#v", quota, want)
	}
	if got := data["quotaUsage"].(map[string]any)["storageQuota"]; got != float64(30) {
		t.Fatalf("quotaUsage.storageQuota = %#v, want 30", got)
	}
}
