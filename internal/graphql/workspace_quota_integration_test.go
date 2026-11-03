// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"reflect"
	"testing"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

func TestWorkspaceQuotaMatchesOwnerAndCommercialQuotaScopes(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	otherWorkspaceID := "owner-storage-workspace"
	teamWorkspaceID := "commercial-storage-workspace"
	if _, err := pool.Exec(ctx, `CREATE TABLE blobs(workspace_id text NOT NULL,size bigint NOT NULL,status text NOT NULL,deleted_at timestamptz,reservation_expires_at timestamptz)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE comment_attachments(LIKE blobs)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id) VALUES ($1),($2)`, otherWorkspaceID, teamWorkspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_members(id,workspace_id,user_id,role,state) VALUES ('owner-other',$1,$3,'owner','active'),('owner-team',$2,$3,'owner','active')`, otherWorkspaceID, teamWorkspaceID, fixture.userID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_invitations(id,workspace_id,status) VALUES ('pending-seat',$1,'pending'),('review-seat',$1,'waiting_review'),('wait-seat',$1,'waiting_seat'),('accepted-seat',$1,'accepted')`, fixture.workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO entitlements(target_type,target_id,source,status,plan,quantity) VALUES ('user',$1,'cloud_subscription','active','pro',NULL),('workspace',$2,'cloud_subscription','active','team',1)`, fixture.userID, teamWorkspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO blobs(workspace_id,size,status) VALUES ($1,30,'completed'),($2,20,'completed'),($3,1000,'completed')`, fixture.workspaceID, otherWorkspaceID, teamWorkspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO blobs(workspace_id,size,status,reservation_expires_at) VALUES ($1,4,'pending',clock_timestamp()+interval '1 hour'),($1,50,'pending',clock_timestamp()-interval '1 hour')`, fixture.workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO blobs(workspace_id,size,status,deleted_at) VALUES ($1,60,'completed',clock_timestamp())`, fixture.workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO comment_attachments(workspace_id,size,status) VALUES ($1,3,'completed')`, fixture.workspaceID); err != nil {
		t.Fatal(err)
	}

	repository := workspaces.NewRepositoryWithDeployment(pool, "cloud")
	handler := NewHandler(config.Config{Deployment: "cloud", Host: "localhost", Port: 3010}, auth.NewSessionResolver(pool), nil, repository)
	query := `query WorkspaceQuota($id: String!) { workspace(id:$id) { quota {
		name blobLimit storageQuota usedStorageQuota historyPeriod memberLimit memberCount overcapacityMemberCount
		humanReadable { name blobLimit storageQuota storageQuotaUsed historyPeriod memberLimit memberCount overcapacityMemberCount }
	} } }`
	read := func() map[string]any {
		t.Helper()
		result := graphRequest(t, handler, query, map[string]any{"id": fixture.workspaceID}, fixture.sessionID, fixture.userID)
		if failures, ok := result["errors"].([]any); ok && len(failures) > 0 {
			t.Fatalf("workspace quota query errors: %#v", failures)
		}
		return result["data"].(map[string]any)["workspace"].(map[string]any)["quota"].(map[string]any)
	}

	ownerQuota := read()
	wantOwnerQuota := map[string]any{
		"name": "Pro", "blobLimit": float64(100 * 1024 * 1024), "storageQuota": float64(100 * 1024 * 1024 * 1024),
		"usedStorageQuota": float64(57), "historyPeriod": float64(30 * 24 * 60 * 60), "memberLimit": float64(10),
		"memberCount": float64(5), "overcapacityMemberCount": float64(0),
		"humanReadable": map[string]any{
			"name": "Pro", "blobLimit": "100 MB", "storageQuota": "100 GB", "storageQuotaUsed": "57 B",
			"historyPeriod": "30 days", "memberLimit": "10", "memberCount": "5", "overcapacityMemberCount": "0",
		},
	}
	if !reflect.DeepEqual(ownerQuota, wantOwnerQuota) {
		t.Fatalf("owner quota = %#v, want %#v", ownerQuota, wantOwnerQuota)
	}

	if _, err := pool.Exec(ctx, `INSERT INTO entitlements(target_type,target_id,source,status,plan,quantity) VALUES ('workspace',$1,'cloud_subscription','active','team',2)`, fixture.workspaceID); err != nil {
		t.Fatal(err)
	}
	teamQuota := read()
	wantTeamQuota := map[string]any{
		"name": "Team", "blobLimit": float64(500 * 1024 * 1024), "storageQuota": float64(140 * 1024 * 1024 * 1024),
		"usedStorageQuota": float64(37), "historyPeriod": float64(30 * 24 * 60 * 60), "memberLimit": float64(2),
		"memberCount": float64(5), "overcapacityMemberCount": float64(3),
		"humanReadable": map[string]any{
			"name": "Team", "blobLimit": "500 MB", "storageQuota": "140 GB", "storageQuotaUsed": "37 B",
			"historyPeriod": "30 days", "memberLimit": "2", "memberCount": "5", "overcapacityMemberCount": "3",
		},
	}
	if !reflect.DeepEqual(teamQuota, wantTeamQuota) {
		t.Fatalf("team quota = %#v, want %#v", teamQuota, wantTeamQuota)
	}

	userState, err := repository.ReadUserQuotaState(ctx, fixture.userID)
	if err != nil {
		t.Fatal(err)
	}
	if userState.Plan != "pro" || userState.SeatLimit != 10 || userState.BlobLimit != 100*1024*1024 ||
		userState.StorageQuota != 100*1024*1024*1024 || userState.UsedStorageQuota != 20 ||
		userState.HistoryPeriodSeconds != 30*24*60*60 || userState.CopilotActionLimit == nil || *userState.CopilotActionLimit != 10 || userState.UnlimitedCopilot {
		t.Fatalf("user quota realtime state = %+v", userState)
	}

	workspaceState, err := repository.ReadWorkspaceQuotaState(ctx, fixture.workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if workspaceState.Plan != "team" || workspaceState.OwnerUserID != fixture.userID || workspaceState.UsesOwnerQuota ||
		workspaceState.SeatLimit != 2 || workspaceState.MemberCount != 5 || workspaceState.OvercapacityMemberCount != 3 ||
		workspaceState.BlobLimit != 500*1024*1024 || workspaceState.StorageQuota != 140*1024*1024*1024 ||
		workspaceState.UsedStorageQuota != 37 || !workspaceState.Readonly || !reflect.DeepEqual(workspaceState.ReadonlyReasons, []string{"member_overflow"}) {
		t.Fatalf("workspace quota realtime state = %+v", workspaceState)
	}
}
