// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"net/http"
	"testing"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/users"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
	"github.com/google/uuid"
)

func TestDeleteAccountGraphQLCompatibility(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	if _, err := pool.Exec(ctx, `ALTER TABLE users ADD COLUMN registered boolean NOT NULL DEFAULT true`); err != nil {
		t.Fatal(err)
	}
	workspaceID := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id) VALUES($1)`, workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_members(id,workspace_id,user_id,role,state) VALUES($1,$2,$3,'owner','active')`, uuid.NewString(), workspaceID, fixture.sharedUserID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO effective_workspace_quota_states(workspace_id,owner_user_id,uses_owner_quota,known,stale) VALUES($1,$2,true,true,false)`, workspaceID, fixture.sharedUserID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO snapshots(workspace_id,guid) VALUES($1,$1)`, workspaceID); err != nil {
		t.Fatal(err)
	}

	userRepository := users.NewRepository(pool)
	workspaceRepository := workspaces.NewRepository(pool)
	objectCleaner := &accountLifecycleWorkspaceCleaner{}
	avatarCleaner := &accountLifecycleAvatarCleaner{}
	mailCanceler := &accountLifecycleMailCanceler{}
	lifecycle := users.NewAccountLifecycleService(users.AccountLifecycleDependencies{
		Repository:          userRepository,
		Workspaces:          workspaceRepository,
		ListOwnedWorkspaces: listAccountOwnedWorkspaces(workspaceRepository),
		Objects:             objectCleaner,
		Avatars:             avatarCleaner,
		Mail:                mailCanceler,
	})
	userService := users.NewService(userRepository, workspaces.NewAuthorizationService(workspaceRepository), lifecycle)
	handler := NewHandler(config.Config{Version: "0.27.5", Deployment: "selfhosted"}, auth.NewSessionResolver(pool), userService, workspaceRepository)
	mutation := `mutation deleteAccount { deleteAccount { success } }`

	response := graphRequest(t, handler, mutation, nil, fixture.sessionID, fixture.sharedUserID)
	if failures, ok := response["errors"].([]any); ok && len(failures) != 0 {
		t.Fatalf("deleteAccount errors = %#v", failures)
	}
	if response["data"].(map[string]any)["deleteAccount"].(map[string]any)["success"] != true {
		t.Fatalf("deleteAccount response = %#v", response)
	}
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1)`, fixture.sharedUserID).Scan(&exists); err != nil || exists {
		t.Fatalf("deleted account exists=%t err=%v", exists, err)
	}
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspaces WHERE id=$1)`, workspaceID).Scan(&exists); err != nil || exists {
		t.Fatalf("personal workspace exists=%t err=%v", exists, err)
	}
	currentUser := graphRequest(t, handler, `{ currentUser { id } }`, nil, fixture.sessionID, fixture.sharedUserID)
	if user := currentUser["data"].(map[string]any)["currentUser"]; user != nil {
		t.Fatalf("currentUser after account deletion = %#v, want null", user)
	}
	if len(objectCleaner.workspaceIDs) != 1 || objectCleaner.workspaceIDs[0] != workspaceID {
		t.Fatalf("workspace object cleanup calls = %#v", objectCleaner.workspaceIDs)
	}
	if len(mailCanceler.recipients) != 1 || mailCanceler.recipients[0] != fixture.sharedEmail || len(mailCanceler.actors) != 1 || mailCanceler.actors[0] != fixture.sharedUserID {
		t.Fatalf("mail cleanup calls = %#v/%#v", mailCanceler.recipients, mailCanceler.actors)
	}
}

func TestDeleteAccountRejectsOwnedTeamWorkspaceBeforeRevokingSession(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	if _, err := pool.Exec(ctx, `ALTER TABLE users ADD COLUMN registered boolean NOT NULL DEFAULT true`); err != nil {
		t.Fatal(err)
	}
	workspaceID := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id) VALUES($1)`, workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_members(id,workspace_id,user_id,role,state) VALUES($1,$2,$3,'owner','active')`, uuid.NewString(), workspaceID, fixture.sharedUserID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO effective_workspace_quota_states(workspace_id,owner_user_id,uses_owner_quota,known,stale) VALUES($1,$2,false,true,false)`, workspaceID, fixture.sharedUserID); err != nil {
		t.Fatal(err)
	}

	userRepository := users.NewRepository(pool)
	workspaceRepository := workspaces.NewRepository(pool)
	objectCleaner := &accountLifecycleWorkspaceCleaner{}
	avatarCleaner := &accountLifecycleAvatarCleaner{}
	mailCanceler := &accountLifecycleMailCanceler{}
	lifecycle := users.NewAccountLifecycleService(users.AccountLifecycleDependencies{
		Repository:          userRepository,
		Workspaces:          workspaceRepository,
		ListOwnedWorkspaces: listAccountOwnedWorkspaces(workspaceRepository),
		Objects:             objectCleaner,
		Avatars:             avatarCleaner,
		Mail:                mailCanceler,
	})
	userService := users.NewService(userRepository, workspaces.NewAuthorizationService(workspaceRepository), lifecycle)
	handler := NewHandler(config.Config{Version: "0.27.5", Deployment: "selfhosted"}, auth.NewSessionResolver(pool), userService, workspaceRepository)
	response := graphRequest(t, handler, `mutation { deleteAccount { success } }`, nil, fixture.sessionID, fixture.sharedUserID)
	assertAdminUserLifecycleError(t, response, "cannot_delete_account_with_owned_team_workspace", http.StatusForbidden)
	assertAdminUserLifecycleMessage(t, response, "Cannot delete account. You are the owner of one or more team workspaces. Please transfer ownership or delete them first.")
	var userExists, sessionExists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1)`, fixture.sharedUserID).Scan(&userExists); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM user_sessions WHERE user_id=$1)`, fixture.sharedUserID).Scan(&sessionExists); err != nil {
		t.Fatal(err)
	}
	if !userExists || !sessionExists || len(objectCleaner.workspaceIDs) != 0 || len(avatarCleaner.urls) != 0 || len(mailCanceler.recipients) != 0 {
		t.Fatalf("team-workspace rejection changed state: user=%t session=%t objects=%#v avatars=%#v mail=%#v", userExists, sessionExists, objectCleaner.workspaceIDs, avatarCleaner.urls, mailCanceler.recipients)
	}
}

func TestDeleteAccountRequiresAuthentication(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	userRepository := users.NewRepository(pool)
	workspaceRepository := workspaces.NewRepository(pool)
	lifecycle := users.NewAccountLifecycleService(users.AccountLifecycleDependencies{
		Repository:          userRepository,
		Workspaces:          workspaceRepository,
		ListOwnedWorkspaces: listAccountOwnedWorkspaces(workspaceRepository),
		Objects:             &accountLifecycleWorkspaceCleaner{},
		Avatars:             &accountLifecycleAvatarCleaner{},
		Mail:                &accountLifecycleMailCanceler{},
	})
	userService := users.NewService(userRepository, workspaces.NewAuthorizationService(workspaceRepository), lifecycle)
	handler := NewHandler(config.Config{Version: "0.27.5", Deployment: "selfhosted"}, auth.NewSessionResolver(pool), userService, workspaceRepository)
	response := graphRequest(t, handler, `mutation { deleteAccount { success } }`, nil, "", "")
	assertAdminUserLifecycleError(t, response, "AUTHENTICATION_REQUIRED", http.StatusUnauthorized)
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1)`, fixture.userID).Scan(&exists); err != nil || !exists {
		t.Fatalf("anonymous delete changed user state: exists=%t err=%v", exists, err)
	}
}
