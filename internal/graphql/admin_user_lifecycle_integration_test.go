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
	"github.com/fydeos/fydeos-notes-server/internal/identity"
	"github.com/fydeos/fydeos-notes-server/internal/users"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
	"github.com/google/uuid"
)

type accountLifecycleWorkspaceCleaner struct {
	workspaceIDs []string
	userIDs      [][]string
}

func (c *accountLifecycleWorkspaceCleaner) DeleteWorkspaceObjects(_ context.Context, workspaceID string, userIDs []string) (int64, error) {
	c.workspaceIDs = append(c.workspaceIDs, workspaceID)
	c.userIDs = append(c.userIDs, append([]string(nil), userIDs...))
	return 0, nil
}

type accountLifecycleAvatarCleaner struct {
	urls []string
}

func (c *accountLifecycleAvatarCleaner) DeleteByURL(_ context.Context, value string) error {
	c.urls = append(c.urls, value)
	return nil
}

type accountLifecycleMailCanceler struct {
	recipients []string
	actors     []string
}

func listAccountOwnedWorkspaces(repository *workspaces.Repository) func(context.Context, string) ([]users.OwnedWorkspace, error) {
	return func(ctx context.Context, userID string) ([]users.OwnedWorkspace, error) {
		owned, err := repository.ListOwnedForUser(ctx, userID)
		if err != nil {
			return nil, err
		}
		result := make([]users.OwnedWorkspace, len(owned))
		for i, workspace := range owned {
			result[i] = users.OwnedWorkspace{ID: workspace.ID, Team: workspace.Team}
		}
		return result, nil
	}
}

func (c *accountLifecycleMailCanceler) CancelByRecipient(_ context.Context, email string) (int64, error) {
	c.recipients = append(c.recipients, email)
	return 1, nil
}

func (c *accountLifecycleMailCanceler) CancelMemberInvitationByActor(_ context.Context, userID string) (int64, error) {
	c.actors = append(c.actors, userID)
	return 1, nil
}

func TestAdminDeleteUserGraphQLCompatibility(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	if _, err := pool.Exec(ctx, `ALTER TABLE users ADD COLUMN registered boolean NOT NULL DEFAULT true`); err != nil {
		t.Fatal(err)
	}

	targetID := uuid.NewString()
	workspaceID := uuid.NewString()
	avatarURL := "https://assets.example.test/avatars/deleted-user.webp"
	createdAt := time.Date(2025, 2, 3, 4, 5, 6, 0, time.UTC)
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,name,email,email_verified,avatar_url,created_at,password,registered,disabled,auth_epoch)
		VALUES($1,'Delete Target','delete-target@example.test',$2,$3,$4,'$argon2id$fixture',true,false,4)`, targetID, createdAt, avatarURL, createdAt); err != nil {
		t.Fatal(err)
	}
	sessionID := uuid.NewString()
	userSessionID := uuid.NewString()
	authSessionID := uuid.NewString()
	refreshTokenID := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO multiple_users_sessions(id) VALUES($1)`, sessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO user_sessions(id,session_id,user_id,created_at) VALUES($1,$2,$3,now())`, userSessionID, sessionID, targetID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO auth_sessions(id,user_session_id,idle_expires_at,absolute_expires_at) VALUES($1,$2,now()+interval '1 hour',now()+interval '1 day')`, authSessionID, userSessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO auth_refresh_tokens(id,auth_session_id) VALUES($1,$2)`, refreshTokenID, authSessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_invitations(id,workspace_id,invitee_user_id,inviter_user_id,status) VALUES($1,$2,$3,$4,'pending')`, uuid.NewString(), fixture.workspaceID, targetID, fixture.userID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id,created_at) VALUES($1,$2)`, workspaceID, createdAt); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_members(id,workspace_id,user_id,role,state,created_at) VALUES($1,$2,$3,'owner','active',$4)`, uuid.NewString(), workspaceID, targetID, createdAt); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO effective_workspace_quota_states(workspace_id,owner_user_id,uses_owner_quota,known,stale) VALUES($1,$2,true,true,false)`, workspaceID, targetID); err != nil {
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
	handler := NewHandlerWithProfilePublisherAndWorkspaceCleaner(
		config.Config{Version: "0.27.5", Deployment: "selfhosted"},
		auth.NewSessionResolver(pool), userService, workspaceRepository, nil, nil, objectCleaner,
	)
	mutation := `mutation deleteUser($id: String!) { deleteUser(id: $id) { success } }`

	nonAdmin := graphRequest(t, handler, mutation, map[string]any{"id": targetID}, fixture.sessionID, fixture.sharedUserID)
	assertAdminUserLifecycleError(t, nonAdmin, "action_forbidden", http.StatusForbidden)

	selfDelete := graphRequest(t, handler, mutation, map[string]any{"id": fixture.userID}, fixture.sessionID, fixture.userID)
	assertAdminUserLifecycleError(t, selfDelete, "cannot_delete_own_account", http.StatusForbidden)
	assertAdminUserLifecycleMessage(t, selfDelete, "Cannot delete own account.")
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1)`, fixture.userID).Scan(&exists); err != nil || !exists {
		t.Fatalf("self-delete changed administrator account: exists=%t err=%v", exists, err)
	}

	deleted := graphRequest(t, handler, mutation, map[string]any{"id": targetID}, fixture.sessionID, fixture.userID)
	if failures, ok := deleted["errors"].([]any); ok && len(failures) != 0 {
		t.Fatalf("admin deleteUser errors = %#v", failures)
	}
	if success := deleted["data"].(map[string]any)["deleteUser"].(map[string]any)["success"]; success != true {
		t.Fatalf("deleteUser result = %#v, want success", deleted)
	}
	repeatedDelete := graphRequest(t, handler, mutation, map[string]any{"id": targetID}, fixture.sessionID, fixture.userID)
	assertAdminUserLifecycleError(t, repeatedDelete, "USER_NOT_FOUND", http.StatusNotFound)
	for _, check := range []struct{ query, argument string }{
		{`SELECT EXISTS(SELECT 1 FROM users WHERE id=$1)`, targetID},
		{`SELECT EXISTS(SELECT 1 FROM user_sessions WHERE user_id=$1)`, targetID},
		{`SELECT EXISTS(SELECT 1 FROM auth_sessions WHERE id=$1)`, authSessionID},
		{`SELECT EXISTS(SELECT 1 FROM auth_refresh_tokens WHERE id=$1)`, refreshTokenID},
		{`SELECT EXISTS(SELECT 1 FROM workspace_invitations WHERE invitee_user_id=$1)`, targetID},
	} {
		var stillExists bool
		if err := pool.QueryRow(ctx, check.query, check.argument).Scan(&stillExists); err != nil {
			t.Fatal(err)
		}
		if stillExists {
			t.Fatalf("deleted account relation remains for query %q", check.query)
		}
	}
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspaces WHERE id=$1)`, workspaceID).Scan(&exists); err != nil || exists {
		t.Fatalf("deleted personal workspace exists=%t err=%v", exists, err)
	}
	if len(objectCleaner.workspaceIDs) != 1 || objectCleaner.workspaceIDs[0] != workspaceID || len(objectCleaner.userIDs[0]) != 1 || objectCleaner.userIDs[0][0] != targetID {
		t.Fatalf("workspace object cleanup calls = %#v/%#v", objectCleaner.workspaceIDs, objectCleaner.userIDs)
	}
	if len(avatarCleaner.urls) != 1 || avatarCleaner.urls[0] != avatarURL {
		t.Fatalf("avatar cleanup calls = %#v", avatarCleaner.urls)
	}
	if len(mailCanceler.recipients) != 1 || mailCanceler.recipients[0] != "delete-target@example.test" || len(mailCanceler.actors) != 1 || mailCanceler.actors[0] != targetID {
		t.Fatalf("mail cleanup calls = %#v/%#v", mailCanceler.recipients, mailCanceler.actors)
	}
}

func TestAdminDeleteUserRejectsOwnedTeamWorkspaceBeforeSideEffects(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	if _, err := pool.Exec(ctx, `ALTER TABLE users ADD COLUMN registered boolean NOT NULL DEFAULT true`); err != nil {
		t.Fatal(err)
	}
	targetID := uuid.NewString()
	workspaceID := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,name,email,created_at,registered,disabled,auth_epoch) VALUES($1,'Team Owner','team-owner@example.test',now(),true,false,0)`, targetID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id) VALUES($1)`, workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_members(id,workspace_id,user_id,role,state) VALUES($1,$2,$3,'owner','active')`, uuid.NewString(), workspaceID, targetID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO effective_workspace_quota_states(workspace_id,owner_user_id,uses_owner_quota,known,stale) VALUES($1,$2,false,true,false)`, workspaceID, targetID); err != nil {
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
	response := graphRequest(t, handler, `mutation deleteUser($id: String!) { deleteUser(id: $id) { success } }`, map[string]any{"id": targetID}, fixture.sessionID, fixture.userID)
	assertAdminUserLifecycleError(t, response, "cannot_delete_account_with_owned_team_workspace", http.StatusForbidden)
	assertAdminUserLifecycleMessage(t, response, "Cannot delete account. You are the owner of one or more team workspaces. Please transfer ownership or delete them first.")
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1)`, targetID).Scan(&exists); err != nil || !exists {
		t.Fatalf("team-owner account was deleted: exists=%t err=%v", exists, err)
	}
	if len(objectCleaner.workspaceIDs) != 0 || len(avatarCleaner.urls) != 0 || len(mailCanceler.recipients) != 0 || len(mailCanceler.actors) != 0 {
		t.Fatalf("team-owner rejection caused cleanup side effects: objects=%#v avatars=%#v mail=%#v/%#v", objectCleaner.workspaceIDs, avatarCleaner.urls, mailCanceler.recipients, mailCanceler.actors)
	}
}

func TestAdminBanUserRecreatesDisabledAccountWithLegacyFields(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	if _, err := pool.Exec(ctx, `ALTER TABLE users ADD COLUMN registered boolean NOT NULL DEFAULT true`); err != nil {
		t.Fatal(err)
	}
	targetID := uuid.NewString()
	createdAt := time.Date(2024, 7, 8, 9, 10, 11, 0, time.UTC)
	verifiedAt := createdAt.Add(time.Hour)
	avatarURL := "https://assets.example.test/avatars/banned-user.webp"
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,name,email,email_verified,avatar_url,created_at,password,registered,disabled,auth_epoch)
		VALUES($1,'Ban Target','ban-target@example.test',$2,$3,$4,'$argon2id$fixture',true,false,4)`, targetID, verifiedAt, avatarURL, createdAt); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO user_features(user_id,name,activated) VALUES($1,'administrator',true)`, targetID); err != nil {
		t.Fatal(err)
	}

	userRepository := users.NewRepository(pool)
	workspaceRepository := workspaces.NewRepository(pool)
	avatarCleaner := &accountLifecycleAvatarCleaner{}
	mailCanceler := &accountLifecycleMailCanceler{}
	lifecycle := users.NewAccountLifecycleService(users.AccountLifecycleDependencies{
		Repository:          userRepository,
		Workspaces:          workspaceRepository,
		ListOwnedWorkspaces: listAccountOwnedWorkspaces(workspaceRepository),
		Avatars:             avatarCleaner,
		Mail:                mailCanceler,
	})
	userService := users.NewService(userRepository, workspaces.NewAuthorizationService(workspaceRepository), lifecycle)
	handler := NewHandler(config.Config{Version: "0.27.5", Deployment: "selfhosted"}, auth.NewSessionResolver(pool), userService, workspaceRepository)
	mutation := `mutation banUser($id: String!) { banUser(id: $id) { id name email disabled emailVerified hasPassword features } }`
	nonAdmin := graphRequest(t, handler, mutation, map[string]any{"id": targetID}, fixture.sessionID, fixture.sharedUserID)
	assertAdminUserLifecycleError(t, nonAdmin, "action_forbidden", http.StatusForbidden)
	response := graphRequest(t, handler, mutation, map[string]any{"id": targetID}, fixture.sessionID, fixture.userID)
	if failures, ok := response["errors"].([]any); ok && len(failures) != 0 {
		t.Fatalf("admin banUser errors = %#v", failures)
	}
	banned := response["data"].(map[string]any)["banUser"].(map[string]any)
	if banned["id"] == targetID || banned["name"] != "Ban Target" || banned["email"] != "ban-target@example.test" || banned["disabled"] != true || banned["emailVerified"] != true || banned["hasPassword"] != true {
		t.Fatalf("banUser result = %#v", banned)
	}
	if features, ok := banned["features"].([]any); !ok || len(features) != 0 {
		t.Fatalf("banUser features = %#v, want legacy relationship cascade", banned["features"])
	}
	var registered, disabled bool
	var name, email, password string
	var authEpoch int
	var actualCreatedAt, actualVerifiedAt time.Time
	var actualAvatar string
	if err := pool.QueryRow(ctx, `SELECT name,email,email_verified,avatar_url,created_at,password,registered,disabled,auth_epoch FROM users WHERE id=$1`, banned["id"]).
		Scan(&name, &email, &actualVerifiedAt, &actualAvatar, &actualCreatedAt, &password, &registered, &disabled, &authEpoch); err != nil {
		t.Fatal(err)
	}
	if name != "Ban Target" || email != "ban-target@example.test" || !actualVerifiedAt.Equal(verifiedAt) || actualAvatar != avatarURL || !actualCreatedAt.Equal(createdAt) || password != "$argon2id$fixture" || !registered || !disabled || authEpoch != 6 {
		t.Fatalf("recreated ban account fields differ: %q %q %v %q %v %t %t %d", name, email, actualVerifiedAt, actualAvatar, actualCreatedAt, registered, disabled, authEpoch)
	}
	var featureCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM user_features WHERE user_id=$1`, banned["id"]).Scan(&featureCount); err != nil || featureCount != 0 {
		t.Fatalf("banUser retained feature relationships = %d err=%v", featureCount, err)
	}
	missing := graphRequest(t, handler, mutation, map[string]any{"id": uuid.NewString()}, fixture.sessionID, fixture.userID)
	assertAdminUserLifecycleError(t, missing, "USER_NOT_FOUND", http.StatusNotFound)
}

func TestBanAccountWaitsForSignupEmailLockBeforeReplacingAccount(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedGraphQLFixture(t, ctx, pool)
	if _, err := pool.Exec(ctx, `ALTER TABLE users ADD COLUMN registered boolean NOT NULL DEFAULT true`); err != nil {
		t.Fatal(err)
	}
	targetID := uuid.NewString()
	email := "ban-email-lock@example.test"
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,name,email,created_at,registered,disabled,auth_epoch)
		VALUES($1,'Ban Lock Target',$2,now(),true,false,4)`, targetID, email); err != nil {
		t.Fatal(err)
	}

	preparedSignup, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = preparedSignup.Rollback(context.Background()) }()
	if err := identity.LockEmail(ctx, preparedSignup, email); err != nil {
		t.Fatal(err)
	}

	type banResult struct {
		account *users.AccountRecord
		user    *users.User
		err     error
	}
	result := make(chan banResult, 1)
	repository := users.NewRepository(pool)
	go func() {
		account, user, err := repository.BanAccountForAdmin(ctx, targetID)
		result <- banResult{account: account, user: user, err: err}
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS(
			SELECT 1 FROM pg_stat_activity
			WHERE wait_event_type='Lock' AND query LIKE '%auth:email:%'
		)`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ban transaction did not wait for the shared signup email lock")
		}
		time.Sleep(10 * time.Millisecond)
	}

	var originalStillPresent bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1)`, targetID).Scan(&originalStillPresent); err != nil || !originalStillPresent {
		t.Fatalf("ban removed the old account before obtaining the email lock: present=%t err=%v", originalStillPresent, err)
	}
	if err := preparedSignup.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	completed := <-result
	if completed.err != nil {
		t.Fatal(completed.err)
	}
	if completed.account.Email != email || completed.user.ID == targetID || !completed.user.Disabled {
		t.Fatalf("banned account = %#v, original = %q", completed.user, targetID)
	}
	var activeCount, disabledCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE disabled=false), count(*) FILTER (WHERE disabled=true)
		FROM users WHERE lower(email)=lower($1)`, email).Scan(&activeCount, &disabledCount); err != nil {
		t.Fatal(err)
	}
	if activeCount != 0 || disabledCount != 1 {
		t.Fatalf("email rows after ban = active %d, disabled %d; want 0 and 1", activeCount, disabledCount)
	}
}

func assertAdminUserLifecycleError(t *testing.T, response map[string]any, name string, status int) {
	t.Helper()
	failures, ok := response["errors"].([]any)
	if !ok || len(failures) != 1 {
		t.Fatalf("admin lifecycle response = %#v, want %s error", response, name)
	}
	extensions := failures[0].(map[string]any)["extensions"].(map[string]any)
	if extensions["name"] != name || extensions["status"] != float64(status) {
		t.Fatalf("admin lifecycle error extensions = %#v, want %s/%d", extensions, name, status)
	}
}

func assertAdminUserLifecycleMessage(t *testing.T, response map[string]any, expected string) {
	t.Helper()
	failures, ok := response["errors"].([]any)
	if !ok || len(failures) != 1 {
		t.Fatalf("admin lifecycle response = %#v, want message %q", response, expected)
	}
	if message := failures[0].(map[string]any)["message"]; message != expected {
		t.Fatalf("admin lifecycle error message = %q, want %q", message, expected)
	}
}
