// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/identity"
	"github.com/fydeos/fydeos-notes-server/internal/password"
	"github.com/fydeos/fydeos-notes-server/internal/users"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
	"github.com/google/uuid"
)

func TestAdminUserQueriesGraphQLCompatibility(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	var hiddenUserID string
	if err := pool.QueryRow(ctx, `SELECT id FROM users WHERE email=$1`, fixture.hiddenEmail).Scan(&hiddenUserID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET created_at = CASE id
		WHEN $1 THEN '2026-01-01T00:00:00Z'::timestamptz
		WHEN $2 THEN '2026-01-02T00:00:00Z'::timestamptz
		WHEN $3 THEN '2026-01-03T00:00:00Z'::timestamptz
		WHEN $4 THEN '2026-01-04T00:00:00Z'::timestamptz END`, fixture.userID, fixture.sharedUserID, hiddenUserID, fixture.disabledUserID); err != nil {
		t.Fatal(err)
	}

	nonAdminSessionID := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO multiple_users_sessions(id) VALUES($1)`, nonAdminSessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO user_sessions(id,session_id,user_id,created_at) VALUES($1,$2,$3,now())`, uuid.NewString(), nonAdminSessionID, fixture.sharedUserID); err != nil {
		t.Fatal(err)
	}

	userRepository := users.NewRepository(pool)
	userService := users.NewService(userRepository, workspaces.NewAuthorizationService(workspaces.NewRepository(pool)))
	handler := NewHandler(config.Config{Version: "0.27.5", Deployment: "selfhosted"}, auth.NewSessionResolver(pool), userService, workspaces.NewRepository(pool))

	denied := graphRequest(t, handler, `query listUsers($filter: ListUserInput!) {
		users(filter: $filter) { id }
	}`, map[string]any{"filter": map[string]any{}}, nonAdminSessionID, fixture.sharedUserID)
	deniedErrors, ok := denied["errors"].([]any)
	if !ok || len(deniedErrors) != 1 {
		t.Fatalf("non-admin users query = %#v, want forbidden error", denied)
	}
	deniedExtensions := deniedErrors[0].(map[string]any)["extensions"].(map[string]any)
	if deniedExtensions["name"] != "action_forbidden" || deniedExtensions["status"] != float64(http.StatusForbidden) {
		t.Fatalf("non-admin users error = %#v", deniedExtensions)
	}

	page := graphRequest(t, handler, `query listUsers($filter: ListUserInput!) {
		users(filter: $filter) { id name email disabled features hasPassword emailVerified avatarUrl }
		usersCount(filter: $filter)
	}`, map[string]any{"filter": map[string]any{"first": 2, "skip": 1}}, fixture.sessionID, fixture.userID)
	data := page["data"].(map[string]any)
	usersPage := data["users"].([]any)
	if len(usersPage) != 2 || usersPage[0].(map[string]any)["id"] != fixture.sharedUserID || usersPage[1].(map[string]any)["id"] != hiddenUserID {
		t.Fatalf("admin user page = %#v", usersPage)
	}
	if data["usersCount"] != float64(4) {
		t.Fatalf("admin user count = %#v, want 4 independent of page", data["usersCount"])
	}

	filtered := graphRequest(t, handler, `query listUsers($filter: ListUserInput!) {
		users(filter: $filter) { id }
		usersCount(filter: $filter)
	}`, map[string]any{"filter": map[string]any{"features": []string{"Admin"}, "keyword": fixture.sharedUserID[:8]}}, fixture.sessionID, fixture.userID)
	filteredData := filtered["data"].(map[string]any)
	if len(filteredData["users"].([]any)) != 0 || filteredData["usersCount"] != float64(0) {
		t.Fatalf("combined user filters = %#v, want no matching users", filteredData)
	}

	disabled := graphRequest(t, handler, `query getUserByEmail($email: String!) {
		userByEmail(email: $email) { id email disabled }
	}`, map[string]any{"email": "DISABLED@example.test"}, fixture.sessionID, fixture.userID)
	disabledUser := disabled["data"].(map[string]any)["userByEmail"].(map[string]any)
	if disabledUser["id"] != fixture.disabledUserID || disabledUser["disabled"] != true {
		t.Fatalf("admin lookup of disabled user = %#v", disabledUser)
	}

	byID := graphRequest(t, handler, `query getUserById($id: String!) {
		userById(id: $id) { id email disabled }
	}`, map[string]any{"id": fixture.disabledUserID}, fixture.sessionID, fixture.userID)
	byIDUser := byID["data"].(map[string]any)["userById"].(map[string]any)
	if byIDUser["id"] != fixture.disabledUserID || byIDUser["disabled"] != true {
		t.Fatalf("admin lookup by id of disabled user = %#v", byIDUser)
	}

	missing := graphRequest(t, handler, `query getUserByEmail($email: String!) {
		userByEmail(email: $email) { id }
	}`, map[string]any{"email": "missing@example.test"}, fixture.sessionID, fixture.userID)
	if missing["data"].(map[string]any)["userByEmail"] != nil {
		t.Fatalf("missing admin email query = %#v, want null", missing)
	}

	if _, err := pool.Exec(ctx, `INSERT INTO user_features(user_id,name,activated,expired_at) VALUES($1,'administrator',true,$2)`, fixture.sharedUserID, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	filteredFeatures := graphRequest(t, handler, `query listUsers($filter: ListUserInput!) {
		users(filter: $filter) { id features }
		usersCount(filter: $filter)
	}`, map[string]any{"filter": map[string]any{"features": []string{"Admin"}}}, fixture.sessionID, fixture.userID)
	featureData := filteredFeatures["data"].(map[string]any)
	featureUsers := featureData["users"].([]any)
	if len(featureUsers) != 2 || featureData["usersCount"] != float64(2) {
		t.Fatalf("feature filter = %#v, want active and expired activated records per legacy list filter", featureData)
	}
}

func TestAdminCreateUserGraphQLCompatibility(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	if _, err := pool.Exec(ctx, `ALTER TABLE users ADD COLUMN registered boolean NOT NULL DEFAULT false`); err != nil {
		t.Fatalf("add registered column for createUser schema: %v", err)
	}
	nonAdminSessionID := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO multiple_users_sessions(id) VALUES($1)`, nonAdminSessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO user_sessions(id,session_id,user_id,created_at) VALUES($1,$2,$3,now())`, uuid.NewString(), nonAdminSessionID, fixture.sharedUserID); err != nil {
		t.Fatal(err)
	}

	userRepository := users.NewRepository(pool)
	userService := users.NewService(userRepository, workspaces.NewAuthorizationService(workspaces.NewRepository(pool)))
	handler := NewHandler(config.Config{Version: "0.27.5", Deployment: "selfhosted"}, auth.NewSessionResolver(pool), userService, workspaces.NewRepository(pool))
	mutation := `mutation createUser($input: CreateUserInput!) { createUser(input: $input) { id name email hasPassword disabled emailVerified } }`
	variables := map[string]any{"input": map[string]any{"email": "created@example.test", "password": "initial-password"}}

	denied := graphRequest(t, handler, mutation, variables, nonAdminSessionID, fixture.sharedUserID)
	deniedErrors, ok := denied["errors"].([]any)
	if !ok || len(deniedErrors) != 1 {
		t.Fatalf("non-admin createUser = %#v, want forbidden error", denied)
	}
	deniedExtensions := deniedErrors[0].(map[string]any)["extensions"].(map[string]any)
	if deniedExtensions["name"] != "action_forbidden" || deniedExtensions["status"] != float64(http.StatusForbidden) {
		t.Fatalf("non-admin createUser error = %#v", deniedExtensions)
	}
	var createdCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE lower(email)=lower($1)`, "created@example.test").Scan(&createdCount); err != nil || createdCount != 0 {
		t.Fatalf("denied createUser rows = %d, err=%v", createdCount, err)
	}

	created := graphRequest(t, handler, mutation, variables, fixture.sessionID, fixture.userID)
	if failures, ok := created["errors"].([]any); ok && len(failures) != 0 {
		t.Fatalf("admin createUser errors = %#v", failures)
	}
	createdUser := created["data"].(map[string]any)["createUser"].(map[string]any)
	if createdUser["id"] == "" || createdUser["name"] != "created" || createdUser["email"] != "created@example.test" || createdUser["hasPassword"] != true || createdUser["disabled"] != false || createdUser["emailVerified"] != false {
		t.Fatalf("createUser result = %#v", createdUser)
	}
	var registered bool
	var hash string
	if err := pool.QueryRow(ctx, `SELECT registered,password FROM users WHERE id=$1`, createdUser["id"]).Scan(&registered, &hash); err != nil {
		t.Fatal(err)
	}
	if !registered || !password.Verify(hash, "initial-password") {
		t.Fatalf("created user registered=%t password hash did not verify", registered)
	}

	duplicate := graphRequest(t, handler, mutation, map[string]any{"input": map[string]any{"email": "CREATED@example.test"}}, fixture.sessionID, fixture.userID)
	duplicateErrors, ok := duplicate["errors"].([]any)
	if !ok || len(duplicateErrors) != 1 {
		t.Fatalf("duplicate createUser = %#v, want email conflict", duplicate)
	}
	duplicateExtensions := duplicateErrors[0].(map[string]any)["extensions"].(map[string]any)
	if duplicateExtensions["name"] != "email_already_used" || duplicateExtensions["type"] != "RESOURCE_ALREADY_EXISTS" || duplicateExtensions["status"] != float64(http.StatusBadRequest) {
		t.Fatalf("duplicate createUser error = %#v", duplicateExtensions)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE lower(email)=lower($1)`, "created@example.test").Scan(&createdCount); err != nil || createdCount != 1 {
		t.Fatalf("duplicate createUser rows = %d, err=%v; want one", createdCount, err)
	}

	explicitName := "Provisioned Name"
	withoutPassword := graphRequest(t, handler, mutation, map[string]any{"input": map[string]any{"email": "named@example.test", "name": explicitName}}, fixture.sessionID, fixture.userID)
	if failures, ok := withoutPassword["errors"].([]any); ok && len(failures) != 0 {
		t.Fatalf("createUser without password errors = %#v", failures)
	}
	namedUser := withoutPassword["data"].(map[string]any)["createUser"].(map[string]any)
	if namedUser["name"] != explicitName || namedUser["hasPassword"] != false {
		t.Fatalf("createUser optional fields = %#v", namedUser)
	}

	type createResult struct {
		user *users.User
		err  error
	}
	results := make(chan createResult, 2)
	for range 2 {
		go func() {
			user, err := userRepository.Create(ctx, "raced@example.test", "raced", nil)
			results <- createResult{user: user, err: err}
		}()
	}
	successes, conflicts := 0, 0
	for range 2 {
		result := <-results
		switch {
		case result.err == nil && result.user != nil:
			successes++
		case errors.Is(result.err, users.ErrEmailAlreadyUsed):
			conflicts++
		default:
			t.Fatalf("concurrent create result = %#v", result)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent create outcomes = %d successes / %d conflicts; want one each", successes, conflicts)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE lower(email)=lower($1)`, "raced@example.test").Scan(&createdCount); err != nil || createdCount != 1 {
		t.Fatalf("concurrent create rows = %d, err=%v; want one", createdCount, err)
	}

	lockTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := identity.LockEmail(ctx, lockTx, "Placeholder@example.test"); err != nil {
		_ = lockTx.Rollback(ctx)
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := userRepository.Create(ctx, "placeholder@EXAMPLE.test", "placeholder", nil)
		result <- err
	}()
	select {
	case err := <-result:
		_ = lockTx.Rollback(ctx)
		t.Fatalf("admin creation did not wait for shared email lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if _, err := lockTx.Exec(ctx, `INSERT INTO users(id,name,email,created_at,registered,disabled) VALUES($1,'Invite','Placeholder@example.test',now(),false,false)`, uuid.NewString()); err != nil {
		_ = lockTx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := lockTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, users.ErrEmailAlreadyUsed) {
		t.Fatalf("admin creation after another account path inserted placeholder = %v, want email conflict", err)
	}
}

func TestAdminImportUsersGraphQLCompatibility(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	if _, err := pool.Exec(ctx, `ALTER TABLE users ADD COLUMN registered boolean NOT NULL DEFAULT false`); err != nil {
		t.Fatalf("add registered column for importUsers schema: %v", err)
	}
	userRepository := users.NewRepository(pool)
	userService := users.NewService(userRepository, workspaces.NewAuthorizationService(workspaces.NewRepository(pool)))
	handler := NewHandler(config.Config{Version: "0.27.5", Deployment: "selfhosted"}, auth.NewSessionResolver(pool), userService, workspaces.NewRepository(pool))
	mutation := `mutation importUsers($input: ImportUsersInput!) {
		importUsers(input: $input) {
			__typename
			... on UserType { id name email hasPassword }
			... on UserImportFailedType { email error }
		}
	}`
	variables := map[string]any{"input": map[string]any{"users": []any{
		map[string]any{"email": "batch-one@example.test", "name": "Batch One", "password": "batch-password-one"},
		map[string]any{"email": fixture.hiddenEmail},
		map[string]any{"email": "batch-three@example.test"},
		map[string]any{"email": "batch-race@example.test"},
		map[string]any{"email": "BATCH-RACE@example.test"},
	}}}

	nonAdminSessionID := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO multiple_users_sessions(id) VALUES($1)`, nonAdminSessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO user_sessions(id,session_id,user_id,created_at) VALUES($1,$2,$3,now())`, uuid.NewString(), nonAdminSessionID, fixture.sharedUserID); err != nil {
		t.Fatal(err)
	}
	denied := graphRequest(t, handler, mutation, variables, nonAdminSessionID, fixture.sharedUserID)
	if failures, ok := denied["errors"].([]any); !ok || len(failures) != 1 {
		t.Fatalf("non-admin importUsers = %#v, want forbidden error", denied)
	}
	var deniedRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE email IN ('batch-one@example.test','batch-three@example.test') OR lower(email)='batch-race@example.test'`).Scan(&deniedRows); err != nil || deniedRows != 0 {
		t.Fatalf("denied import inserted %d users, err=%v", deniedRows, err)
	}

	response := graphRequest(t, handler, mutation, variables, fixture.sessionID, fixture.userID)
	if failures, ok := response["errors"].([]any); ok && len(failures) != 0 {
		t.Fatalf("admin importUsers errors = %#v", failures)
	}
	results := response["data"].(map[string]any)["importUsers"].([]any)
	if len(results) != 5 {
		t.Fatalf("importUsers returned %d entries, want one for each input: %#v", len(results), results)
	}
	first := results[0].(map[string]any)
	if first["__typename"] != "UserType" || first["email"] != "batch-one@example.test" || first["name"] != "Batch One" || first["hasPassword"] != true {
		t.Fatalf("first import result = %#v", first)
	}
	second := results[1].(map[string]any)
	if second["__typename"] != "UserImportFailedType" || second["email"] != fixture.hiddenEmail || second["error"] != "This email has already been registered." {
		t.Fatalf("duplicate import result = %#v", second)
	}
	third := results[2].(map[string]any)
	if third["__typename"] != "UserType" || third["email"] != "batch-three@example.test" || third["name"] != "batch-three" || third["hasPassword"] != false {
		t.Fatalf("third import result = %#v", third)
	}
	fourth, fifth := results[3].(map[string]any), results[4].(map[string]any)
	if fourth["__typename"] == fifth["__typename"] ||
		(fourth["__typename"] != "UserType" && fourth["__typename"] != "UserImportFailedType") ||
		(fifth["__typename"] != "UserType" && fifth["__typename"] != "UserImportFailedType") {
		t.Fatalf("same-batch case-insensitive duplicate results = %#v / %#v", fourth, fifth)
	}
	failedDuplicate := fourth
	if failedDuplicate["__typename"] == "UserType" {
		failedDuplicate = fifth
	}
	if failedDuplicate["email"] == "" || failedDuplicate["error"] != "This email has already been registered." {
		t.Fatalf("same-batch duplicate failure = %#v", failedDuplicate)
	}
	var registered bool
	var hash string
	if err := pool.QueryRow(ctx, `SELECT registered,password FROM users WHERE id=$1`, first["id"]).Scan(&registered, &hash); err != nil {
		t.Fatal(err)
	}
	if !registered || !password.Verify(hash, "batch-password-one") {
		t.Fatalf("imported user registered=%t password hash invalid", registered)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE email IN ('batch-one@example.test','batch-three@example.test') OR lower(email)='batch-race@example.test'`).Scan(&deniedRows); err != nil || deniedRows != 3 {
		t.Fatalf("successful imports inserted %d users, err=%v; want 3", deniedRows, err)
	}
}

func TestAdminUpdateUserGraphQLCompatibility(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	nonAdminSessionID := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO multiple_users_sessions(id) VALUES($1)`, nonAdminSessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO user_sessions(id,session_id,user_id,created_at) VALUES($1,$2,$3,now())`, uuid.NewString(), nonAdminSessionID, fixture.sharedUserID); err != nil {
		t.Fatal(err)
	}
	var targetSessionID string
	if err := pool.QueryRow(ctx, `SELECT id FROM user_sessions WHERE session_id=$1 AND user_id=$2`, fixture.sessionID, fixture.sharedUserID).Scan(&targetSessionID); err != nil {
		t.Fatal(err)
	}
	authSessionID := uuid.NewString()
	refreshTokenID := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO auth_sessions(id,user_session_id,idle_expires_at,absolute_expires_at) VALUES($1,$2,now()+interval '1 hour',now()+interval '1 day')`, authSessionID, targetSessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO auth_refresh_tokens(id,auth_session_id) VALUES($1,$2)`, refreshTokenID, authSessionID); err != nil {
		t.Fatal(err)
	}
	userRepository := users.NewRepository(pool)
	workspaceRepository := workspaces.NewRepository(pool)
	userService := users.NewService(userRepository, workspaces.NewAuthorizationService(workspaceRepository))
	publisher := &profilePublisherRecorder{}
	handler := NewHandlerWithProfilePublisher(
		config.Config{Version: "0.27.5", Deployment: "selfhosted"},
		auth.NewSessionResolver(pool), userService, workspaceRepository, nil, publisher,
	)
	mutation := `mutation updateAccount($id: String!, $input: ManageUserInput!) {
		updateUser(id: $id, input: $input) { id name email emailVerified disabled }
	}`
	variables := map[string]any{"id": fixture.sharedUserID, "input": map[string]any{"name": "Renamed"}}

	denied := graphRequest(t, handler, mutation, variables, nonAdminSessionID, fixture.sharedUserID)
	deniedErrors, ok := denied["errors"].([]any)
	if !ok || len(deniedErrors) != 1 {
		t.Fatalf("non-admin updateUser = %#v, want forbidden error", denied)
	}
	deniedExtensions := deniedErrors[0].(map[string]any)["extensions"].(map[string]any)
	if deniedExtensions["name"] != "action_forbidden" || deniedExtensions["status"] != float64(http.StatusForbidden) {
		t.Fatalf("non-admin updateUser error = %#v", deniedExtensions)
	}
	var name, email string
	var epoch int
	if err := pool.QueryRow(ctx, `SELECT name,email,auth_epoch FROM users WHERE id=$1`, fixture.sharedUserID).Scan(&name, &email, &epoch); err != nil {
		t.Fatal(err)
	}
	if name != "Shared User" || email != fixture.sharedEmail || epoch != 0 {
		t.Fatalf("denied updateUser changed user to %q/%q epoch=%d", name, email, epoch)
	}

	invalid := graphRequest(t, handler, mutation, map[string]any{"id": fixture.sharedUserID, "input": map[string]any{"email": "not-an-email"}}, fixture.sessionID, fixture.userID)
	assertUpdateUserError(t, invalid, "invalid_email", http.StatusBadRequest)
	if err := pool.QueryRow(ctx, `SELECT email,auth_epoch FROM users WHERE id=$1`, fixture.sharedUserID).Scan(&email, &epoch); err != nil || email != fixture.sharedEmail || epoch != 0 {
		t.Fatalf("invalid email changed user to %q epoch=%d err=%v", email, epoch, err)
	}

	duplicate := graphRequest(t, handler, mutation, map[string]any{"id": fixture.sharedUserID, "input": map[string]any{"email": strings.ToUpper(fixture.hiddenEmail)}}, fixture.sessionID, fixture.userID)
	assertUpdateUserError(t, duplicate, "email_already_used", http.StatusBadRequest)
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM user_sessions WHERE user_id=$1`, fixture.sharedUserID).Scan(&epoch); err != nil || epoch != 2 {
		t.Fatalf("duplicate email changed session count to %d err=%v; want 2", epoch, err)
	}

	updated := graphRequest(t, handler, mutation, map[string]any{"id": fixture.sharedUserID, "input": map[string]any{"email": "AdminUpdated@Example.test", "name": "Renamed"}}, fixture.sessionID, fixture.userID)
	if failures, ok := updated["errors"].([]any); ok && len(failures) != 0 {
		t.Fatalf("admin updateUser errors = %#v", failures)
	}
	updatedUser := updated["data"].(map[string]any)["updateUser"].(map[string]any)
	if updatedUser["name"] != "Renamed" || updatedUser["email"] != "AdminUpdated@Example.test" || updatedUser["emailVerified"] != true || updatedUser["disabled"] != false {
		t.Fatalf("admin updateUser result = %#v", updatedUser)
	}
	var verifiedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT auth_epoch,email_verified FROM users WHERE id=$1`, fixture.sharedUserID).Scan(&epoch, &verifiedAt); err != nil {
		t.Fatal(err)
	}
	if epoch != 1 || verifiedAt == nil {
		t.Fatalf("email update auth epoch/verification = %d/%v", epoch, verifiedAt)
	}
	var retainedSessions, retainedAuthSessions, retainedRefreshTokens int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM user_sessions WHERE user_id=$1`, fixture.sharedUserID).Scan(&retainedSessions); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM auth_sessions WHERE id=$1`, authSessionID).Scan(&retainedAuthSessions); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM auth_refresh_tokens WHERE id=$1`, refreshTokenID).Scan(&retainedRefreshTokens); err != nil {
		t.Fatal(err)
	}
	if retainedSessions != 0 || retainedAuthSessions != 0 || retainedRefreshTokens != 0 {
		t.Fatalf("email update retained sessions: cookie=%d auth=%d refresh=%d", retainedSessions, retainedAuthSessions, retainedRefreshTokens)
	}
	if !reflect.DeepEqual(publisher.userIDs, []string{fixture.sharedUserID}) {
		t.Fatalf("updateUser publications = %#v, want one profile update", publisher.userIDs)
	}

	nameOnly := graphRequest(t, handler, mutation, map[string]any{"id": fixture.sharedUserID, "input": map[string]any{"name": "Renamed Again"}}, fixture.sessionID, fixture.userID)
	if failures, ok := nameOnly["errors"].([]any); ok && len(failures) != 0 {
		t.Fatalf("name-only updateUser errors = %#v", failures)
	}
	nameOnlyUser := nameOnly["data"].(map[string]any)["updateUser"].(map[string]any)
	if nameOnlyUser["name"] != "Renamed Again" || nameOnlyUser["email"] != "AdminUpdated@Example.test" {
		t.Fatalf("name-only updateUser = %#v", nameOnlyUser)
	}
	if !reflect.DeepEqual(publisher.userIDs, []string{fixture.sharedUserID, fixture.sharedUserID}) {
		t.Fatalf("name-only updateUser publications = %#v", publisher.userIDs)
	}

	noOp := graphRequest(t, handler, mutation, map[string]any{"id": fixture.sharedUserID, "input": map[string]any{}}, fixture.sessionID, fixture.userID)
	if failures, ok := noOp["errors"].([]any); ok && len(failures) != 0 {
		t.Fatalf("empty updateUser errors = %#v", failures)
	}
	if !reflect.DeepEqual(publisher.userIDs, []string{fixture.sharedUserID, fixture.sharedUserID}) {
		t.Fatalf("empty updateUser published an update: %#v", publisher.userIDs)
	}

	sameEmail := graphRequest(t, handler, mutation, map[string]any{"id": fixture.sharedUserID, "input": map[string]any{"email": "AdminUpdated@Example.test"}}, fixture.sessionID, fixture.userID)
	if failures, ok := sameEmail["errors"].([]any); ok && len(failures) != 0 {
		t.Fatalf("same-email updateUser errors = %#v", failures)
	}
	if !reflect.DeepEqual(publisher.userIDs, []string{fixture.sharedUserID, fixture.sharedUserID, fixture.sharedUserID}) {
		t.Fatalf("same-email updateUser publications = %#v", publisher.userIDs)
	}
}

func assertUpdateUserError(t *testing.T, response map[string]any, name string, status int) {
	t.Helper()
	failures, ok := response["errors"].([]any)
	if !ok || len(failures) != 1 {
		t.Fatalf("updateUser response = %#v, want %s error", response, name)
	}
	extensions := failures[0].(map[string]any)["extensions"].(map[string]any)
	if extensions["name"] != name || extensions["status"] != float64(status) {
		t.Fatalf("updateUser %s extensions = %#v", name, extensions)
	}
}

func TestAdminUpdateEmailSerializesCaseInsensitiveDuplicates(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	repository := users.NewRepository(pool)
	results := make(chan error, 2)
	for _, update := range []struct{ userID, email string }{
		{fixture.sharedUserID, "race@example.test"},
		{fixture.disabledUserID, "RACE@example.test"},
	} {
		update := update
		go func() {
			_, err := repository.UpdateEmail(ctx, update.userID, update.email)
			results <- err
		}()
	}
	successes, conflicts := 0, 0
	for range 2 {
		err := <-results
		switch {
		case err == nil:
			successes++
		case errors.Is(err, users.ErrEmailAlreadyUsed):
			conflicts++
		default:
			t.Fatalf("concurrent update email error = %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent update email outcomes = %d successes / %d conflicts; want one each", successes, conflicts)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE lower(email)=lower($1)`, "race@example.test").Scan(&count); err != nil || count != 1 {
		t.Fatalf("concurrent update email rows = %d, err=%v; want one", count, err)
	}
}

func TestAdminUpdateUserFeaturesGraphQLCompatibility(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	nonAdminSessionID := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO multiple_users_sessions(id) VALUES($1)`, nonAdminSessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO user_sessions(id,session_id,user_id,created_at) VALUES($1,$2,$3,now())`, uuid.NewString(), nonAdminSessionID, fixture.sharedUserID); err != nil {
		t.Fatal(err)
	}
	userRepository := users.NewRepository(pool)
	workspaceRepository := workspaces.NewRepository(pool)
	userService := users.NewService(userRepository, workspaces.NewAuthorizationService(workspaceRepository))
	publisher := &profilePublisherRecorder{}
	handler := NewHandlerWithProfilePublisher(
		config.Config{Version: "0.27.5", Deployment: "selfhosted"},
		auth.NewSessionResolver(pool), userService, workspaceRepository, nil, publisher,
	)

	mutation := `mutation updateFeatures($userId: String!, $features: [FeatureType!]!) {
		updateUserFeatures(id: $userId, features: $features)
	}`
	variables := map[string]any{"userId": fixture.sharedUserID, "features": []string{"Admin"}}

	denied := graphRequest(t, handler, mutation, variables, nonAdminSessionID, fixture.sharedUserID)
	deniedErrors, ok := denied["errors"].([]any)
	if !ok || len(deniedErrors) != 1 {
		t.Fatalf("non-admin feature update = %#v, want forbidden error", denied)
	}
	deniedExtensions := deniedErrors[0].(map[string]any)["extensions"].(map[string]any)
	if deniedExtensions["name"] != "action_forbidden" || deniedExtensions["status"] != float64(http.StatusForbidden) {
		t.Fatalf("non-admin feature update error = %#v", deniedExtensions)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM user_features WHERE user_id=$1 AND name='administrator'`, fixture.sharedUserID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("non-admin feature update wrote %d rows, err=%v", count, err)
	}

	grant := graphRequest(t, handler, mutation, variables, fixture.sessionID, fixture.userID)
	if failures, ok := grant["errors"].([]any); ok && len(failures) != 0 {
		t.Fatalf("admin feature grant errors = %#v", failures)
	}
	if got := grant["data"].(map[string]any)["updateUserFeatures"]; !reflect.DeepEqual(got, []any{"Admin"}) {
		t.Fatalf("admin feature grant result = %#v", got)
	}
	var featureType int
	var reason string
	var activated bool
	if err := pool.QueryRow(ctx, `SELECT type, reason, activated FROM user_features WHERE user_id=$1 AND name='administrator'`, fixture.sharedUserID).Scan(&featureType, &reason, &activated); err != nil {
		t.Fatal(err)
	}
	if featureType != 0 || reason != "admin panel" || !activated {
		t.Fatalf("granted feature type=%d reason=%q activated=%t", featureType, reason, activated)
	}

	duplicateGrant := graphRequest(t, handler, mutation, variables, fixture.sessionID, fixture.userID)
	if failures, ok := duplicateGrant["errors"].([]any); ok && len(failures) != 0 {
		t.Fatalf("duplicate admin feature grant errors = %#v", failures)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM user_features WHERE user_id=$1 AND name='administrator'`, fixture.sharedUserID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate grant created %d feature records, err=%v", count, err)
	}

	variables["features"] = []string{}
	revoke := graphRequest(t, handler, mutation, variables, fixture.sessionID, fixture.userID)
	if failures, ok := revoke["errors"].([]any); ok && len(failures) != 0 {
		t.Fatalf("admin feature revoke errors = %#v", failures)
	}
	if got := revoke["data"].(map[string]any)["updateUserFeatures"]; !reflect.DeepEqual(got, []any{}) {
		t.Fatalf("admin feature revoke result = %#v", got)
	}
	if err := pool.QueryRow(ctx, `SELECT activated FROM user_features WHERE user_id=$1 AND name='administrator'`, fixture.sharedUserID).Scan(&activated); err != nil || activated {
		t.Fatalf("revoked feature active=%t err=%v", activated, err)
	}
	if !reflect.DeepEqual(publisher.userIDs, []string{fixture.sharedUserID, fixture.sharedUserID, fixture.sharedUserID}) {
		t.Fatalf("user-updated publications = %#v", publisher.userIDs)
	}

	variables["userId"] = fixture.disabledUserID
	variables["features"] = []string{"Admin"}
	disabledGrant := graphRequest(t, handler, mutation, variables, fixture.sessionID, fixture.userID)
	if failures, ok := disabledGrant["errors"].([]any); ok && len(failures) != 0 {
		t.Fatalf("disabled user feature grant errors = %#v", failures)
	}
	if len(publisher.userIDs) != 3 {
		t.Fatalf("disabled user's update should not publish; got %d publications", len(publisher.userIDs))
	}
}

func TestAdminEnableUserRevokesExistingSessions(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	nonAdminSessionID := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO multiple_users_sessions(id) VALUES($1)`, nonAdminSessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO user_sessions(id,session_id,user_id,created_at) VALUES($1,$2,$3,now())`, uuid.NewString(), nonAdminSessionID, fixture.sharedUserID); err != nil {
		t.Fatal(err)
	}
	var disabledUserSessionID string
	if err := pool.QueryRow(ctx, `SELECT id FROM user_sessions WHERE session_id=$1 AND user_id=$2`, fixture.disabledSessionID, fixture.disabledUserID).Scan(&disabledUserSessionID); err != nil {
		t.Fatal(err)
	}
	oldAuthSessionID := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO auth_sessions(id,user_session_id,idle_expires_at,absolute_expires_at) VALUES($1,$2,now()+interval '1 hour',now()+interval '1 day')`, oldAuthSessionID, disabledUserSessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO auth_refresh_tokens(id,auth_session_id) VALUES($1,$2)`, uuid.NewString(), oldAuthSessionID); err != nil {
		t.Fatal(err)
	}

	userRepository := users.NewRepository(pool)
	workspaceRepository := workspaces.NewRepository(pool)
	userService := users.NewService(userRepository, workspaces.NewAuthorizationService(workspaceRepository))
	handler := NewHandler(config.Config{Version: "0.27.5", Deployment: "selfhosted"}, auth.NewSessionResolver(pool), userService, workspaceRepository)
	mutation := `mutation enableUser($id: String!) { enableUser(id: $id) { id disabled } }`
	variables := map[string]any{"id": fixture.disabledUserID}

	denied := graphRequest(t, handler, mutation, variables, nonAdminSessionID, fixture.sharedUserID)
	deniedErrors, ok := denied["errors"].([]any)
	if !ok || len(deniedErrors) != 1 {
		t.Fatalf("non-admin enableUser = %#v, want forbidden error", denied)
	}
	deniedExtensions := deniedErrors[0].(map[string]any)["extensions"].(map[string]any)
	if deniedExtensions["name"] != "action_forbidden" || deniedExtensions["status"] != float64(http.StatusForbidden) {
		t.Fatalf("non-admin enableUser error = %#v", deniedExtensions)
	}
	var disabled bool
	var authEpoch int
	if err := pool.QueryRow(ctx, `SELECT disabled,auth_epoch FROM users WHERE id=$1`, fixture.disabledUserID).Scan(&disabled, &authEpoch); err != nil || !disabled || authEpoch != 0 {
		t.Fatalf("denied enableUser changed user state disabled=%t auth_epoch=%d err=%v", disabled, authEpoch, err)
	}

	enabled := graphRequest(t, handler, mutation, variables, fixture.sessionID, fixture.userID)
	if failures, ok := enabled["errors"].([]any); ok && len(failures) != 0 {
		t.Fatalf("admin enableUser errors = %#v", failures)
	}
	user := enabled["data"].(map[string]any)["enableUser"].(map[string]any)
	if user["id"] != fixture.disabledUserID || user["disabled"] != false {
		t.Fatalf("enableUser result = %#v", user)
	}
	if err := pool.QueryRow(ctx, `SELECT disabled,auth_epoch FROM users WHERE id=$1`, fixture.disabledUserID).Scan(&disabled, &authEpoch); err != nil || disabled || authEpoch != 1 {
		t.Fatalf("enabled user state disabled=%t auth_epoch=%d err=%v", disabled, authEpoch, err)
	}
	for _, table := range []string{"user_sessions", "auth_sessions", "auth_refresh_tokens"} {
		var count int
		query := `SELECT count(*) FROM ` + table + ` WHERE `
		switch table {
		case "user_sessions":
			query += `user_id=$1`
			if err := pool.QueryRow(ctx, query, fixture.disabledUserID).Scan(&count); err != nil {
				t.Fatal(err)
			}
		case "auth_sessions":
			query += `id=$1`
			if err := pool.QueryRow(ctx, query, oldAuthSessionID).Scan(&count); err != nil {
				t.Fatal(err)
			}
		case "auth_refresh_tokens":
			query += `auth_session_id=$1`
			if err := pool.QueryRow(ctx, query, oldAuthSessionID).Scan(&count); err != nil {
				t.Fatal(err)
			}
		}
		if count != 0 {
			t.Fatalf("enableUser retained %d rows in %s", count, table)
		}
	}
	secondEnable := graphRequest(t, handler, mutation, variables, fixture.sessionID, fixture.userID)
	if failures, ok := secondEnable["errors"].([]any); ok && len(failures) != 0 {
		t.Fatalf("repeated enableUser errors = %#v", failures)
	}
	if err := pool.QueryRow(ctx, `SELECT auth_epoch FROM users WHERE id=$1`, fixture.disabledUserID).Scan(&authEpoch); err != nil || authEpoch != 2 {
		t.Fatalf("repeated enableUser auth_epoch=%d err=%v, want 2", authEpoch, err)
	}

	oldSession := graphRequest(t, handler, `query { currentUser { id } }`, nil, fixture.disabledSessionID, fixture.disabledUserID)
	if oldSession["data"].(map[string]any)["currentUser"] != nil {
		t.Fatalf("enableUser kept old cookie session active: %#v", oldSession)
	}
}
