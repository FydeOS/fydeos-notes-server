// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/copilot"
	"github.com/fydeos/fydeos-notes-server/internal/mcpcredentials"
	"github.com/fydeos/fydeos-notes-server/internal/oauth"
	"github.com/fydeos/fydeos-notes-server/internal/search"
	"github.com/fydeos/fydeos-notes-server/internal/serverconfig"
)

func TestPublicServerConfigUsesDatabaseOverridesAndInitializationState(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	for _, sql := range []string{`CREATE TABLE app_configs(id text PRIMARY KEY,value jsonb NOT NULL)`, `CREATE TABLE users(id text PRIMARY KEY,disabled boolean NOT NULL DEFAULT false)`} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"server":{"name":"File name"},"flags":{"allowGuestDemoWorkspace":false},"auth":{"passwordRequirements":{"min":12,"max":64}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Deployment: "selfhosted", RuntimeConfigPath: path}
	newHandler := func() (http.Handler, *serverconfig.Service) {
		state := serverconfig.New(cfg, pool)
		return NewHandlerWithServices(cfg, noSessionAuthenticator{}, nil, nil, nil, nil, nil, nil, HandlerDependencies{
			ServerConfig:              state,
			OAuthProviders:            oauth.NewProviderCatalog(pool, cfg),
			MCPCredentials:            mcpcredentials.NewService(pool, cfg),
			CopilotWorkspaceArtifacts: copilot.NewWorkspaceArtifacts(pool, nil),
			SearchProjection:          search.NewProjectionService(pool),
		}), state
	}
	h, state := newHandler()
	query := `{serverConfig{name initialized features oauthProviders credentialsRequirement{password{minLength maxLength}}}}`
	got := queryPublicServerConfig(t, h, query)
	if got["name"] != "File name" || got["initialized"] != false || !reflect.DeepEqual(got["features"], []any{}) {
		t.Fatalf("file state = %#v", got)
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE embedding_sources(id text); CREATE TABLE embedding_indexes(id text);`); err != nil {
		t.Fatal(err)
	}
	if got := queryPublicServerConfig(t, h, `{serverConfig{features}}`)["features"]; !reflect.DeepEqual(got, []any{"CopilotEmbedding"}) {
		t.Fatalf("embedding-capable feature = %#v", got)
	}
	password := got["credentialsRequirement"].(map[string]any)["password"].(map[string]any)
	if password["minLength"] != float64(12) || password["maxLength"] != float64(64) {
		t.Fatalf("file password limits=%#v", password)
	}
	set := func(key, value string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO app_configs(id,value) VALUES($1,$2::jsonb) ON CONFLICT(id) DO UPDATE SET value=excluded.value`, key, value); err != nil {
			t.Fatal(err)
		}
	}
	set("server.name", `""`)
	set("flags.allowGuestDemoWorkspace", `true`)
	set("auth.passwordRequirements", `{"min":16,"max":48}`)
	set("oauth.providers.google", `{"clientId":"test-client","clientSecret":"test-secret"}`)
	got = queryPublicServerConfig(t, h, query)
	if got["name"] != "" || got["initialized"] != false || !equalServerFeatureSet(got["features"], "CopilotEmbedding", "OAuth", "LocalWorkspace") || !reflect.DeepEqual(got["oauthProviders"], []any{"Google"}) {
		t.Fatalf("DB overrides = %#v", got)
	}
	set("copilot.enabled", `true`)
	set("indexer.enabled", `true`)
	if got := queryPublicServerConfig(t, h, `{serverConfig{features}}`)["features"]; !equalServerFeatureSet(got, "Copilot", "CopilotEmbedding", "Indexer", "OAuth", "LocalWorkspace") {
		t.Fatalf("enabled AI/search features = %#v", got)
	}
	set("copilot.enabled", `false`)
	if got := queryPublicServerConfig(t, h, `{serverConfig{features}}`)["features"]; !equalServerFeatureSet(got, "CopilotEmbedding", "Indexer", "OAuth", "LocalWorkspace") {
		t.Fatalf("disabled Copilot feature = %#v", got)
	}
	password = got["credentialsRequirement"].(map[string]any)["password"].(map[string]any)
	if password["minLength"] != float64(16) || password["maxLength"] != float64(48) {
		t.Fatalf("DB password limits=%#v", password)
	}
	set("server.name", `null`)
	if got := queryPublicServerConfig(t, h, `{serverConfig{name}}`)["name"]; got != "FydeOS Notes" {
		t.Fatalf("null name=%v", got)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM app_configs WHERE id='server.name'`); err != nil {
		t.Fatal(err)
	}
	if got := queryPublicServerConfig(t, h, `{serverConfig{name}}`)["name"]; got != "File name" {
		t.Fatalf("cleared name=%v", got)
	}
	// Node-owned object descriptors replace, rather than merge, the baseline.
	set("auth.passwordRequirements", `{"min":20}`)
	raw, found, err := config.ReadNodeRuntimeValue(ctx, pool, path, "auth.passwordRequirements")
	var decoded map[string]any
	if err != nil || !found || json.Unmarshal(raw, &decoded) != nil || len(decoded) != 1 || decoded["min"] != float64(20) {
		t.Fatalf("Node descriptor replacement=%s %v", raw, err)
	}
	if _, _, err := state.PasswordLimits(ctx); err == nil {
		t.Fatal("partial descriptor silently inherited a max")
	}
	// Any user counts, including a disabled account, and only positive state is cached.
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,disabled) VALUES('disabled-user',true)`); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			value, err := state.Initialized(ctx)
			if err != nil || !value {
				t.Errorf("concurrent initialization=%v %v", value, err)
			}
		}()
	}
	wg.Wait()
	if _, err := pool.Exec(ctx, `DELETE FROM users`); err != nil {
		t.Fatal(err)
	}
	if got := queryPublicServerConfig(t, h, `{serverConfig{initialized}}`)["initialized"]; got != true {
		t.Fatalf("positive state was not cached: %v", got)
	}
	h, _ = newHandler()
	if got := queryPublicServerConfig(t, h, `{serverConfig{initialized}}`)["initialized"]; got != false {
		t.Fatalf("new process retained old state: %v", got)
	}
	if _, err := pool.Exec(ctx, `DROP TABLE users`); err != nil {
		t.Fatal(err)
	}
	// Selection of name must not query users or resolve malformed password settings.
	if got := queryPublicServerConfig(t, h, `{serverConfig{name}}`)["name"]; got != "File name" {
		t.Fatalf("unselected dependencies affected name: %v", got)
	}
	result := graphRequest(t, h, `{serverConfig{initialized}}`, nil, "", "")
	if result["errors"] == nil || result["data"] != nil || graphErrors(result) != "server configuration is unavailable" {
		t.Fatalf("database failure leaked or reported initialized: %#v", result)
	}
}

func equalServerFeatureSet(value any, expected ...string) bool {
	actualValues, ok := value.([]any)
	if !ok || len(actualValues) != len(expected) {
		return false
	}
	actual := make(map[string]struct{}, len(actualValues))
	for _, item := range actualValues {
		feature, ok := item.(string)
		if !ok {
			return false
		}
		actual[feature] = struct{}{}
	}
	if len(actual) != len(expected) {
		return false
	}
	for _, feature := range expected {
		if _, ok := actual[feature]; !ok {
			return false
		}
	}
	return true
}

func TestServerConfigReturnsEnabledCalDAVProviderPresets(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `CREATE TABLE app_configs(id text PRIMARY KEY,value jsonb NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"calendar":{"caldav":{"enabled":true,"providers":[{"id":"icloud","label":"iCloud","serverUrl":"https://caldav.icloud.com","requiresAppPassword":true,"docsUrl":"https://support.apple.com/"}]}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Deployment: "selfhosted", RuntimeConfigPath: path}
	state := serverconfig.New(cfg, pool)
	h := NewHandlerWithServices(cfg, noSessionAuthenticator{}, nil, nil, nil, nil, nil, nil, HandlerDependencies{ServerConfig: state})
	query := `{serverConfig{calendarCalDAVProviders{id label requiresAppPassword docsUrl}}}`
	want := []any{map[string]any{"id": "icloud", "label": "iCloud", "requiresAppPassword": true, "docsUrl": "https://support.apple.com/"}}
	if got := queryPublicServerConfig(t, h, query)["calendarCalDAVProviders"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("file CalDAV presets=%#v", got)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO app_configs(id,value) VALUES('calendar.caldav','{"enabled":false,"providers":[]}')`); err != nil {
		t.Fatal(err)
	}
	if got := queryPublicServerConfig(t, h, query)["calendarCalDAVProviders"]; !reflect.DeepEqual(got, []any{}) {
		t.Fatalf("database-disabled CalDAV presets=%#v", got)
	}
	if got := queryPublicServerConfig(t, NewHandler(config.Config{}, noSessionAuthenticator{}, nil, nil), query)["calendarCalDAVProviders"]; !reflect.DeepEqual(got, []any{}) {
		t.Fatalf("unconfigured CalDAV presets=%#v", got)
	}
}
