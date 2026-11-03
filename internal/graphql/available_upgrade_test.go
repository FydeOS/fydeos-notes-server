// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/upgrades"
)

type availableUpgradeCheckerFake struct {
	release   *upgrades.Release
	err       error
	called    bool
	namespace string
	version   string
}

func (f *availableUpgradeCheckerFake) Available(_ context.Context, namespace, version string) (*upgrades.Release, error) {
	f.called = true
	f.namespace = namespace
	f.version = version
	return f.release, f.err
}

func TestServerConfigAvailableUpgradeIsSelfHostedOnly(t *testing.T) {
	checker := &availableUpgradeCheckerFake{release: &upgrades.Release{Version: "0.27.6"}}
	handler := NewHandlerWithServices(
		config.Config{Version: "0.27.5", Deployment: "affine"},
		noSessionAuthenticator{}, nil, nil, nil, nil, nil, nil,
		HandlerDependencies{UpgradeChecker: checker},
	)
	response := executeServerConfigQuery(t, handler)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"availableUpgrade":null`) {
		t.Fatalf("response = %d %s, want null upgrade", response.Code, response.Body.String())
	}
	if checker.called {
		t.Fatal("cloud deployment called the upgrade checker")
	}
}

func TestServerConfigAvailableUpgradeReturnsReleaseFields(t *testing.T) {
	checker := &availableUpgradeCheckerFake{release: &upgrades.Release{
		Version:     "0.27.6",
		URL:         "https://example.org/releases/0.27.6",
		PublishedAt: time.Date(2026, time.October, 17, 4, 30, 0, 0, time.FixedZone("UTC+8", 8*60*60)),
		Changelog:   "Fixes and improvements",
	}}
	handler := NewHandlerWithServices(
		config.Config{Version: "0.27.5", Deployment: "selfhosted", Namespace: "beta"},
		noSessionAuthenticator{}, nil, nil, nil, nil, nil, nil,
		HandlerDependencies{UpgradeChecker: checker},
	)
	response := executeServerConfigQuery(t, handler)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, expected := range []string{
		`"version":"0.27.6"`,
		`"url":"https://example.org/releases/0.27.6"`,
		`"publishedAt":"2026-10-16T20:30:00.000Z"`,
		`"changelog":"Fixes and improvements"`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("response %s does not contain %s", body, expected)
		}
	}
	if !checker.called || checker.namespace != "beta" || checker.version != "0.27.5" {
		t.Fatalf("checker call = %+v", checker)
	}
}

func TestServerConfigAvailableUpgradeReturnsNilOnProviderFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusBadGateway)
	}))
	defer server.Close()

	handler := NewHandlerWithServices(
		config.Config{Version: "0.27.5", Deployment: "selfhosted"},
		noSessionAuthenticator{}, nil, nil, nil, nil, nil, nil,
		HandlerDependencies{UpgradeChecker: upgrades.NewChecker(server.Client(), server.URL)},
	)
	response := executeServerConfigQuery(t, handler)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"availableUpgrade":null`) {
		t.Fatalf("response = %d %s, want null upgrade on provider failure", response.Code, response.Body.String())
	}
}

func executeServerConfigQuery(t *testing.T, handler http.Handler) *deadlineTestResponseWriter {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(`{"query":"{ serverConfig { availableUpgrade { version url publishedAt changelog } } }"}`))
	request.Header.Set("Content-Type", "application/json")
	response := newDeadlineTestResponseWriter()
	handler.ServeHTTP(response, request)
	return response
}
