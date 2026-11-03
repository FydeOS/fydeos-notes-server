// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/byok"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/mcpcredentials"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
	"github.com/jackc/pgx/v5/pgxpool"
)

const byokTestRootKey = "test-only stable BYOK root key"

func TestWorkspaceByokProfileLifecycleAndDisclosureBoundary(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	fixture := seedGraphQLFixture(t, ctx, pool)
	applyBYOKMigration(t, pool)
	store, handler := newBYOKGraphQLHandler(t, pool)

	settings := byokSettings(t, graphRequest(t, handler, `query($id: String!) {
		workspace(id: $id) { byokSettings { entitled serverEntitled localEntitled policy { allowedProviders } profiles { profileId } } }
	}`, map[string]any{"id": fixture.workspaceID}, fixture.sessionID, fixture.userID), "workspace")
	if settings["entitled"] != false || settings["serverEntitled"] != false || settings["localEntitled"] != false {
		t.Fatalf("unentitled settings = %#v", settings)
	}
	providers := settings["policy"].(map[string]any)["allowedProviders"].([]any)
	if got := strings.Join(anyStrings(providers), ","); got != "anthropic,fal,gemini,openai" {
		t.Fatalf("allowed provider order = %q", got)
	}
	if len(settings["profiles"].([]any)) != 0 {
		t.Fatalf("unentitled profile list = %#v", settings["profiles"])
	}
	denied := createByokProfile(t, handler, fixture.workspaceID, fixture.sessionID, fixture.userID, "No entitlement", "secret-without-entitlement")
	if message := byokGraphErrorMessage(t, denied); !strings.Contains(message, "BYOK requires") {
		t.Fatalf("unentitled mutation error = %q", message)
	}

	if _, err := pool.Exec(ctx, `INSERT INTO entitlements(target_type,target_id,source,plan,status)
		VALUES ('user',$1,'admin_grant','pro','active')`, fixture.userID); err != nil {
		t.Fatalf("grant BYOK entitlement: %v", err)
	}
	omittedDescription := graphRequest(t, handler, `mutation($input: CreateWorkspaceByokProfileInput!) {
		createWorkspaceByokProfile(input: $input) { profileId }
	}`, map[string]any{"input": byokCreateInput(fixture.workspaceID, "", "secret-omitted-description", false)}, fixture.sessionID, fixture.userID)
	if message := byokGraphErrorMessage(t, omittedDescription); !strings.Contains(message, "description must be provided explicitly") {
		t.Fatalf("omitted description error = %q", message)
	}

	first := byokGraphData(t, createByokProfile(t, handler, fixture.workspaceID, fixture.sessionID, fixture.userID, "Primary", "secret-primary"))["createWorkspaceByokProfile"].(map[string]any)
	firstID := first["profileId"].(string)
	if first["description"] != nil || first["revision"] != float64(1) || first["sortOrder"] != float64(0) {
		t.Fatalf("created profile = %#v", first)
	}
	chatTarget, err := store.ResolveChatTarget(ctx, fixture.workspaceID, fixture.userID, firstID, "gpt-6.1-sol")
	if err != nil {
		t.Fatalf("resolve entitled BYOK chat target: %v", err)
	}
	if chatTarget.Provider != "openai" || chatTarget.ModelID != "gpt-6.1-sol" || string(chatTarget.Credential.Bytes()) != "secret-primary" || chatTarget.BaseURL != "https://api.openai.com/v1" {
		t.Fatalf("resolved BYOK chat target metadata or credential mismatch")
	}
	chatTarget.Credential.Destroy()
	if _, err := store.ResolveChatTarget(ctx, fixture.workspaceID, fixture.userID, firstID, "disabled-model"); err == nil {
		t.Fatal("resolved an undeclared BYOK chat model")
	}
	embeddingDefinition := map[string]any{
		"endpoint": map[string]any{"kind": "provider_default"},
		"models": []any{map[string]any{
			"modelId": "text-embedding-3-small", "enabled": true,
			"capabilities": []any{map[string]any{
				"input": []string{"text"}, "output": []string{"embedding"}, "features": []string{},
				"attachmentKinds": []string{}, "attachmentSources": []string{},
			}},
		}},
	}
	embeddingProfileResponse := graphRequest(t, handler, `mutation($input: CreateWorkspaceByokProfileInput!) {
		createWorkspaceByokProfile(input: $input) { profileId }
	}`, map[string]any{"input": map[string]any{
		"workspaceId": fixture.workspaceID, "provider": "openai", "name": "Embedding", "description": nil,
		"credential": "secret-embedding", "enabled": true, "definition": embeddingDefinition,
	}}, fixture.sessionID, fixture.userID)
	embeddingProfileID := byokGraphData(t, embeddingProfileResponse)["createWorkspaceByokProfile"].(map[string]any)["profileId"].(string)
	embeddingTarget, err := store.ResolveServerTarget(ctx, fixture.workspaceID, fixture.userID, "embedding")
	if err != nil {
		t.Fatalf("resolve server embedding target: %v", err)
	}
	if embeddingTarget.ProfileID != embeddingProfileID || embeddingTarget.ModelID != "text-embedding-3-small" || string(embeddingTarget.Credential.Bytes()) != "secret-embedding" {
		t.Fatal("server embedding target did not resolve the configured profile credential")
	}
	embeddingTarget.Credential.Destroy()
	if deleted, err := store.Delete(ctx, fixture.workspaceID, embeddingProfileID); err != nil || !deleted {
		t.Fatalf("remove temporary embedding profile: deleted=%v err=%v", deleted, err)
	}
	if encoded, _ := json.Marshal(first); strings.Contains(string(encoded), "secret-primary") {
		t.Fatalf("profile mutation response disclosed credential: %s", encoded)
	}

	duplicate := createByokProfile(t, handler, fixture.workspaceID, fixture.sessionID, fixture.userID, "Primary", "secret-duplicate")
	if message := byokGraphErrorMessage(t, duplicate); !strings.Contains(message, "BYOK profile name already exists") {
		t.Fatalf("duplicate profile error = %q", message)
	}

	introspection := graphRequest(t, handler, `query { __type(name: "WorkspaceByokProfileType") { fields { name } } }`, nil, fixture.sessionID, fixture.userID)
	fields := byokGraphData(t, introspection)["__type"].(map[string]any)["fields"].([]any)
	for _, field := range fields {
		name := field.(map[string]any)["name"].(string)
		if name == "credential" || name == "encryptedCredential" || name == "encryptedApiKey" {
			t.Fatalf("secret-bearing profile field exposed in GraphQL schema: %s", name)
		}
	}

	validation := map[string]any{
		"definitionFingerprint": "old-fingerprint",
		"credentialGeneration":  1,
		"connection":            map[string]any{"kind": "verified", "testedAtMs": 1},
		"models": []any{map[string]any{
			"modelId": "gpt-6.1-sol",
			"checks": []any{map[string]any{
				"operation": "chat", "status": map[string]any{"kind": "verified", "testedAtMs": 1},
			}},
		}},
	}
	validationJSON, err := json.Marshal(validation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE ai_workspace_byok_configs SET validation=$2::jsonb WHERE id=$1`, firstID, validationJSON); err != nil {
		t.Fatalf("seed validation evidence: %v", err)
	}

	replaced := graphRequest(t, handler, `mutation($input: ReplaceWorkspaceByokProfileInput!) {
		replaceWorkspaceByokProfile(input: $input) { profileId name description revision validation { credentialGeneration connection { kind } models { modelId } } }
	}`, map[string]any{"input": map[string]any{
		"workspaceId": fixture.workspaceID, "profileId": firstID, "expectedRevision": 1,
		"name": "Primary updated", "description": nil, "enabled": true,
		"definition": byokDefinition(false),
	}}, fixture.sessionID, fixture.userID)
	replacedProfile := byokGraphData(t, replaced)["replaceWorkspaceByokProfile"].(map[string]any)
	if replacedProfile["revision"] != float64(2) || replacedProfile["description"] != nil {
		t.Fatalf("replaced profile = %#v", replacedProfile)
	}
	replacedValidation := replacedProfile["validation"].(map[string]any)
	if replacedValidation["credentialGeneration"] != float64(1) || replacedValidation["connection"].(map[string]any)["kind"] != "verified" || len(replacedValidation["models"].([]any)) != 0 {
		t.Fatalf("reconciled validation = %#v", replacedValidation)
	}
	assertStoredCredential(t, store, fixture.workspaceID, firstID, "secret-primary")

	stale := graphRequest(t, handler, `mutation($input: ReplaceWorkspaceByokProfileInput!) {
		replaceWorkspaceByokProfile(input: $input) { profileId }
	}`, map[string]any{"input": map[string]any{
		"workspaceId": fixture.workspaceID, "profileId": firstID, "expectedRevision": 1,
		"name": "Stale", "description": "ignored", "enabled": true, "definition": byokDefinition(false),
	}}, fixture.sessionID, fixture.userID)
	if message := byokGraphErrorMessage(t, stale); !strings.Contains(message, "byok_revision_conflict") {
		t.Fatalf("stale revision error = %q", message)
	}

	second := byokGraphData(t, createByokProfile(t, handler, fixture.workspaceID, fixture.sessionID, fixture.userID, "Secondary", "secret-secondary"))["createWorkspaceByokProfile"].(map[string]any)
	secondID := second["profileId"].(string)
	rotated := graphRequest(t, handler, `mutation($input: RotateWorkspaceByokCredentialInput!) {
		rotateWorkspaceByokCredential(input: $input) { profileId revision validation { credentialGeneration } }
	}`, map[string]any{"input": map[string]any{
		"workspaceId": fixture.workspaceID, "profileId": firstID, "expectedRevision": 2, "credential": "secret-rotated",
	}}, fixture.sessionID, fixture.userID)
	rotatedProfile := byokGraphData(t, rotated)["rotateWorkspaceByokCredential"].(map[string]any)
	if rotatedProfile["revision"] != float64(3) || rotatedProfile["validation"] != nil {
		t.Fatalf("rotated profile = %#v", rotatedProfile)
	}
	assertStoredCredential(t, store, fixture.workspaceID, firstID, "secret-rotated")

	reordered := graphRequest(t, handler, `mutation($input: ReorderWorkspaceByokProfilesInput!) {
		reorderWorkspaceByokProfiles(input: $input) { profileId sortOrder revision }
	}`, map[string]any{"input": map[string]any{
		"workspaceId": fixture.workspaceID,
		"profiles": []any{
			map[string]any{"profileId": secondID, "expectedRevision": 1},
			map[string]any{"profileId": firstID, "expectedRevision": 3},
		},
	}}, fixture.sessionID, fixture.userID)
	ordered := byokGraphData(t, reordered)["reorderWorkspaceByokProfiles"].([]any)
	if len(ordered) != 2 || ordered[0].(map[string]any)["profileId"] != secondID || ordered[0].(map[string]any)["sortOrder"] != float64(0) || ordered[1].(map[string]any)["profileId"] != firstID || ordered[1].(map[string]any)["sortOrder"] != float64(1) {
		t.Fatalf("reordered profiles = %#v", ordered)
	}
	settings = byokSettings(t, graphRequest(t, handler, `query($id: String!) {
		workspace(id: $id) { byokSettings { profiles { profileId } } }
	}`, map[string]any{"id": fixture.workspaceID}, fixture.sessionID, fixture.userID), "workspace")
	listed := settings["profiles"].([]any)
	if len(listed) != 2 || listed[0].(map[string]any)["profileId"] != secondID || listed[1].(map[string]any)["profileId"] != firstID {
		t.Fatalf("listed profiles after reorder = %#v", listed)
	}

	deleted := graphRequest(t, handler, `mutation($workspaceId: String!, $profileId: ID!) {
		deleteWorkspaceByokProfile(workspaceId: $workspaceId, profileId: $profileId)
	}`, map[string]any{"workspaceId": fixture.workspaceID, "profileId": secondID}, fixture.sessionID, fixture.userID)
	if byokGraphData(t, deleted)["deleteWorkspaceByokProfile"] != true {
		t.Fatalf("delete result = %#v", deleted)
	}
	if profiles, err := store.List(ctx, fixture.workspaceID); err != nil || len(profiles) != 1 || profiles[0].ProfileID != firstID {
		t.Fatalf("profiles after delete = %#v, error = %v", profiles, err)
	}
}

func TestWorkspaceByokUsageGraphQLParity(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	fixture := seedGraphQLFixture(t, ctx, pool)
	applyBYOKMigration(t, pool)
	_, handler := newBYOKGraphQLHandler(t, pool)
	if _, err := pool.Exec(ctx, `CREATE TABLE ai_usage_events (
		id text PRIMARY KEY, workspace_id text, provider_source text NOT NULL,
		feature_kind text NOT NULL, total_tokens bigint NOT NULL DEFAULT 0,
		created_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ai_usage_events(id,workspace_id,provider_source,feature_kind,total_tokens,created_at) VALUES
		('server-chat',$1,'byok_server','chat',9,'2026-01-01T23:30:00Z'),
		('local-chat',$1,'byok_local','chat',3,'2026-01-01T10:00:00Z'),
		('server-summary',$1,'byok_server','summarize',7,'2026-01-02T02:00:00Z'),
		('managed-chat',$1,'fydeos_managed','chat',100,'2026-01-01T12:00:00Z'),
		('end-exclusive',$1,'byok_server','chat',100,'2026-01-03T00:00:00Z'),
		('other-workspace','other-workspace','byok_server','chat',100,'2026-01-01T12:00:00Z')`, fixture.workspaceID); err != nil {
		t.Fatal(err)
	}
	query := `query($id:String!,$from:DateTime!,$to:DateTime!){workspace(id:$id){byokUsage(from:$from,to:$to){date featureKind totalTokens}}}`
	variables := map[string]any{"id": fixture.workspaceID, "from": "2026-01-01T00:00:00Z", "to": "2026-01-03T00:00:00Z"}
	result := graphRequest(t, handler, query, variables, fixture.sessionID, fixture.userID)
	if result["errors"] != nil {
		t.Fatalf("query workspace BYOK usage=%#v", result)
	}
	points := result["data"].(map[string]any)["workspace"].(map[string]any)["byokUsage"].([]any)
	if len(points) != 2 {
		t.Fatalf("usage points=%#v", points)
	}
	first := points[0].(map[string]any)
	if first["date"] != "2026-01-01T00:00:00Z" || first["featureKind"] != "chat" || first["totalTokens"] != float64(12) {
		t.Fatalf("first UTC usage point=%#v", first)
	}
	second := points[1].(map[string]any)
	if second["featureKind"] != "summarize" || second["date"] != "2026-01-02T00:00:00Z" || second["totalTokens"] != float64(7) {
		t.Fatalf("usage ordering and UTC day grouping=%#v", points)
	}
	denied := graphRequest(t, handler, query, variables, fixture.sessionID, fixture.sharedUserID)
	if denied["errors"] == nil {
		t.Fatalf("collaborator without workspace settings permission read BYOK usage: %#v", denied)
	}
}

func TestWorkspaceByokProfileRequiresWorkspaceSettingsPermission(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	fixture := seedGraphQLFixture(t, ctx, pool)
	applyBYOKMigration(t, pool)
	if _, err := pool.Exec(ctx, `INSERT INTO entitlements(target_type,target_id,source,plan,status)
		VALUES ('user',$1,'admin_grant','pro','active')`, fixture.userID); err != nil {
		t.Fatalf("grant BYOK entitlement: %v", err)
	}
	_, handler := newBYOKGraphQLHandler(t, pool)
	denied := createByokProfile(t, handler, fixture.workspaceID, fixture.sessionID, fixture.sharedUserID, "Denied", "secret-denied")
	if message := byokGraphErrorMessage(t, denied); !strings.Contains(message, "You do not have permission to access Space") {
		t.Fatalf("collaborator mutation error = %q", message)
	}
}

func TestWorkspaceByokLocalLeaseUsesLocalEntitlementAndStoresEncryptedReusableState(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	fixture := seedGraphQLFixture(t, ctx, pool)
	applyBYOKMigration(t, pool)
	applyRuntimeStatesMigration(t, pool)
	store, handler := newBYOKGraphQLHandler(t, pool)

	input := map[string]any{
		"workspaceId": fixture.workspaceID,
		"providers": []any{map[string]any{
			"provider": "openai", "name": "Local OpenAI", "description": nil,
			"credential": "local-only-secret", "enabled": true,
			"definition": byokDefinition(true),
		}},
	}
	mutation := `mutation($input: CreateWorkspaceByokLocalLeaseInput!) {
		createWorkspaceByokLocalLease(input: $input) { leaseId expiresAt }
	}`
	unauthenticated := graphRequest(t, handler, mutation, map[string]any{"input": input}, "", "")
	if message := byokGraphErrorMessage(t, unauthenticated); !strings.Contains(message, ErrAuthenticationRequired.Error()) {
		t.Fatalf("anonymous local lease error = %q", message)
	}
	unentitled := graphRequest(t, handler, mutation, map[string]any{"input": input}, fixture.sessionID, fixture.userID)
	if message := byokGraphErrorMessage(t, unentitled); !strings.Contains(message, "BYOK requires") {
		t.Fatalf("unentitled local lease error = %q", message)
	}

	if _, err := pool.Exec(ctx, `INSERT INTO entitlements(target_type,target_id,source,plan,status)
		VALUES ('user',$1,'admin_grant','pro','active')`, fixture.sharedUserID); err != nil {
		t.Fatalf("grant actor local BYOK entitlement: %v", err)
	}
	deniedMember := graphRequest(t, handler, mutation, map[string]any{"input": input}, fixture.sessionID, fixture.sharedUserID)
	if message := byokGraphErrorMessage(t, deniedMember); !strings.Contains(message, "You do not have permission to access Space") {
		t.Fatalf("workspace permission error = %q", message)
	}
	entitlement, err := store.Entitlement(ctx, fixture.workspaceID, fixture.sharedUserID)
	if err != nil {
		t.Fatalf("read local-only BYOK entitlement: %v", err)
	}
	if entitlement.Server || !entitlement.Local {
		t.Fatalf("local-only actor entitlement = %+v", entitlement)
	}
	_, localLeaseHandler := newBYOKGraphQLHandlerWithPermissions(t, pool, byokLocalLeasePermissionReader{})

	omittedDescription := map[string]any{
		"workspaceId": fixture.workspaceID,
		"providers": []any{map[string]any{
			"provider": "openai", "name": "Local OpenAI", "credential": "local-only-secret",
			"enabled": true, "definition": byokDefinition(true),
		}},
	}
	omitted := graphRequest(t, localLeaseHandler, mutation, map[string]any{"input": omittedDescription}, fixture.sessionID, fixture.sharedUserID)
	if message := byokGraphErrorMessage(t, omitted); !strings.Contains(message, "description must be provided explicitly") {
		t.Fatalf("omitted description error = %q", message)
	}

	response := graphRequest(t, localLeaseHandler, mutation, map[string]any{"input": input}, fixture.sessionID, fixture.sharedUserID)
	result := byokGraphData(t, response)["createWorkspaceByokLocalLease"].(map[string]any)
	leaseID := result["leaseId"].(string)
	localTarget, err := store.ResolveLocalChatTarget(ctx, fixture.workspaceID, fixture.sharedUserID, leaseID, leaseID+":0", "gpt-6.1-sol")
	if err != nil {
		t.Fatalf("resolve authorized Electron local target: %v", err)
	}
	if localTarget.Source != "byok_local" || localTarget.Provider != "openai" || localTarget.ModelID != "gpt-6.1-sol" || localTarget.Credential == nil || string(localTarget.Credential.Bytes()) != "local-only-secret" {
		t.Fatalf("resolved local target = %+v", localTarget)
	}
	localTarget.Credential.Destroy()
	for _, attempt := range []struct{ workspaceID, actorID, leaseID, profileID, modelID string }{
		{fixture.workspaceID, fixture.userID, leaseID, leaseID + ":0", "gpt-6.1-sol"},
		{fixture.workspaceID, fixture.sharedUserID, "other-lease", leaseID + ":0", "gpt-6.1-sol"},
		{fixture.workspaceID, fixture.sharedUserID, leaseID, leaseID + ":1", "gpt-6.1-sol"},
	} {
		if target, err := store.ResolveLocalChatTarget(ctx, attempt.workspaceID, attempt.actorID, attempt.leaseID, attempt.profileID, attempt.modelID); err == nil {
			if target.Credential != nil {
				target.Credential.Destroy()
			}
			t.Fatalf("unauthorized local route was accepted: %+v", attempt)
		}
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, result["expiresAt"].(string))
	if err != nil {
		t.Fatalf("parse local lease expiry: %v", err)
	}
	remaining := time.Until(expiresAt)
	if remaining < 9*time.Minute || remaining > 10*time.Minute+time.Second {
		t.Fatalf("local lease expiry remaining = %s, want about ten minutes", remaining)
	}

	repeated := graphRequest(t, localLeaseHandler, mutation, map[string]any{"input": input}, fixture.sessionID, fixture.sharedUserID)
	repeatedResult := byokGraphData(t, repeated)["createWorkspaceByokLocalLease"].(map[string]any)
	if repeatedResult["leaseId"] != leaseID || repeatedResult["expiresAt"] != result["expiresAt"] {
		t.Fatalf("repeated local lease = %#v, initial = %#v", repeatedResult, result)
	}

	const concurrentRequests = 6
	var group sync.WaitGroup
	leaseIDs := make(chan string, concurrentRequests)
	for range concurrentRequests {
		group.Add(1)
		go func() {
			defer group.Done()
			concurrent := graphRequest(t, localLeaseHandler, mutation, map[string]any{"input": input}, fixture.sessionID, fixture.sharedUserID)
			created := byokGraphData(t, concurrent)["createWorkspaceByokLocalLease"].(map[string]any)
			leaseIDs <- created["leaseId"].(string)
		}()
	}
	group.Wait()
	close(leaseIDs)
	for concurrentLeaseID := range leaseIDs {
		if concurrentLeaseID != leaseID {
			t.Fatalf("concurrent local lease = %q, want reused lease %q", concurrentLeaseID, leaseID)
		}
	}

	var storedPayload []byte
	if err := pool.QueryRow(ctx, `SELECT payload FROM runtime_states WHERE purpose='copilot_byok_local_lease'`).Scan(&storedPayload); err != nil {
		t.Fatalf("read local lease payload: %v", err)
	}
	var payload struct {
		WorkspaceID string `json:"workspaceId"`
		UserID      string `json:"userId"`
		Providers   []struct {
			Provider            string                 `json:"provider"`
			EncryptedCredential string                 `json:"encryptedCredential"`
			Definition          byok.ProfileDefinition `json:"definition"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(storedPayload, &payload); err != nil {
		t.Fatalf("decode local lease payload: %v", err)
	}
	if strings.Contains(string(storedPayload), "local-only-secret") || payload.WorkspaceID != fixture.workspaceID || payload.UserID != fixture.sharedUserID || len(payload.Providers) != 1 {
		t.Fatalf("local lease payload exposed or lost fields: %s", storedPayload)
	}
	key, err := byok.NewCredentialEnvelopeKey([]byte(byokTestRootKey))
	if err != nil {
		t.Fatalf("derive local lease envelope key: %v", err)
	}
	defer key.Destroy()
	credential, err := key.Decrypt(payload.Providers[0].EncryptedCredential, byok.LocalAAD(fixture.workspaceID, fixture.sharedUserID, leaseID, 0, "openai", "default"))
	if err != nil {
		t.Fatalf("decrypt stored local credential: %v", err)
	}
	defer credential.Destroy()
	if string(credential.Bytes()) != "local-only-secret" || payload.Providers[0].Definition.Models[0].ModelID != "gpt-6.1-sol" {
		t.Fatalf("decrypted local lease provider = %#v", payload.Providers[0])
	}
	var leaseRows, activeRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM runtime_states WHERE purpose='copilot_byok_local_lease'`).Scan(&leaseRows); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM runtime_states WHERE purpose='copilot_byok_local_lease:active'`).Scan(&activeRows); err != nil {
		t.Fatal(err)
	}
	if leaseRows != 1 || activeRows != 1 {
		t.Fatalf("lease rows = %d, active rows = %d, want 1 each", leaseRows, activeRows)
	}
}

func TestWorkspaceByokProfileAndDraftProbesPersistOnlyCurrentEvidence(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	fixture := seedGraphQLFixture(t, ctx, pool)
	applyBYOKMigration(t, pool)
	store, handler := newBYOKGraphQLHandler(t, pool)
	store.SetProbeRunner(graphByokProbeRunner(func(_ context.Context, target byok.ProbeTarget) byok.ProbeStatus {
		if target.ModelID != "gpt-6.1-sol" || target.Operation != "chat" || string(target.Credential) != "secret-primary" {
			t.Fatalf("profile probe target = %#v", target)
		}
		return byokProbeVerified()
	}))

	denied := graphRequest(t, handler, `mutation($input: ProbeWorkspaceByokDraftInput!) {
		probeWorkspaceByokDraft(input: $input) { connection { kind } }
	}`, map[string]any{"input": map[string]any{
		"workspaceId": fixture.workspaceID, "provider": "openai", "credential": "unentitled-key",
		"definition": byokDefinition(true), "checks": []any{map[string]any{"modelId": "gpt-6.1-sol", "operation": "chat"}},
	}}, fixture.sessionID, fixture.userID)
	if message := byokGraphErrorMessage(t, denied); !strings.Contains(message, "BYOK requires") {
		t.Fatalf("unentitled draft probe error = %q", message)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO entitlements(target_type,target_id,source,plan,status)
		VALUES ('user',$1,'admin_grant','pro','active')`, fixture.userID); err != nil {
		t.Fatalf("grant BYOK entitlement: %v", err)
	}
	created := byokGraphData(t, createByokProfile(t, handler, fixture.workspaceID, fixture.sessionID, fixture.userID, "Primary", "secret-primary"))["createWorkspaceByokProfile"].(map[string]any)
	profileID := created["profileId"].(string)

	profileProbe := graphRequest(t, handler, `mutation($input: ProbeWorkspaceByokProfileInput!) {
		probeWorkspaceByokProfile(input: $input) {
			definitionFingerprint stale connection { kind testedAt errorKind }
			models { modelId checks { operation status { kind testedAt errorKind } } }
		}
	}`, map[string]any{"input": map[string]any{
		"workspaceId": fixture.workspaceID, "profileId": profileID,
		"checks": []any{map[string]any{"modelId": "gpt-6.1-sol", "operation": "chat"}},
	}}, fixture.sessionID, fixture.userID)
	probeResult := byokGraphData(t, profileProbe)["probeWorkspaceByokProfile"].(map[string]any)
	if probeResult["stale"] != false || probeResult["connection"].(map[string]any)["kind"] != "verified" {
		t.Fatalf("profile probe result = %#v", probeResult)
	}
	if resultModels := probeResult["models"].([]any); len(resultModels) != 1 || resultModels[0].(map[string]any)["checks"].([]any)[0].(map[string]any)["status"].(map[string]any)["kind"] != "verified" {
		t.Fatalf("profile probe model evidence = %#v", resultModels)
	}

	settings := byokSettings(t, graphRequest(t, handler, `query($id: String!) {
		workspace(id: $id) { byokSettings { profiles { profileId revision validation { credentialGeneration connection { kind } models { modelId } } } } }
	}`, map[string]any{"id": fixture.workspaceID}, fixture.sessionID, fixture.userID), "workspace")
	profiles := settings["profiles"].([]any)
	if len(profiles) != 1 {
		t.Fatalf("settings profiles = %#v", profiles)
	}
	profile := profiles[0].(map[string]any)
	validation := profile["validation"].(map[string]any)
	if profile["profileId"] != profileID || profile["revision"] != float64(1) || validation["credentialGeneration"] != float64(1) || validation["connection"].(map[string]any)["kind"] != "verified" {
		t.Fatalf("persisted probe evidence = %#v", profile)
	}

	store.SetProbeRunner(graphByokProbeRunner(func(_ context.Context, target byok.ProbeTarget) byok.ProbeStatus {
		if string(target.Credential) != "draft-secret" {
			t.Fatalf("draft probe credential = %q", string(target.Credential))
		}
		return byokProbeVerified()
	}))
	draftProbe := graphRequest(t, handler, `mutation($input: ProbeWorkspaceByokDraftInput!) {
		probeWorkspaceByokDraft(input: $input) { stale connection { kind } }
	}`, map[string]any{"input": map[string]any{
		"workspaceId": fixture.workspaceID, "provider": "openai", "credential": "draft-secret",
		"definition": byokDefinition(true), "checks": []any{map[string]any{"modelId": "gpt-6.1-sol", "operation": "chat"}},
	}}, fixture.sessionID, fixture.userID)
	draftResult := byokGraphData(t, draftProbe)["probeWorkspaceByokDraft"].(map[string]any)
	if draftResult["stale"] != false || draftResult["connection"].(map[string]any)["kind"] != "verified" {
		t.Fatalf("draft probe result = %#v", draftResult)
	}

	store.SetProbeRunner(graphByokProbeRunner(func(callCtx context.Context, _ byok.ProbeTarget) byok.ProbeStatus {
		if _, err := pool.Exec(callCtx, `UPDATE ai_workspace_byok_configs SET revision=revision+1 WHERE id=$1`, profileID); err != nil {
			t.Fatalf("concurrent profile update: %v", err)
		}
		return byokProbeVerified()
	}))
	staleProbe := graphRequest(t, handler, `mutation($input: ProbeWorkspaceByokProfileInput!) {
		probeWorkspaceByokProfile(input: $input) { stale connection { kind } }
	}`, map[string]any{"input": map[string]any{
		"workspaceId": fixture.workspaceID, "profileId": profileID,
		"checks": []any{map[string]any{"modelId": "gpt-6.1-sol", "operation": "chat"}},
	}}, fixture.sessionID, fixture.userID)
	staleResult := byokGraphData(t, staleProbe)["probeWorkspaceByokProfile"].(map[string]any)
	if staleResult["stale"] != true || staleResult["connection"].(map[string]any)["kind"] != "verified" {
		t.Fatalf("stale profile probe result = %#v", staleResult)
	}
}

type graphByokProbeRunner func(context.Context, byok.ProbeTarget) byok.ProbeStatus

func (runner graphByokProbeRunner) Probe(ctx context.Context, target byok.ProbeTarget) byok.ProbeStatus {
	return runner(ctx, target)
}

func byokProbeVerified() byok.ProbeStatus {
	testedAt := time.Now().UnixMilli()
	return byok.ProbeStatus{Kind: "verified", TestedAtMS: &testedAt}
}

func newBYOKGraphQLHandler(t *testing.T, pool *pgxpool.Pool) (*byok.ProfileStore, http.Handler) {
	workspaceRepository := workspaces.NewRepository(pool)
	return newBYOKGraphQLHandlerWithPermissions(t, pool, workspaces.NewAuthorizationService(workspaceRepository))
}

func newBYOKGraphQLHandlerWithPermissions(t *testing.T, pool *pgxpool.Pool, permissionReader WorkspaceRolePermissionsReader) (*byok.ProfileStore, http.Handler) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO app_configs(id,value) VALUES ('copilot.enabled','true'::jsonb) ON CONFLICT (id) DO UPDATE SET value=EXCLUDED.value`); err != nil {
		t.Fatal(err)
	}
	keyLoader := func(context.Context) (string, error) { return byokTestRootKey, nil }
	store := byok.NewProfileStore(pool, "", "cloud", keyLoader)
	workspaceRepository := workspaces.NewRepository(pool)
	cfg := config.Config{Version: "0.27.5", Deployment: "cloud", Host: "localhost", Port: 3010}
	handler := NewHandlerWithServices(cfg, auth.NewSessionResolver(pool), nil, workspaceRepository, nil, nil, nil, nil, HandlerDependencies{
		BYOKProfiles: store, MCPCredentials: mcpcredentials.NewService(pool, cfg),
		WorkspacePermissionReader: permissionReader,
	})
	return store, handler
}

type byokLocalLeasePermissionReader struct{}

func (byokLocalLeasePermissionReader) GetWorkspaceRolePermissions(context.Context, string, string) (workspaces.WorkspaceAccess, error) {
	return workspaces.WorkspaceAccess{Permissions: map[string]bool{
		"Workspace_Copilot": true, "Workspace_Settings_Update": true,
	}}, nil
}

func applyBYOKMigration(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	applyInitialSchemaObjects(t, context.Background(), pool, "ai_workspace_byok_configs")
}

func applyRuntimeStatesMigration(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	applyInitialSchemaObjects(t, context.Background(), pool, "runtime_states")
}

func createByokProfile(t *testing.T, handler http.Handler, workspaceID, sessionID, userID, name, credential string) map[string]any {
	t.Helper()
	return graphRequest(t, handler, `mutation($input: CreateWorkspaceByokProfileInput!) {
		createWorkspaceByokProfile(input: $input) {
			profileId workspaceId provider name description enabled revision sortOrder
			definition { endpoint { kind } models { modelId enabled } }
		}
	}`, map[string]any{"input": byokCreateInput(workspaceID, name, credential, true)}, sessionID, userID)
}

func byokCreateInput(workspaceID, name, credential string, includeDescription bool) map[string]any {
	input := map[string]any{
		"workspaceId": workspaceID, "provider": "openai", "name": name,
		"credential": credential, "enabled": true, "definition": byokDefinition(true),
	}
	if includeDescription {
		input["description"] = nil
	}
	return input
}

func byokDefinition(modelEnabled bool) map[string]any {
	return map[string]any{
		"endpoint": map[string]any{"kind": "provider_default"},
		"models": []any{map[string]any{
			"modelId": "gpt-6.1-sol", "enabled": modelEnabled,
			"capabilities": []any{map[string]any{
				"input": []string{"text"}, "output": []string{"text"}, "features": []string{},
				"attachmentKinds": []string{}, "attachmentSources": []string{},
			}},
		}},
	}
}

func byokSettings(t *testing.T, response map[string]any, workspaceField string) map[string]any {
	t.Helper()
	return byokGraphData(t, response)[workspaceField].(map[string]any)["byokSettings"].(map[string]any)
}

func byokGraphData(t *testing.T, response map[string]any) map[string]any {
	t.Helper()
	if _, exists := response["errors"]; exists {
		t.Fatalf("GraphQL errors: %s", byokGraphErrorMessage(t, response))
	}
	data, ok := response["data"].(map[string]any)
	if !ok {
		t.Fatalf("GraphQL response has no data: %#v", response)
	}
	return data
}

func byokGraphErrorMessage(t *testing.T, response map[string]any) string {
	t.Helper()
	errors, ok := response["errors"].([]any)
	if !ok || len(errors) == 0 {
		t.Fatalf("GraphQL response has no errors: %#v", response)
	}
	return errors[0].(map[string]any)["message"].(string)
}

func anyStrings(values []any) []string {
	result := make([]string, len(values))
	for index, value := range values {
		result[index] = value.(string)
	}
	return result
}

func assertStoredCredential(t *testing.T, store *byok.ProfileStore, workspaceID, profileID, want string) {
	t.Helper()
	profiles, err := store.List(context.Background(), workspaceID)
	if err != nil {
		t.Fatalf("list encrypted profiles: %v", err)
	}
	var encrypted string
	var profile byok.Profile
	for _, candidate := range profiles {
		if candidate.ProfileID == profileID {
			encrypted, profile = candidate.EncryptedCredential, candidate
			break
		}
	}
	if encrypted == "" || !strings.HasPrefix(encrypted, "byok:v1:") || strings.Contains(encrypted, want) {
		t.Fatalf("stored credential envelope = %q", encrypted)
	}
	key, err := byok.NewCredentialEnvelopeKey([]byte(byokTestRootKey))
	if err != nil {
		t.Fatal(err)
	}
	defer key.Destroy()
	credential, err := key.Decrypt(encrypted, byok.ServerAAD(workspaceID, profileID, profile.Provider, "default"))
	if err != nil {
		t.Fatalf("decrypt stored credential: %v", err)
	}
	defer credential.Destroy()
	if got := string(credential.Bytes()); got != want {
		t.Fatalf("stored credential = %q, want %q", got, want)
	}
}
