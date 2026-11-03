// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/users"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
	"github.com/google/uuid"
)

func TestAuthSigningKeysGraphQLCompatibility(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	createdAt := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `INSERT INTO app_configs(id,value) VALUES($1,$2::jsonb)`, "auth.session.signingKeys", `[
		{"id":"active-key","secret":"MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY","status":"active","createdAt":"`+createdAt.Format(time.RFC3339Nano)+`","source":"auto"}
	]`); err != nil {
		t.Fatal(err)
	}

	repository := users.NewRepository(pool)
	userService := users.NewService(repository, workspaces.NewAuthorizationService(workspaces.NewRepository(pool)))
	handler := NewHandlerWithServices(config.Config{Deployment: "affine"}, auth.NewSessionResolver(pool), userService, workspaces.NewRepository(pool), nil, nil, nil, nil, HandlerDependencies{
		AuthSigningKeys: auth.NewAuthSigningKeyRing(pool, config.Config{}),
	})
	response := graphRequest(t, handler, `query { authSigningKeys { id status source createdAt retiredAt verifyUntil canDelete } }`, nil, fixture.sessionID, fixture.userID)
	if response["errors"] != nil {
		t.Fatalf("admin signing key query errors = %#v", response["errors"])
	}
	keys := response["data"].(map[string]any)["authSigningKeys"].([]any)
	if len(keys) != 1 {
		t.Fatalf("admin signing keys = %#v, want one metadata entry", keys)
	}
	key := keys[0].(map[string]any)
	if key["id"] != "active-key" || key["status"] != "active" || key["source"] != "auto" || key["createdAt"] != createdAt.Format(time.RFC3339Nano) || key["retiredAt"] != nil || key["verifyUntil"] != nil || key["canDelete"] != false {
		t.Fatalf("active signing key metadata = %#v", key)
	}
	encodedResponse, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encodedResponse), "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY") {
		t.Fatal("GraphQL response exposed the signing secret")
	}

	rotateRequest := func() map[string]any {
		return graphRequest(t, handler, `mutation Rotate($expectedActiveKeyId: String!) {
		rotateAuthSigningKey(expectedActiveKeyId: $expectedActiveKeyId) { id status source createdAt retiredAt verifyUntil canDelete }
		}`, map[string]any{"expectedActiveKeyId": "active-key"}, fixture.sessionID, fixture.userID)
	}
	startRotations := make(chan struct{})
	rotationResults := make(chan map[string]any, 2)
	var rotationWait sync.WaitGroup
	rotationWait.Add(2)
	for range 2 {
		go func() {
			defer rotationWait.Done()
			<-startRotations
			rotationResults <- rotateRequest()
		}()
	}
	close(startRotations)
	rotationWait.Wait()
	close(rotationResults)
	var rotated map[string]any
	successes, staleFailures := 0, 0
	for result := range rotationResults {
		if result["errors"] == nil {
			successes++
			rotated = result
			continue
		}
		assertSigningKeyError(t, result, "invalid_app_config_input", http.StatusBadRequest)
		staleFailures++
	}
	if successes != 1 || staleFailures != 1 {
		t.Fatalf("concurrent rotations: successes=%d stale failures=%d; want exactly one each", successes, staleFailures)
	}
	if rotated["errors"] != nil {
		t.Fatalf("admin signing key rotation errors = %#v", rotated["errors"])
	}
	rotatedKeys := rotated["data"].(map[string]any)["rotateAuthSigningKey"].([]any)
	if len(rotatedKeys) != 2 {
		t.Fatalf("rotated signing key metadata = %#v, want old and new keys", rotatedKeys)
	}
	var retiring, active map[string]any
	for _, item := range rotatedKeys {
		key := item.(map[string]any)
		if key["id"] == "active-key" {
			retiring = key
		} else {
			active = key
		}
	}
	if retiring == nil || retiring["status"] != "retiring" || retiring["source"] != "auto" || retiring["canDelete"] != false || retiring["retiredAt"] == nil || retiring["verifyUntil"] == nil {
		t.Fatalf("retired signing key metadata = %#v", retiring)
	}
	if active == nil || active["status"] != "active" || active["source"] != "admin" || active["createdAt"] == nil || active["retiredAt"] != nil || active["verifyUntil"] != nil {
		t.Fatalf("new active signing key metadata = %#v", active)
	}
	var rotatedKeyringJSON []byte
	if err := pool.QueryRow(ctx, `SELECT value FROM app_configs WHERE id='auth.session.signingKeys'`).Scan(&rotatedKeyringJSON); err != nil {
		t.Fatal(err)
	}
	var storedKeys []map[string]json.RawMessage
	if err := json.Unmarshal(rotatedKeyringJSON, &storedKeys); err != nil {
		t.Fatal(err)
	}
	var storedActiveKey map[string]json.RawMessage
	for _, storedKey := range storedKeys {
		var id string
		if err := json.Unmarshal(storedKey["id"], &id); err != nil {
			t.Fatal(err)
		}
		if id == active["id"].(string) {
			storedActiveKey = storedKey
			break
		}
	}
	if storedActiveKey == nil {
		t.Fatal("rotated keyring does not contain the new active key")
	}
	for _, field := range []string{"createdAt", "source", "secret"} {
		if _, exists := storedActiveKey[field]; !exists {
			t.Fatalf("new active key omitted required stored field %q", field)
		}
	}
	for _, field := range []string{"retiredAt", "verifyUntil"} {
		if _, exists := storedActiveKey[field]; exists {
			t.Fatalf("new active key unexpectedly serialized optional field %q", field)
		}
	}
	rotatedJSON, err := json.Marshal(rotated)
	if err != nil || strings.Contains(string(rotatedJSON), "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY") {
		t.Fatalf("rotation response leaked a signing secret or could not encode: %v", err)
	}
	retiredAt, err := time.Parse(time.RFC3339Nano, retiring["retiredAt"].(string))
	if err != nil {
		t.Fatal(err)
	}
	verifyUntil, err := time.Parse(time.RFC3339Nano, retiring["verifyUntil"].(string))
	if err != nil || verifyUntil.Sub(retiredAt) < 930*time.Second {
		t.Fatalf("retiring key verification window = %s, err=%v; want at least access TTL + clock skew", verifyUntil.Sub(retiredAt), err)
	}

	stale := graphRequest(t, handler, `mutation Rotate($expectedActiveKeyId: String!) {
		rotateAuthSigningKey(expectedActiveKeyId: $expectedActiveKeyId) { id status }
	}`, map[string]any{"expectedActiveKeyId": "active-key"}, fixture.sessionID, fixture.userID)
	assertSigningKeyError(t, stale, "invalid_app_config_input", http.StatusBadRequest)
	afterStale := graphRequest(t, handler, `query { authSigningKeys { id status } }`, nil, fixture.sessionID, fixture.userID)
	if afterStale["errors"] != nil || afterStale["data"].(map[string]any)["authSigningKeys"].([]any)[1].(map[string]any)["id"] != active["id"] {
		t.Fatalf("stale rotation changed the active keyring: %#v", afterStale)
	}
	protectedDelete := graphRequest(t, handler, `mutation Delete($id: String!) { deleteAuthSigningKey(id: $id) { id } }`, map[string]any{"id": active["id"]}, fixture.sessionID, fixture.userID)
	assertSigningKeyError(t, protectedDelete, "action_forbidden", http.StatusForbidden)
	retiringDelete := graphRequest(t, handler, `mutation Delete($id: String!) { deleteAuthSigningKey(id: $id) { id } }`, map[string]any{"id": "active-key"}, fixture.sessionID, fixture.userID)
	assertSigningKeyError(t, retiringDelete, "action_forbidden", http.StatusForbidden)
	missingDelete := graphRequest(t, handler, `mutation Delete($id: String!) { deleteAuthSigningKey(id: $id) { id } }`, map[string]any{"id": "missing-key"}, fixture.sessionID, fixture.userID)
	assertSigningKeyError(t, missingDelete, "invalid_app_config_input", http.StatusBadRequest)

	var stored []map[string]any
	var storedJSON []byte
	if err := pool.QueryRow(ctx, `SELECT value FROM app_configs WHERE id='auth.session.signingKeys'`).Scan(&storedJSON); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(storedJSON, &stored); err != nil {
		t.Fatal(err)
	}
	oldRetiredAt := time.Now().UTC().Add(-time.Hour)
	oldVerifyUntil := oldRetiredAt.Add(930 * time.Second)
	for _, key := range stored {
		if key["id"] == "active-key" {
			key["retiredAt"] = oldRetiredAt.Format(time.RFC3339Nano)
			key["verifyUntil"] = oldVerifyUntil.Format(time.RFC3339Nano)
		}
	}
	storedJSON, err = json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE app_configs SET value=$1::jsonb WHERE id='auth.session.signingKeys'`, string(storedJSON)); err != nil {
		t.Fatal(err)
	}
	deleted := graphRequest(t, handler, `mutation Delete($id: String!) { deleteAuthSigningKey(id: $id) { id status source canDelete } }`, map[string]any{"id": "active-key"}, fixture.sessionID, fixture.userID)
	if deleted["errors"] != nil {
		t.Fatalf("eligible signing key deletion errors = %#v", deleted["errors"])
	}
	remaining := deleted["data"].(map[string]any)["deleteAuthSigningKey"].([]any)
	if len(remaining) != 1 || remaining[0].(map[string]any)["id"] != active["id"] {
		t.Fatalf("remaining signing keys after retiring-key deletion = %#v", remaining)
	}
	if strings.Contains(fmt.Sprint(deleted), "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY") {
		t.Fatal("signing key deletion response exposed a secret")
	}
	var lastUpdatedBy string
	if err := pool.QueryRow(ctx, `SELECT last_updated_by FROM app_configs WHERE id='auth.session.signingKeys'`).Scan(&lastUpdatedBy); err != nil || lastUpdatedBy != fixture.userID {
		t.Fatalf("signing key audit actor = %q, err=%v", lastUpdatedBy, err)
	}

	nonAdminSessionID := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO multiple_users_sessions(id) VALUES($1)`, nonAdminSessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO user_sessions(id,session_id,user_id,created_at) VALUES($1,$2,$3,now())`, uuid.NewString(), nonAdminSessionID, fixture.sharedUserID); err != nil {
		t.Fatal(err)
	}
	denied := graphRequest(t, handler, `query { authSigningKeys { id } }`, nil, nonAdminSessionID, fixture.sharedUserID)
	assertGraphQLAdminDenied(t, denied)
	deniedRotate := graphRequest(t, handler, `mutation { rotateAuthSigningKey(expectedActiveKeyId:"`+active["id"].(string)+`") { id } }`, nil, nonAdminSessionID, fixture.sharedUserID)
	assertGraphQLAdminDenied(t, deniedRotate)
	deniedDelete := graphRequest(t, handler, `mutation { deleteAuthSigningKey(id:"`+active["id"].(string)+`") { id } }`, nil, nonAdminSessionID, fixture.sharedUserID)
	assertGraphQLAdminDenied(t, deniedDelete)
	anonymous := graphRequest(t, handler, `query { authSigningKeys { id } }`, nil, "", "")
	assertGraphQLAdminDenied(t, anonymous)
}

func assertGraphQLAdminDenied(t *testing.T, response map[string]any) {
	t.Helper()
	errorsValue, ok := response["errors"].([]any)
	if !ok || len(errorsValue) != 1 {
		t.Fatalf("non-admin GraphQL request = %#v, want forbidden error", response)
	}
	extensions, ok := errorsValue[0].(map[string]any)["extensions"].(map[string]any)
	if !ok || extensions["name"] != "action_forbidden" || extensions["status"] != float64(http.StatusForbidden) {
		t.Fatalf("GraphQL authorization error = %#v", extensions)
	}
}

func assertSigningKeyError(t *testing.T, response map[string]any, name string, status int) {
	t.Helper()
	errorsValue, ok := response["errors"].([]any)
	if !ok || len(errorsValue) != 1 {
		t.Fatalf("signing key operation = %#v, want %s error", response, name)
	}
	extensions, ok := errorsValue[0].(map[string]any)["extensions"].(map[string]any)
	if !ok || extensions["name"] != name || extensions["status"] != float64(status) {
		t.Fatalf("signing key operation error = %#v, want name=%q status=%d", extensions, name, status)
	}
}
