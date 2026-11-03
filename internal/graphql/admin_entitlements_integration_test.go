// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/entitlements"
	"github.com/fydeos/fydeos-notes-server/internal/users"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAdminCommercialEntitlementGraphQLLifecycle(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	fixture := seedGraphQLFixture(t, ctx, pool)
	applyAdminGrantTestSchema(t, pool)
	grantStore := entitlements.NewAdminGrantStore(pool, "cloud")
	publisher := &adminEntitlementPublisherRecorder{}
	handler := newAdminEntitlementHandler(pool, "cloud", grantStore, publisher)

	nonAdminSessionID := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO multiple_users_sessions(id) VALUES($1)`, nonAdminSessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO user_sessions(id,session_id,user_id,created_at) VALUES($1,$2,$3,now())`, uuid.NewString(), nonAdminSessionID, fixture.sharedUserID); err != nil {
		t.Fatal(err)
	}
	denied := graphRequest(t, handler, `mutation($targetId: String!) {
		grantCommercialEntitlement(targetType: "workspace", targetId: $targetId, plan: "team", quantity: 2)
	}`, map[string]any{"targetId": fixture.workspaceID}, nonAdminSessionID, fixture.sharedUserID)
	if extensions := adminGrantGraphErrorExtensions(t, denied); extensions["name"] != "action_forbidden" || extensions["status"] != float64(http.StatusForbidden) {
		t.Fatalf("non-admin grant error = %#v", extensions)
	}
	var activeCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM entitlements WHERE source='admin_grant' AND status IN ('active','grace')`).Scan(&activeCount); err != nil || activeCount != 0 {
		t.Fatalf("non-admin request created %d active grants, err=%v", activeCount, err)
	}

	grantTeam := `mutation($targetId: String!, $quantity: Int) {
		grantCommercialEntitlement(targetType: "workspace", targetId: $targetId, plan: "team", quantity: $quantity)
	}`
	if data := adminGrantGraphData(t, graphRequest(t, handler, grantTeam, map[string]any{"targetId": fixture.workspaceID, "quantity": 5}, fixture.sessionID, fixture.userID)); data["grantCommercialEntitlement"] != true {
		t.Fatalf("team grant result = %#v", data)
	}
	var grantID, subjectID, plan, status string
	var quantity int
	if err := pool.QueryRow(ctx, `SELECT id,subject_id,plan,status,quantity FROM entitlements
		WHERE source='admin_grant' AND target_type='workspace' AND target_id=$1`, fixture.workspaceID).
		Scan(&grantID, &subjectID, &plan, &status, &quantity); err != nil {
		t.Fatal(err)
	}
	if subjectID != "admin_grant:workspace:"+fixture.workspaceID || plan != "team" || status != "active" || quantity != 5 {
		t.Fatalf("stored team grant = id:%s subject:%s plan:%s status:%s quantity:%d", grantID, subjectID, plan, status, quantity)
	}

	if _, err := pool.Exec(ctx, `UPDATE workspace_members SET role='admin' WHERE workspace_id=$1 AND user_id=$2`, fixture.workspaceID, fixture.sharedUserID); err != nil {
		t.Fatal(err)
	}
	for _, invitationStatus := range []string{"pending", "waiting_review", "waiting_seat"} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspace_invitations(id,workspace_id,status) VALUES($1,$2,$3)`, uuid.NewString(), fixture.workspaceID, invitationStatus); err != nil {
			t.Fatal(err)
		}
	}
	grantFree := `mutation($targetId: String!) {
		grantCommercialEntitlement(targetType: "workspace", targetId: $targetId, plan: "free")
	}`
	if data := adminGrantGraphData(t, graphRequest(t, handler, grantFree, map[string]any{"targetId": fixture.workspaceID}, fixture.sessionID, fixture.userID)); data["grantCommercialEntitlement"] != true {
		t.Fatalf("workspace free grant result = %#v", data)
	}
	var currentPlan, currentStatus string
	var currentQuantity *int32
	if err := pool.QueryRow(ctx, `SELECT plan,status,quantity FROM entitlements WHERE id=$1`, grantID).Scan(&currentPlan, &currentStatus, &currentQuantity); err != nil {
		t.Fatal(err)
	}
	if currentPlan != "free" || currentStatus != "active" || currentQuantity != nil {
		t.Fatalf("replaced grant = %s/%s/%v", currentPlan, currentStatus, currentQuantity)
	}
	var invitations int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspace_invitations WHERE workspace_id=$1`, fixture.workspaceID).Scan(&invitations); err != nil {
		t.Fatal(err)
	}
	var demotedRole string
	if err := pool.QueryRow(ctx, `SELECT role FROM workspace_members WHERE workspace_id=$1 AND user_id=$2`, fixture.workspaceID, fixture.sharedUserID).Scan(&demotedRole); err != nil {
		t.Fatal(err)
	}
	if invitations != 0 || demotedRole != "member" {
		t.Fatalf("team downgrade left %d invitations and role %q", invitations, demotedRole)
	}

	if _, err := pool.Exec(ctx, `UPDATE workspace_members SET role='admin' WHERE workspace_id=$1 AND user_id=$2`, fixture.workspaceID, fixture.sharedUserID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_invitations(id,workspace_id,status) VALUES($1,$2,'pending')`, uuid.NewString(), fixture.workspaceID); err != nil {
		t.Fatal(err)
	}
	if data := adminGrantGraphData(t, graphRequest(t, handler, grantTeam, map[string]any{"targetId": fixture.workspaceID, "quantity": 5}, fixture.sessionID, fixture.userID)); data["grantCommercialEntitlement"] != true {
		t.Fatalf("second team grant result = %#v", data)
	}
	revokeWorkspace := `mutation($targetId: String!) {
		revokeCommercialEntitlement(targetType: "workspace", targetId: $targetId)
	}`
	if data := adminGrantGraphData(t, graphRequest(t, handler, revokeWorkspace, map[string]any{"targetId": fixture.workspaceID}, fixture.sessionID, fixture.userID)); data["revokeCommercialEntitlement"] != true {
		t.Fatalf("workspace revoke result = %#v", data)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM entitlements WHERE id=$1`, grantID).Scan(&currentStatus); err != nil || currentStatus != "revoked" {
		t.Fatalf("revoked workspace grant status=%q err=%v", currentStatus, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspace_invitations WHERE workspace_id=$1`, fixture.workspaceID).Scan(&invitations); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT role FROM workspace_members WHERE workspace_id=$1 AND user_id=$2`, fixture.workspaceID, fixture.sharedUserID).Scan(&demotedRole); err != nil {
		t.Fatal(err)
	}
	if invitations != 0 || demotedRole != "member" {
		t.Fatalf("team revoke left %d invitations and role %q", invitations, demotedRole)
	}

	grantUser := `mutation($targetId: String!) {
		grantCommercialEntitlement(targetType: "user", targetId: $targetId, plan: "pro", quantity: 5)
	}`
	if data := adminGrantGraphData(t, graphRequest(t, handler, grantUser, map[string]any{"targetId": fixture.sharedUserID}, fixture.sessionID, fixture.userID)); data["grantCommercialEntitlement"] != true {
		t.Fatalf("user grant result = %#v", data)
	}
	var userQuantity *int32
	if err := pool.QueryRow(ctx, `SELECT quantity FROM entitlements WHERE target_type='user' AND target_id=$1 AND source='admin_grant'`, fixture.sharedUserID).Scan(&userQuantity); err != nil || userQuantity != nil {
		t.Fatalf("non-seat grant quantity=%v err=%v", userQuantity, err)
	}
	if data := adminGrantGraphData(t, graphRequest(t, handler, `mutation($targetId: String!) {
		revokeCommercialEntitlement(targetType: "user", targetId: $targetId)
	}`, map[string]any{"targetId": fixture.sharedUserID}, fixture.sessionID, fixture.userID)); data["revokeCommercialEntitlement"] != true {
		t.Fatalf("user revoke result = %#v", data)
	}

	if got := strings.Join(publisher.workspaceIDs, ","); got != strings.Join([]string{fixture.workspaceID, fixture.workspaceID, fixture.workspaceID, fixture.workspaceID}, ",") {
		t.Fatalf("workspace change publications = %#v", publisher.workspaceIDs)
	}
	if got := strings.Join(publisher.userIDs, ","); got != strings.Join([]string{fixture.userID, fixture.userID, fixture.userID, fixture.userID, fixture.sharedUserID, fixture.sharedUserID}, ",") {
		t.Fatalf("user quota publications = %#v", publisher.userIDs)
	}
}

func TestAdminCommercialEntitlementConcurrentGrantsSerializeAndSelfHostedRejects(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	fixture := seedGraphQLFixture(t, ctx, pool)
	applyAdminGrantTestSchema(t, pool)
	store := entitlements.NewAdminGrantStore(pool, "cloud")
	start := make(chan struct{})
	errCh := make(chan error, 8)
	var wait sync.WaitGroup
	for i := 0; i < 8; i++ {
		wait.Add(1)
		go func(quantity int32) {
			defer wait.Done()
			<-start
			_, err := store.Upsert(ctx, entitlements.AdminGrantInput{
				TargetType: "workspace", TargetID: fixture.workspaceID, Plan: "team", Quantity: &quantity,
			})
			errCh <- err
		}(int32(i + 1))
	}
	close(start)
	wait.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent grant: %v", err)
		}
	}
	var allRows, activeRows int
	if err := pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE status='active') FROM entitlements
		WHERE source='admin_grant' AND subject_id=$1`, "admin_grant:workspace:"+fixture.workspaceID).Scan(&allRows, &activeRows); err != nil {
		t.Fatal(err)
	}
	if allRows != 1 || activeRows != 1 {
		t.Fatalf("serialized grants left %d history rows and %d active rows", allRows, activeRows)
	}

	selfHosted := entitlements.NewAdminGrantStore(pool, "selfhosted")
	if _, err := selfHosted.Upsert(ctx, entitlements.AdminGrantInput{TargetType: "workspace", TargetID: fixture.workspaceID, Plan: "team"}); err == nil || !strings.Contains(err.Error(), "signed license") {
		t.Fatalf("self-hosted grant error = %v", err)
	}
	if _, err := selfHosted.Revoke(ctx, "workspace", fixture.workspaceID); err != nil {
		t.Fatalf("self-hosted admin grant revoke should remain allowed: %v", err)
	}
}

func newAdminEntitlementHandler(pool *pgxpool.Pool, deployment string, store *entitlements.AdminGrantStore, publisher *adminEntitlementPublisherRecorder) http.Handler {
	workspaceRepository := workspaces.NewRepository(pool)
	userService := users.NewService(users.NewRepository(pool), workspaces.NewAuthorizationService(workspaceRepository))
	return NewHandlerWithServices(config.Config{Version: "0.27.5", Deployment: deployment}, auth.NewSessionResolver(pool), userService, workspaceRepository, nil, nil, nil, nil, HandlerDependencies{
		AdminEntitlements:         store,
		LicenseWorkspacePublisher: publisher,
		LicenseQuotaPublisher:     publisher,
	})
}

type adminEntitlementPublisherRecorder struct {
	workspaceIDs []string
	userIDs      []string
}

func (p *adminEntitlementPublisherRecorder) PublishWorkspaceEntitlementChanged(workspaceID string) {
	p.workspaceIDs = append(p.workspaceIDs, workspaceID)
}

func (p *adminEntitlementPublisherRecorder) PublishUserQuotaStateChanged(userID, _ string) {
	p.userIDs = append(p.userIDs, userID)
}

func applyAdminGrantTestSchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for _, statement := range []string{
		`ALTER TABLE entitlements ADD COLUMN id text NOT NULL DEFAULT gen_random_uuid()::text`,
		`ALTER TABLE entitlements ADD COLUMN subject_id text`,
		`ALTER TABLE entitlements ADD COLUMN metadata jsonb NOT NULL DEFAULT '{}'`,
		`ALTER TABLE entitlements ADD COLUMN validated_at timestamptz`,
		`ALTER TABLE entitlements ADD COLUMN created_at timestamptz NOT NULL DEFAULT now()`,
		`ALTER TABLE entitlements ADD PRIMARY KEY (id)`,
		`CREATE UNIQUE INDEX entitlements_active_subject_key ON entitlements(source,subject_id) WHERE subject_id IS NOT NULL AND status IN ('active','grace')`,
	} {
		if _, err := pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("apply admin entitlement test schema %q: %v", statement, err)
		}
	}
}

func adminGrantGraphErrorExtensions(t *testing.T, result map[string]any) map[string]any {
	t.Helper()
	errors, ok := result["errors"].([]any)
	if !ok || len(errors) == 0 {
		t.Fatalf("GraphQL result has no error: %#v", result)
	}
	return errors[0].(map[string]any)["extensions"].(map[string]any)
}

func adminGrantGraphData(t *testing.T, result map[string]any) map[string]any {
	t.Helper()
	if failures, ok := result["errors"].([]any); ok && len(failures) != 0 {
		t.Fatalf("GraphQL errors: %#v", failures)
	}
	data, ok := result["data"].(map[string]any)
	if !ok {
		t.Fatalf("GraphQL data = %#v", result)
	}
	return data
}
