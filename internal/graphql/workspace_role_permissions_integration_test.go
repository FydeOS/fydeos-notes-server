// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"net/http"
	"testing"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

const workspaceRolePermissionsQuery = `query getWorkspaceRolePermissions($id: String!) {
	workspaceRolePermissions(id: $id) {
		role
		permissions {
			Workspace_Administrators_Manage
			Workspace_Blobs_Manage
			Workspace_Blobs_Upload
			Workspace_Copilot
			Workspace_CreateDoc
			Workspace_Delete
			Workspace_Organize_Read
			Workspace_Payment_Manage
			Workspace_Preview
			Workspace_Properties_Create
			Workspace_Properties_Delete
			Workspace_Properties_Read
			Workspace_Properties_Update
			Workspace_Read
			Workspace_Settings_Read
			Workspace_Settings_Update
			Workspace_Sync
			Workspace_TransferOwner
			Workspace_Users_Manage
			Workspace_Users_Read
		}
	}
}`

func TestWorkspaceRolePermissionsGraphQLUsesWorkspaceAuthorization(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	repository := workspaces.NewRepository(pool)
	authorization := workspaces.NewAuthorizationService(repository)
	if _, err := pool.Exec(ctx, `UPDATE effective_workspace_quota_states SET known=false, stale=true WHERE workspace_id=$1`, fixture.workspaceID); err != nil {
		t.Fatal(err)
	}
	handler := NewHandlerWithServices(
		config.Config{Deployment: "cloud", Host: "localhost", Port: 3010},
		auth.NewSessionResolver(pool), nil, repository, nil, nil, nil, nil,
		HandlerDependencies{WorkspacePermissionReader: authorization},
	)
	variables := map[string]any{"id": fixture.workspaceID}
	response := graphRequest(t, handler, workspaceRolePermissionsQuery, variables, fixture.sessionID, fixture.userID)
	if errors, ok := response["errors"]; ok {
		t.Fatalf("owner query errors = %#v", errors)
	}
	data := response["data"].(map[string]any)["workspaceRolePermissions"].(map[string]any)
	permissions := data["permissions"].(map[string]any)
	if data["role"] != "Owner" || len(permissions) != 20 {
		t.Fatalf("workspace role permissions = %#v", data)
	}
	for name, want := range map[string]bool{
		"Workspace_Read":                  true,
		"Workspace_Sync":                  true,
		"Workspace_Users_Manage":          true,
		"Workspace_Payment_Manage":        true,
		"Workspace_Administrators_Manage": false,
	} {
		if permissions[name] != want {
			t.Errorf("permission %s = %v, want %v", name, permissions[name], want)
		}
	}

	if _, err := pool.Exec(ctx, `UPDATE workspace_members SET role='admin' WHERE workspace_id=$1 AND user_id=$2`, fixture.workspaceID, fixture.userID); err != nil {
		t.Fatal(err)
	}
	response = graphRequest(t, handler, workspaceRolePermissionsQuery, variables, fixture.sessionID, fixture.userID)
	data = response["data"].(map[string]any)["workspaceRolePermissions"].(map[string]any)
	permissions = data["permissions"].(map[string]any)
	if data["role"] != "Collaborator" || permissions["Workspace_Users_Manage"] != false || permissions["Workspace_Administrators_Manage"] != false {
		t.Fatalf("unlicensed admin permissions = %#v", data)
	}

	if _, err := pool.Exec(ctx, `UPDATE workspace_members SET state='inactive' WHERE workspace_id=$1 AND user_id=$2`, fixture.workspaceID, fixture.userID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_access_policies(workspace_id, visibility, sharing_enabled, url_preview_enabled) VALUES ($1, 'public', true, true)`, fixture.workspaceID); err != nil {
		t.Fatal(err)
	}
	response = graphRequest(t, handler, workspaceRolePermissionsQuery, variables, fixture.sessionID, fixture.userID)
	data = response["data"].(map[string]any)["workspaceRolePermissions"].(map[string]any)
	permissions = data["permissions"].(map[string]any)
	if data["role"] != "External" || permissions["Workspace_Read"] != true || permissions["Workspace_Preview"] != true || permissions["Workspace_Sync"] != false {
		t.Fatalf("public external permissions = %#v", data)
	}

	if _, err := pool.Exec(ctx, `UPDATE workspace_access_policies SET visibility='private' WHERE workspace_id=$1`, fixture.workspaceID); err != nil {
		t.Fatal(err)
	}
	assertWorkspacePermissionDenied(t, graphRequest(t, handler, workspaceRolePermissionsQuery, variables, fixture.sessionID, fixture.userID), fixture.workspaceID)
	assertWorkspacePermissionDenied(t, graphRequest(t, handler, workspaceRolePermissionsQuery, map[string]any{"id": "missing-workspace"}, fixture.sessionID, fixture.userID), "missing-workspace")

	unauthenticated := graphRequest(t, handler, workspaceRolePermissionsQuery, variables, "", "")
	errors, ok := unauthenticated["errors"].([]any)
	if !ok || len(errors) != 1 || errors[0].(map[string]any)["message"] != ErrAuthenticationRequired.Error() {
		t.Fatalf("unauthenticated response = %#v", unauthenticated)
	}
}

func assertWorkspacePermissionDenied(t *testing.T, response map[string]any, workspaceID string) {
	t.Helper()
	errors, ok := response["errors"].([]any)
	if !ok || len(errors) != 1 {
		t.Fatalf("workspace %q denial response = %#v", workspaceID, response)
	}
	entry := errors[0].(map[string]any)
	extensions := entry["extensions"].(map[string]any)
	data := extensions["data"].(map[string]any)
	if extensions["name"] != "space_access_denied" || data["spaceId"] != workspaceID {
		t.Fatalf("workspace %q denial = %#v", workspaceID, entry)
	}
	if extensions["status"] != float64(http.StatusForbidden) {
		t.Fatalf("workspace %q denial status = %#v", workspaceID, extensions["status"])
	}
}
