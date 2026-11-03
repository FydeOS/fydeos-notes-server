// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/config"
)

func TestServerConfigReadsPublicFileSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"server":{"name":"Notes staging"},"flags":{"allowGuestDemoWorkspace":false},"auth":{"passwordRequirements":{"min":12,"max":64}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(config.Config{RuntimeConfigPath: path}, noSessionAuthenticator{}, nil, nil)
	got := queryPublicServerConfig(t, h, `{serverConfig{name features credentialsRequirement{password{minLength maxLength}}}}`)
	want := map[string]any{"name": "Notes staging", "features": []any{}, "credentialsRequirement": map[string]any{"password": map[string]any{"minLength": float64(12), "maxLength": float64(64)}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("public server configuration = %#v, want %#v", got, want)
	}
}

func queryPublicServerConfig(t *testing.T, h http.Handler, query string) map[string]any {
	t.Helper()
	body, err := json.Marshal(map[string]string{"query": query})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := newDeadlineTestResponseWriter()
	h.ServeHTTP(w, r)
	var result struct {
		Data struct {
			ServerConfig map[string]any `json:"serverConfig"`
		} `json:"data"`
		Errors []any `json:"errors"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 || len(result.Errors) != 0 {
		t.Fatalf("server configuration query: status=%d body=%s error=%v", w.Code, w.Body.String(), err)
	}
	return result.Data.ServerConfig
}

type countedOAuthCatalog struct{ calls atomic.Int32 }

func (c *countedOAuthCatalog) AvailableProviders(context.Context) ([]string, error) {
	c.calls.Add(1)
	time.Sleep(time.Millisecond)
	return []string{"google"}, nil
}

func TestOAuthProviderAliasesShareOneOperationSnapshot(t *testing.T) {
	catalog := &countedOAuthCatalog{}
	h := NewHandlerWithServices(config.Config{Version: "0.27.5", Deployment: "selfhosted"}, noSessionAuthenticator{}, nil, nil, nil, nil, nil, nil, HandlerDependencies{OAuthProviders: catalog})
	fields := make([]string, 128)
	for i := range fields {
		fields[i] = "p" + strconv.Itoa(i) + ":oauthProviders"
	}
	got := queryPublicServerConfig(t, h, "{serverConfig{"+strings.Join(fields, " ")+"}}")
	if catalog.calls.Load() != 1 {
		t.Fatalf("provider catalog loads=%d for 128 aliases", catalog.calls.Load())
	}
	for _, field := range fields {
		name := strings.SplitN(field, ":", 2)[0]
		if !reflect.DeepEqual(got[name], []any{"Google"}) {
			t.Fatalf("%s=%#v", name, got[name])
		}
	}
}
