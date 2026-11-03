// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package staticfiles

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const mobileUserAgent = "Mozilla/5.0 (Linux; Android 6.0; Nexus 5) AppleWebKit/537.36 Chrome/129.0.0.0 Mobile Safari/537.36"

func TestStaticHandlerServesAssetsAndSPAForConfiguredSubpath(t *testing.T) {
	root := staticFixture(t)
	handler := NewHandler(HandlerConfig{Root: root, ServerPath: "notes/v1", Namespace: "production", Deployment: "affine"})

	for _, test := range []struct {
		method string
		path   string
		want   int
		body   string
		cache  string
	}{
		{method: http.MethodGet, path: "/notes/v1/assets/main.js", want: http.StatusOK, body: "web-asset"},
		{method: http.MethodGet, path: "/notes/v1/assets/missing.js", want: http.StatusNotFound},
		{method: http.MethodGet, path: "/notes/v1/workspace/doc-1", want: http.StatusOK, body: "web-index", cache: "private, no-cache, no-store, max-age=0, must-revalidate"},
		{method: http.MethodGet, path: "/notes/v1/admin/settings", want: http.StatusOK, body: "admin-index"},
		{method: http.MethodGet, path: "/notes/v1/index.html", want: http.StatusOK, body: "web-index"},
		{method: http.MethodPost, path: "/notes/v1/workspace/doc-1", want: http.StatusNotFound},
	} {
		t.Run(test.path, func(t *testing.T) {
			response := serve(handler, test.method, test.path, nil)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d; body=%q", response.Code, test.want, response.Body.String())
			}
			if test.body != "" && strings.TrimSpace(response.Body.String()) != test.body {
				t.Fatalf("body = %q, want %q", response.Body.String(), test.body)
			}
			if test.cache != "" && response.Header().Get("Cache-Control") != test.cache {
				t.Fatalf("Cache-Control = %q, want %q", response.Header().Get("Cache-Control"), test.cache)
			}
		})
	}
}

func TestStaticHandlerSelectsMobileFilesOnlyInDevNamespace(t *testing.T) {
	root := staticFixture(t)
	for _, test := range []struct {
		name      string
		namespace string
		path      string
		want      string
	}{
		{name: "mobile user agent in dev", namespace: "dev", path: "/assets/main.js", want: "mobile-asset"},
		{name: "mobile client hint in dev", namespace: "dev", path: "/workspace/doc", want: "mobile-index"},
		{name: "mobile user agent in production", namespace: "production", path: "/assets/main.js", want: "web-asset"},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := NewHandler(HandlerConfig{Root: root, Namespace: test.namespace, Deployment: "affine"})
			headers := make(http.Header)
			if strings.Contains(test.name, "user agent") {
				headers.Set("User-Agent", mobileUserAgent)
			}
			if strings.Contains(test.name, "client hint") {
				headers.Set("Sec-CH-UA-Mobile", "?1")
			}
			response := serve(handler, http.MethodGet, test.path, headers)
			if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != test.want {
				t.Fatalf("response = %d %q, want 200 %q", response.Code, response.Body.String(), test.want)
			}
		})
	}
}

func TestSelfhostStaticHandlerGatesPagesOnInitialization(t *testing.T) {
	root := staticFixture(t)
	initialized := false
	handler := NewHandler(HandlerConfig{
		Root: root, ServerPath: "/notes", Namespace: "production", Deployment: "selfhosted",
		Initialized: func(context.Context) (bool, error) { return initialized, nil },
	})

	for _, test := range []struct {
		path       string
		wantStatus int
		wantBody   string
		wantLoc    string
	}{
		{path: "/notes/", wantStatus: http.StatusFound, wantLoc: "/notes/admin/setup"},
		{path: "/notes/admin/setup", wantStatus: http.StatusOK, wantBody: "admin-selfhost-index"},
		{path: "/notes/admin/setup.js", wantStatus: http.StatusOK, wantBody: "admin-asset"},
	} {
		response := serve(handler, http.MethodGet, test.path, nil)
		if response.Code != test.wantStatus || response.Header().Get("Location") != test.wantLoc {
			t.Fatalf("GET %s = %d location %q, want %d location %q", test.path, response.Code, response.Header().Get("Location"), test.wantStatus, test.wantLoc)
		}
		if test.wantBody != "" && strings.TrimSpace(response.Body.String()) != test.wantBody {
			t.Fatalf("GET %s body = %q, want %q", test.path, response.Body.String(), test.wantBody)
		}
	}

	response := serve(handler, http.MethodGet, "/notes/healthz", nil)
	if response.Code != http.StatusFound || response.Header().Get("Location") != "/notes/admin/setup" {
		t.Fatalf("GET /notes/healthz = %d Location=%q, want setup redirect", response.Code, response.Header().Get("Location"))
	}
	if response.Header().Get("Content-Type") != "text/plain; charset=utf-8" || response.Body.String() != "Found. Redirecting to /notes/admin/setup" {
		t.Fatalf("setup redirect response = %q %q, want legacy plain text", response.Header().Get("Content-Type"), response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "" || response.Header().Get("Vary") != "Accept" {
		t.Fatalf("setup redirect headers = Cache-Control:%q Vary:%q", response.Header().Get("Cache-Control"), response.Header().Get("Vary"))
	}

	initialized = true
	response = serve(handler, http.MethodGet, "/notes/admin/setup", nil)
	if response.Code != http.StatusFound || response.Header().Get("Location") != "/notes/admin" {
		t.Fatalf("initialized setup route = %d location %q, want redirect to /notes/admin", response.Code, response.Header().Get("Location"))
	}
	response = serve(handler, http.MethodGet, "/notes/healthz", nil)
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != "selfhost-index" {
		t.Fatalf("initialized /healthz = %d %q, want selfhost index fallback", response.Code, response.Body.String())
	}
}

func TestStaticHandlerRejectsTraversalAndHiddenFiles(t *testing.T) {
	root := staticFixture(t)
	if err := os.WriteFile(filepath.Join(root, ".secret"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(HandlerConfig{Root: root, Namespace: "production", Deployment: "affine"})
	for _, path := range []string{"/%2e%2e/secret", "/../secret", "/.secret"} {
		response := serve(handler, http.MethodGet, path, nil)
		if response.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, response.Code)
		}
	}
}

func TestSelfhostSetupGateContinuesWhenInitializationCheckFails(t *testing.T) {
	handler := NewHandler(HandlerConfig{
		Root: t.TempDir(), Namespace: "production", Deployment: "selfhosted",
		Initialized: func(context.Context) (bool, error) { return false, os.ErrPermission },
	})
	response := serve(handler, http.MethodGet, "/", nil)
	if response.Code != http.StatusNotFound {
		t.Fatalf("failed initialization check status = %d, want static 404", response.Code)
	}
}

func staticFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"index.html":               "web-index",
		"selfhost.html":            "selfhost-index",
		"admin/index.html":         "admin-index",
		"admin/selfhost.html":      "admin-selfhost-index",
		"admin/setup.js":           "admin-asset",
		"assets/main.js":           "web-asset",
		"mobile/index.html":        "mobile-index",
		"mobile/selfhost.html":     "mobile-selfhost-index",
		"mobile/assets/main.js":    "mobile-asset",
		"_plugin/bundle/plugin.js": "plugin-asset",
	}
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func serve(handler http.Handler, method, target string, headers http.Header) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, nil)
	request.Header = headers
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
