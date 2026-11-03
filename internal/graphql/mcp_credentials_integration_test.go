// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/mcpcredentials"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
	"github.com/jackc/pgx/v5/pgxpool"
)

const mcpCredentialFields = `id name workspaceId accessMode fingerprint createdAt expiresAt lastUsedAt revokedAt graceEndsAt status`

func TestMCPCredentialGraphQLLifecycleAndEndpointBinding(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	applyMCPCredentialMigration(t, ctx, pool)
	if _, err := pool.Exec(ctx, `INSERT INTO app_configs(id,value) VALUES ('copilot.enabled','true'::jsonb) ON CONFLICT (id) DO UPDATE SET value=EXCLUDED.value`); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{Deployment: "cloud", NodeEnv: "production", Namespace: "production", Host: "localhost", Port: 3010}
	manager := mcpcredentials.NewService(pool, cfg)
	repository := workspaces.NewRepository(pool)
	handler := NewHandlerWithServices(cfg, auth.NewSessionResolver(pool), nil, repository, nil, nil, nil, nil, HandlerDependencies{
		WorkspacePermissionReader: workspaces.NewAuthorizationService(repository),
		MCPCredentials:            manager,
	})

	available := graphRequest(t, handler, `query { mcpCredentialReadWriteAvailable }`, nil, "", "")
	if available["errors"] != nil || available["data"].(map[string]any)["mcpCredentialReadWriteAvailable"] != false {
		t.Fatalf("production read/write availability = %#v", available)
	}
	created := graphRequest(t, handler, `mutation Create($input: CreateMcpCredentialInput!) {
		createMcpCredential(input: $input) { credential { `+mcpCredentialFields+` } token }
	}`, map[string]any{"input": map[string]any{"workspaceId": fixture.workspaceID, "name": "  Claude Desktop  "}}, fixture.sessionID, fixture.userID)
	if created["errors"] != nil {
		t.Fatalf("create MCP credential errors = %#v", created["errors"])
	}
	createdResult := created["data"].(map[string]any)["createMcpCredential"].(map[string]any)
	credential := createdResult["credential"].(map[string]any)
	if credential["name"] != "Claude Desktop" || credential["workspaceId"] != fixture.workspaceID || credential["accessMode"] != "READ_ONLY" || credential["status"] != "ACTIVE" || credential["graceEndsAt"] != nil || credential["lastUsedAt"] != nil {
		t.Fatalf("created credential projection = %#v", credential)
	}
	if _, ok := credential["fingerprint"].(string); !ok {
		t.Fatalf("created credential fingerprint = %#v", credential["fingerprint"])
	}
	token := createdResult["token"].(string)
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != mcpcredentials.TokenPrefix || parts[1] != credential["id"] {
		t.Fatalf("issued token shape = %q", token)
	}
	digest := sha256.Sum256([]byte(parts[2]))
	wantHash := hex.EncodeToString(digest[:])
	var storedHash, fingerprint string
	if err := pool.QueryRow(ctx, `SELECT secret_hash,fingerprint FROM mcp_credentials WHERE id=$1`, parts[1]).Scan(&storedHash, &fingerprint); err != nil {
		t.Fatal(err)
	}
	if storedHash != wantHash || storedHash == parts[2] || fingerprint != wantHash[:12] {
		t.Fatalf("stored secret material hash=%q fingerprint=%q, want digest and prefix", storedHash, fingerprint)
	}
	if _, err := manager.Authenticate(ctx, token, fixture.workspaceID); err != nil {
		t.Fatalf("authenticate issued token on bound workspace: %v", err)
	}
	if _, err := manager.Authenticate(ctx, token, fixture.hiddenWorkspaceID); !errors.Is(err, mcpcredentials.ErrUnauthorized) {
		t.Fatalf("authenticate token on other workspace = %v, want unauthorized", err)
	}
	wrongToken := parts[0] + "." + parts[1] + ".wrong-secret"
	if _, err := manager.Authenticate(ctx, wrongToken, fixture.workspaceID); !errors.Is(err, mcpcredentials.ErrUnauthorized) {
		t.Fatalf("authenticate wrong secret = %v, want unauthorized", err)
	}

	listed := graphRequest(t, handler, `query List($workspaceId: String!) { mcpCredentials(workspaceId: $workspaceId) { `+mcpCredentialFields+` } }`, map[string]any{"workspaceId": fixture.workspaceID}, fixture.sessionID, fixture.userID)
	if listed["errors"] != nil {
		t.Fatalf("list MCP credentials errors = %#v", listed["errors"])
	}
	listedCredentials := listed["data"].(map[string]any)["mcpCredentials"].([]any)
	if len(listedCredentials) != 1 {
		t.Fatalf("listed MCP credentials = %#v, want one", listedCredentials)
	}
	if listedCredentials[0].(map[string]any)["lastUsedAt"] == nil {
		t.Fatal("successful authentication did not record lastUsedAt")
	}

	rotate := func() map[string]any {
		return graphRequest(t, handler, `mutation Rotate($id: ID!, $workspaceId: String!, $expirationDays: Int!) {
			rotateMcpCredential(id: $id, workspaceId: $workspaceId, expirationDays: $expirationDays) { credential { `+mcpCredentialFields+` } token }
		}`, map[string]any{"id": credential["id"], "workspaceId": fixture.workspaceID, "expirationDays": 30}, fixture.sessionID, fixture.userID)
	}
	start := make(chan struct{})
	results := make(chan map[string]any, 2)
	for range 2 {
		go func() {
			<-start
			results <- rotate()
		}()
	}
	close(start)
	first, second := <-results, <-results
	successes, stale := 0, 0
	var rotated map[string]any
	for _, result := range []map[string]any{first, second} {
		if result["errors"] == nil {
			successes++
			rotated = result["data"].(map[string]any)["rotateMcpCredential"].(map[string]any)
			continue
		}
		assertMCPCredentialError(t, result, mcpcredentials.ErrCredentialNotFound.Error(), http.StatusBadRequest)
		stale++
	}
	if successes != 1 || stale != 1 {
		t.Fatalf("concurrent rotation results: successes=%d stale=%d", successes, stale)
	}
	rotatedCredential := rotated["credential"].(map[string]any)
	newToken := rotated["token"].(string)
	if rotatedCredential["status"] != "ROTATING" || rotatedCredential["graceEndsAt"] == nil || rotatedCredential["accessMode"] != "READ_ONLY" {
		t.Fatalf("rotated credential projection = %#v", rotatedCredential)
	}
	if _, err := manager.Authenticate(ctx, token, fixture.workspaceID); err != nil {
		t.Fatalf("old token must remain valid during rotation grace: %v", err)
	}
	if _, err := manager.Authenticate(ctx, newToken, fixture.workspaceID); err != nil {
		t.Fatalf("authenticate replacement token: %v", err)
	}
	latest := graphRequest(t, handler, `query List($workspaceId: String!) { mcpCredentials(workspaceId: $workspaceId) { `+mcpCredentialFields+` } }`, map[string]any{"workspaceId": fixture.workspaceID}, fixture.sessionID, fixture.userID)
	latestCredentials := latest["data"].(map[string]any)["mcpCredentials"].([]any)
	if len(latestCredentials) != 1 || latestCredentials[0].(map[string]any)["id"] != rotatedCredential["id"] {
		t.Fatalf("family list after rotation = %#v", latestCredentials)
	}

	revoked := graphRequest(t, handler, `mutation Revoke($id: ID!, $workspaceId: String!) { revokeMcpCredential(id: $id, workspaceId: $workspaceId) }`, map[string]any{"id": rotatedCredential["id"], "workspaceId": fixture.workspaceID}, fixture.sessionID, fixture.userID)
	if revoked["errors"] != nil || revoked["data"].(map[string]any)["revokeMcpCredential"] != true {
		t.Fatalf("revoke MCP credential = %#v", revoked)
	}
	if _, err := manager.Authenticate(ctx, token, fixture.workspaceID); !errors.Is(err, mcpcredentials.ErrUnauthorized) {
		t.Fatalf("old family token after revoke = %v, want unauthorized", err)
	}
	if _, err := manager.Authenticate(ctx, newToken, fixture.workspaceID); !errors.Is(err, mcpcredentials.ErrUnauthorized) {
		t.Fatalf("replacement token after revoke = %v, want unauthorized", err)
	}
	var revokedCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM mcp_credentials WHERE family_id=$1 AND revoked_at IS NOT NULL`, credential["id"]).Scan(&revokedCount); err != nil || revokedCount != 2 {
		t.Fatalf("revoked family rows = %d, %v; want 2", revokedCount, err)
	}
}

func TestMCPCredentialGraphQLFeatureAndPermissionGates(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	applyMCPCredentialMigration(t, ctx, pool)
	if _, err := pool.Exec(ctx, `INSERT INTO app_configs(id,value) VALUES ('copilot.enabled','true'::jsonb) ON CONFLICT (id) DO UPDATE SET value=EXCLUDED.value`); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{Deployment: "cloud", NodeEnv: "production", Namespace: "production", Host: "localhost", Port: 3010}
	repository := workspaces.NewRepository(pool)
	manager := mcpcredentials.NewService(pool, cfg)
	handler := NewHandlerWithServices(cfg, auth.NewSessionResolver(pool), nil, repository, nil, nil, nil, nil, HandlerDependencies{
		WorkspacePermissionReader: workspaces.NewAuthorizationService(repository),
		MCPCredentials:            manager,
	})
	listQuery := `query List($workspaceId: String!) { mcpCredentials(workspaceId: $workspaceId) { id } }`
	variables := map[string]any{"workspaceId": fixture.workspaceID}

	unauthenticated := graphRequest(t, handler, listQuery, variables, "", "")
	if errs := unauthenticated["errors"].([]any); len(errs) != 1 || errs[0].(map[string]any)["message"] != ErrAuthenticationRequired.Error() {
		t.Fatalf("unauthenticated MCP list = %#v", unauthenticated)
	}
	denied := graphRequest(t, handler, listQuery, map[string]any{"workspaceId": fixture.hiddenWorkspaceID}, fixture.sessionID, fixture.userID)
	assertWorkspacePermissionDenied(t, denied, fixture.hiddenWorkspaceID)

	readWriteCreate := `mutation Create($input: CreateMcpCredentialInput!) { createMcpCredential(input: $input) { credential { id } token } }`
	writeDenied := graphRequest(t, handler, readWriteCreate, map[string]any{"input": map[string]any{"workspaceId": fixture.workspaceID, "name": "write", "accessMode": "READ_WRITE"}}, fixture.sessionID, fixture.userID)
	assertMCPCredentialError(t, writeDenied, mcpcredentials.ErrWriteToolsUnavailable.Error(), http.StatusBadRequest)

	devCfg := cfg
	devCfg.NodeEnv = "development"
	devHandler := NewHandlerWithServices(devCfg, auth.NewSessionResolver(pool), nil, repository, nil, nil, nil, nil, HandlerDependencies{
		WorkspacePermissionReader: workspaces.NewAuthorizationService(repository),
		MCPCredentials:            mcpcredentials.NewService(pool, devCfg),
	})
	available := graphRequest(t, devHandler, `query { mcpCredentialReadWriteAvailable }`, nil, "", "")
	if available["errors"] != nil || available["data"].(map[string]any)["mcpCredentialReadWriteAvailable"] != true {
		t.Fatalf("development read/write availability = %#v", available)
	}
	writeCreated := graphRequest(t, devHandler, readWriteCreate, map[string]any{"input": map[string]any{"workspaceId": fixture.workspaceID, "name": "write", "accessMode": "READ_WRITE"}}, fixture.sessionID, fixture.userID)
	if writeCreated["errors"] != nil {
		t.Fatalf("development READ_WRITE creation errors = %#v", writeCreated["errors"])
	}

	if _, err := pool.Exec(ctx, `INSERT INTO app_configs(id,value) VALUES ('copilot.enabled','false'::jsonb) ON CONFLICT (id) DO UPDATE SET value=EXCLUDED.value`); err != nil {
		t.Fatal(err)
	}
	disabled := graphRequest(t, devHandler, listQuery, variables, fixture.sessionID, fixture.userID)
	assertMCPCredentialError(t, disabled, mcpcredentials.ErrCopilotDisabled.Error(), http.StatusForbidden)
	disabledWrite := graphRequest(t, devHandler, readWriteCreate, map[string]any{"input": map[string]any{"workspaceId": fixture.workspaceID, "name": "blocked"}}, fixture.sessionID, fixture.userID)
	assertMCPCredentialError(t, disabledWrite, mcpcredentials.ErrCopilotDisabled.Error(), http.StatusForbidden)
}

func TestMCPCredentialExpiryAndDisabledUserAreRejected(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	applyMCPCredentialMigration(t, ctx, pool)
	if _, err := pool.Exec(ctx, `INSERT INTO app_configs(id,value) VALUES ('copilot.enabled','true'::jsonb) ON CONFLICT (id) DO UPDATE SET value=EXCLUDED.value`); err != nil {
		t.Fatal(err)
	}
	service := mcpcredentials.NewService(pool, config.Config{NodeEnv: "production", Namespace: "production"})
	issued, err := service.Create(ctx, mcpcredentials.CreateInput{UserID: fixture.userID, WorkspaceID: fixture.workspaceID, Name: "expiry", AccessMode: "READ_ONLY", ExpirationDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE mcp_credentials SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, issued.Credential.ID); err != nil {
		t.Fatal(err)
	}
	credentials, err := service.List(ctx, fixture.userID, fixture.workspaceID)
	if err != nil || len(credentials) != 1 || credentials[0].Status != "EXPIRED" {
		t.Fatalf("expired credential list = %#v, %v", credentials, err)
	}
	if _, err := service.Authenticate(ctx, issued.Token, fixture.workspaceID); !errors.Is(err, mcpcredentials.ErrUnauthorized) {
		t.Fatalf("expired token authentication = %v, want unauthorized", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE mcp_credentials SET expires_at=clock_timestamp()+interval '30 days' WHERE id=$1`, issued.Credential.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO app_configs(id,value) VALUES ('copilot.enabled','false'::jsonb) ON CONFLICT (id) DO UPDATE SET value=EXCLUDED.value`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Authenticate(ctx, issued.Token, fixture.workspaceID); !errors.Is(err, mcpcredentials.ErrCopilotDisabled) {
		t.Fatalf("Copilot-disabled token authentication = %v, want disabled", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE app_configs SET value='true'::jsonb WHERE id='copilot.enabled'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET disabled=true WHERE id=$1`, fixture.userID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Authenticate(ctx, issued.Token, fixture.workspaceID); !errors.Is(err, mcpcredentials.ErrUnauthorized) {
		t.Fatalf("disabled user's token authentication = %v, want unauthorized", err)
	}
}

func applyMCPCredentialMigration(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	applyInitialSchemaObjects(t, ctx, pool, "McpAccessMode", "mcp_credentials")
}

func assertMCPCredentialError(t *testing.T, response map[string]any, message string, status int) {
	t.Helper()
	graphErrors, ok := response["errors"].([]any)
	if !ok || len(graphErrors) != 1 {
		t.Fatalf("MCP GraphQL error response = %#v", response)
	}
	entry := graphErrors[0].(map[string]any)
	extensions := entry["extensions"].(map[string]any)
	if entry["message"] != message || extensions["status"] != float64(status) {
		t.Fatalf("MCP GraphQL error = %#v, want message %q and status %d", entry, message, status)
	}
}
