// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fydeos/fydeos-notes-server/internal/appconfig"
	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/users"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
	"github.com/google/uuid"
)

func TestAdminAuthAppConfigGraphQLReadValidateUpdateAndClear(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"auth":{"allowSignup":true,"session":{"ttl":900}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO app_configs(id,value) VALUES('auth.allowSignup','false'::jsonb)`); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Deployment: "selfhosted", RuntimeConfigPath: configPath}
	userService := users.NewService(users.NewRepository(pool), workspaces.NewAuthorizationService(workspaces.NewRepository(pool)))
	handler := NewHandlerWithServices(cfg, auth.NewSessionResolver(pool), userService, workspaces.NewRepository(pool), nil, nil, nil, nil, HandlerDependencies{
		AppConfig: appconfig.New(pool, configPath),
	})

	read := graphRequest(t, handler, `query { appConfig appConfigMetadata }`, nil, fixture.sessionID, fixture.userID)
	if read["errors"] != nil {
		t.Fatalf("app config read errors = %#v", read["errors"])
	}
	data := read["data"].(map[string]any)
	values := data["appConfig"].(map[string]any)
	authValues := values["auth"].(map[string]any)
	if authValues["allowSignup"] != false || authValues["session"].(map[string]any)["ttl"] != float64(900) {
		t.Fatalf("effective auth config = %#v", authValues)
	}
	metadata := data["appConfigMetadata"].(map[string]any)["auth"].(map[string]any)
	if metadata["allowSignup"].(map[string]any)["source"] != "database" || metadata["session"].(map[string]any)["ttl"].(map[string]any)["source"] != "file" {
		t.Fatalf("auth config source metadata = %#v", metadata)
	}

	validation := graphRequest(t, handler, `query Validate($updates: [UpdateAppConfigInput!]!) {
		validateAppConfig(updates: $updates) { module key value valid error }
	}`, map[string]any{"updates": []any{
		map[string]any{"module": "auth", "key": "token.accessTokenTtl", "value": 120},
		map[string]any{"module": "auth", "key": "token.refreshGracePeriod", "value": 90},
		map[string]any{"module": "crypto", "key": "privateKey", "value": "never-return-secret"},
		map[string]any{"module": "auth", "key": "allowSignup", "value": nil},
	}}, fixture.sessionID, fixture.userID)
	if validation["errors"] != nil {
		t.Fatalf("config validation errors = %#v", validation["errors"])
	}
	results := validation["data"].(map[string]any)["validateAppConfig"].([]any)
	if len(results) != 4 || results[0].(map[string]any)["valid"] != true || results[1].(map[string]any)["valid"] != false || results[2].(map[string]any)["valid"] != false || results[3].(map[string]any)["valid"] != false {
		t.Fatalf("validation results = %#v", results)
	}
	if results[3].(map[string]any)["error"] != "Expected boolean" {
		t.Fatalf("explicit null value error = %#v, want type validation error", results[3])
	}
	serializedValidation, err := json.Marshal(validation)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(serializedValidation), "never-return-secret") {
		t.Fatal("validation response exposed a secret value for an unsupported secret key")
	}

	mutation := `mutation Update($updates: [UpdateAppConfigInput!]!) { updateAppConfig(updates: $updates) }`
	updated := graphRequest(t, handler, mutation, map[string]any{"updates": []any{
		map[string]any{"module": "auth", "key": "allowSignup", "value": true},
		map[string]any{"module": "auth", "key": "session.ttl", "value": 1200},
	}}, fixture.sessionID, fixture.userID)
	if updated["errors"] != nil {
		t.Fatalf("app config update errors = %#v", updated["errors"])
	}
	updatedAuth := updated["data"].(map[string]any)["updateAppConfig"].(map[string]any)["auth"].(map[string]any)
	if updatedAuth["allowSignup"] != true || updatedAuth["session"].(map[string]any)["ttl"] != float64(1200) {
		t.Fatalf("updated auth values = %#v", updatedAuth)
	}
	var updatedBy *string
	if err := pool.QueryRow(ctx, `SELECT last_updated_by FROM app_configs WHERE id='auth.allowSignup'`).Scan(&updatedBy); err != nil || updatedBy == nil || *updatedBy != fixture.userID {
		t.Fatalf("app config audit actor = %v, err=%v", updatedBy, err)
	}

	cleared := graphRequest(t, handler, mutation, map[string]any{"updates": []any{
		map[string]any{"module": "auth", "key": "allowSignup", "clear": true},
	}}, fixture.sessionID, fixture.userID)
	if cleared["errors"] != nil {
		t.Fatalf("app config clear errors = %#v", cleared["errors"])
	}
	clearedAuth := cleared["data"].(map[string]any)["updateAppConfig"].(map[string]any)["auth"].(map[string]any)
	if clearedAuth["allowSignup"] != true {
		t.Fatalf("cleared auth value did not fall back to config file: %#v", clearedAuth)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM app_configs WHERE id='auth.allowSignup'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("clear retained app config row: count=%d err=%v", count, err)
	}
}

func TestAdminAuthAppConfigRejectsInvalidBatchWithoutWritesAndDeniesNonAdmins(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	cfg := config.Config{Deployment: "selfhosted"}
	userService := users.NewService(users.NewRepository(pool), workspaces.NewAuthorizationService(workspaces.NewRepository(pool)))
	handler := NewHandlerWithServices(cfg, auth.NewSessionResolver(pool), userService, workspaces.NewRepository(pool), nil, nil, nil, nil, HandlerDependencies{
		AppConfig: appconfig.New(pool, ""),
	})
	mutation := `mutation Update($updates: [UpdateAppConfigInput!]!) { updateAppConfig(updates: $updates) }`
	invalid := graphRequest(t, handler, mutation, map[string]any{"updates": []any{
		map[string]any{"module": "auth", "key": "allowSignup", "value": false},
		map[string]any{"module": "auth", "key": "token.accessTokenTtl", "value": 10},
	}}, fixture.sessionID, fixture.userID)
	assertSigningKeyError(t, invalid, "invalid_app_config_input", 400)
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM app_configs WHERE id='auth.allowSignup'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("invalid config batch partially committed: count=%d err=%v", count, err)
	}

	explicitNull := graphRequest(t, handler, mutation, map[string]any{"updates": []any{
		map[string]any{"module": "auth", "key": "allowSignup", "value": nil},
	}}, fixture.sessionID, fixture.userID)
	assertSigningKeyError(t, explicitNull, "invalid_app_config_input", 400)
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM app_configs WHERE id='auth.allowSignup'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("explicit null config was persisted: count=%d err=%v", count, err)
	}

	nonAdminSessionID := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO multiple_users_sessions(id) VALUES($1)`, nonAdminSessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO user_sessions(id,session_id,user_id,created_at) VALUES($1,$2,$3,now())`, uuid.NewString(), nonAdminSessionID, fixture.sharedUserID); err != nil {
		t.Fatal(err)
	}
	updates := map[string]any{"updates": []any{map[string]any{"module": "auth", "key": "allowSignup", "value": false}}}
	for _, test := range []struct {
		query     string
		variables map[string]any
	}{
		{query: `query { appConfig }`},
		{query: `query { appConfigMetadata }`},
		{query: `query Validate($updates: [UpdateAppConfigInput!]!) { validateAppConfig(updates: $updates) { valid } }`, variables: updates},
		{query: mutation, variables: updates},
	} {
		denied := graphRequest(t, handler, test.query, test.variables, nonAdminSessionID, fixture.sharedUserID)
		assertGraphQLAdminDenied(t, denied)
	}
	anonymous := graphRequest(t, handler, `query { appConfigMetadata }`, nil, "", "")
	assertGraphQLAdminDenied(t, anonymous)
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM app_configs WHERE id='auth.allowSignup'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rejected non-admin app config updates persisted: count=%d err=%v", count, err)
	}
}
