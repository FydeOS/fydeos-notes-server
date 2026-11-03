// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fydeos/fydeos-notes-server/internal/config"
)

func TestInfoMatchesExistingPublicContract(t *testing.T) {
	handler := NewHandler(config.Config{
		Version:    "0.27.5",
		Deployment: "selfhosted",
		Flavor:     "graphql",
	})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/info", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	var got Info
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := Info{
		Compatibility: "0.27.5",
		Message:       "FydeOS Notes 0.27.5 Server",
		Type:          "selfhosted",
		Flavor:        "graphql",
	}
	if got != want {
		t.Fatalf("info = %+v, want %+v", got, want)
	}
}

func TestInfoHonorsConfiguredSubpath(t *testing.T) {
	handler := NewHandler(config.Config{Version: "dev", Deployment: "affine", Flavor: "front", Path: "notes/v1"})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/notes/v1/info", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
}

func TestInfoRejectsWrongMethodAndUnknownPaths(t *testing.T) {
	handler := NewHandler(config.Config{Version: "dev", Deployment: "selfhosted", Flavor: "allinone"})
	for _, test := range []struct {
		method string
		path   string
		want   int
	}{
		{method: http.MethodPost, path: "/info", want: http.StatusMethodNotAllowed},
		{method: http.MethodGet, path: "/unknown", want: http.StatusNotFound},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(test.method, test.path, nil))
		if response.Code != test.want {
			t.Errorf("%s %s = %d, want %d", test.method, test.path, response.Code, test.want)
		}
	}
}

func TestPublicRootDocumentRouteIsDistinctFromDocumentNamedRootDoc(t *testing.T) {
	config := config.Config{Version: "dev", Deployment: "selfhosted", Flavor: "graphql"}
	handler := NewApplicationHandler(config, nil, ApplicationHandlers{
		WorkspaceDocuments: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("document:" + r.PathValue("docId")))
		}),
		WorkspacePublicRootDoc: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("root:" + r.PathValue("docId")))
		}),
	})

	for _, test := range []struct {
		path string
		want string
	}{
		{path: "/api/workspaces/space/public-docs/root-doc", want: "document:root-doc"},
		{path: "/api/workspaces/space/public-docs/public/root-doc", want: "root:public"},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
		if response.Code != http.StatusOK || response.Body.String() != test.want {
			t.Errorf("GET %s = %d %q, want 200 %q", test.path, response.Code, response.Body.String(), test.want)
		}
	}
}

func TestCopilotChatRouteIsMountedForBackendFlavors(t *testing.T) {
	for _, flavor := range []string{"allinone", "graphql", "sync"} {
		t.Run(flavor, func(t *testing.T) {
			handler := NewApplicationHandler(config.Config{Flavor: flavor, Path: "notes/v1"}, nil, ApplicationHandlers{
				Copilot: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = w.Write([]byte(r.PathValue("sessionId") + "/" + r.PathValue("endpoint")))
				}),
			})
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/notes/v1/api/copilot/chat/session-1/stream-object", nil))
			if response.Code != http.StatusOK || response.Body.String() != "session-1/stream-object" {
				t.Fatalf("Copilot route = %d %q", response.Code, response.Body.String())
			}
		})
	}
}

func TestCopilotActionRouteIsMountedForBackendFlavors(t *testing.T) {
	for _, flavor := range []string{"allinone", "graphql", "sync"} {
		t.Run(flavor, func(t *testing.T) {
			handler := NewApplicationHandler(config.Config{Flavor: flavor, Path: "notes/v1"}, nil, ApplicationHandlers{
				CopilotActions: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = w.Write([]byte(r.PathValue("sessionId")))
				}),
			})
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/notes/v1/api/copilot/actions/session-1/stream", nil))
			if response.Code != http.StatusOK || response.Body.String() != "session-1" {
				t.Fatalf("Copilot action route = %d %q", response.Code, response.Body.String())
			}
		})
	}
}

func TestCalendarWebhookRouteIsMountedForBackendFlavors(t *testing.T) {
	for _, flavor := range []string{"allinone", "graphql", "sync", "front"} {
		t.Run(flavor, func(t *testing.T) {
			called := false
			handler := NewApplicationHandler(config.Config{Flavor: flavor}, nil, ApplicationHandlers{
				CalendarWebhook: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					called = true
					w.WriteHeader(http.StatusNoContent)
				}),
			})
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/calendar/webhook/google", nil))
			if response.Code != http.StatusNoContent || !called {
				t.Fatalf("webhook status=%d called=%t", response.Code, called)
			}
		})
	}
}

func TestCopilotAttachmentRouteIsMountedForBackendFlavors(t *testing.T) {
	for _, flavor := range []string{"allinone", "graphql", "sync"} {
		t.Run(flavor, func(t *testing.T) {
			handler := NewApplicationHandler(config.Config{Flavor: flavor, Path: "notes/v1"}, nil, ApplicationHandlers{
				CopilotAttachments: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = w.Write([]byte(r.PathValue("sessionId") + "/" + r.PathValue("key")))
				}),
			})
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/notes/v1/api/copilot/chat/session-1/attachments/hash-key", nil))
			if response.Code != http.StatusOK || response.Body.String() != "session-1/hash-key" {
				t.Fatalf("Copilot attachment route = %d %q", response.Code, response.Body.String())
			}
		})
	}
}

func TestReadyzUsesDependencyReadiness(t *testing.T) {
	cfg := config.Config{Version: "dev", Deployment: "selfhosted", Flavor: "allinone"}
	for _, test := range []struct {
		name       string
		check      ReadinessCheck
		wantStatus int
		wantBody   string
	}{
		{name: "ready", check: func(context.Context) error { return nil }, wantStatus: http.StatusOK, wantBody: `{"status":"ready"}`},
		{name: "dependency unavailable", check: func(context.Context) error { return errors.New("database unavailable") }, wantStatus: http.StatusServiceUnavailable, wantBody: `{"status":"not_ready"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := NewHandlerWithReadiness(cfg, test.check)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
			if got := strings.TrimSpace(response.Body.String()); got != test.wantBody {
				t.Fatalf("body = %q, want %q", got, test.wantBody)
			}
		})
	}
}

func TestReadyzHonorsConfiguredSubpath(t *testing.T) {
	handler := NewHandlerWithReadiness(
		config.Config{Version: "dev", Deployment: "selfhosted", Flavor: "allinone", Path: "notes/v1"},
		func(context.Context) error { return nil },
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/notes/v1/readyz", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
}

func TestGraphQLMountsAtConfiguredSubpath(t *testing.T) {
	graphQL := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	handler := NewApplicationHandler(
		config.Config{Version: "dev", Deployment: "selfhosted", Flavor: "graphql", Path: "notes/v1"},
		nil,
		ApplicationHandlers{GraphQL: graphQL},
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/notes/v1/graphql", nil))
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusAccepted)
	}
}

func TestHealthzFallsThroughToFrontendWhileReadyzChecksReadiness(t *testing.T) {
	frontend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			http.Redirect(w, r, "/admin/setup", http.StatusFound)
			return
		}
		http.NotFound(w, r)
	})
	handler := NewApplicationHandler(
		config.Config{Version: "dev", Deployment: "selfhosted", Flavor: "graphql"},
		func(context.Context) error { return errors.New("database unavailable") },
		ApplicationHandlers{Frontend: frontend},
	)

	healthResponse := httptest.NewRecorder()
	handler.ServeHTTP(healthResponse, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if healthResponse.Code != http.StatusFound || healthResponse.Header().Get("Location") != "/admin/setup" {
		t.Fatalf("GET /healthz = %d Location=%q, want frontend redirect", healthResponse.Code, healthResponse.Header().Get("Location"))
	}

	readyResponse := httptest.NewRecorder()
	handler.ServeHTTP(readyResponse, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if readyResponse.Code != http.StatusServiceUnavailable || readyResponse.Body.String() != "{\"status\":\"not_ready\"}\n" {
		t.Fatalf("GET /readyz = %d %q, want 503 not_ready", readyResponse.Code, readyResponse.Body.String())
	}
}

func TestBlobManifestRoutesMountAtConfiguredSubpath(t *testing.T) {
	manifest := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(r.PathValue("id")))
	})
	handler := NewApplicationHandler(
		config.Config{Version: "dev", Deployment: "selfhosted", Flavor: "graphql", Path: "notes/v1"},
		nil,
		ApplicationHandlers{BlobManifests: manifest},
	)
	for _, path := range []string{
		"/notes/v1/api/workspaces/space/blob-manifest/v1?sourceType=currentDoc&docId=doc",
		"/notes/v1/api/workspaces/space/readable-blob-manifest/v1",
		"/notes/v1/api/workspaces/space/blobs/v1/blob-key?sourceType=currentDoc&docId=doc",
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusAccepted || response.Body.String() != "space" {
			t.Errorf("GET %s = %d %q, want 202 %q", path, response.Code, response.Body.String(), "space")
		}
	}
}

func TestMCPRouteMountsAtWorkspaceEndpointAndOnlyInGraphRoles(t *testing.T) {
	mcp := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(r.PathValue("id")))
	})
	for _, flavor := range []string{"graphql", "allinone"} {
		t.Run(flavor, func(t *testing.T) {
			handler := NewApplicationHandler(
				config.Config{Version: "dev", Deployment: "selfhosted", Flavor: flavor, Path: "notes/v1"},
				nil,
				ApplicationHandlers{MCP: mcp},
			)
			for _, path := range []string{
				"/notes/v1/api/workspaces/space/mcp",
				"/notes/v1/api/workspaces/space/mcp/",
			} {
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
				if response.Code != http.StatusAccepted || response.Body.String() != "space" {
					t.Errorf("GET %s = %d %q, want 202 %q", path, response.Code, response.Body.String(), "space")
				}
			}
		})
	}

	for _, flavor := range []string{"sync", "front", "renderer"} {
		t.Run(flavor+" excludes MCP", func(t *testing.T) {
			handler := NewApplicationHandler(
				config.Config{Version: "dev", Deployment: "selfhosted", Flavor: flavor},
				nil,
				ApplicationHandlers{MCP: mcp},
			)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/workspaces/space/mcp", nil))
			if response.Code != http.StatusNotFound {
				t.Fatalf("POST MCP route = %d, want 404", response.Code)
			}
		})
	}
}

func TestAuthSessionMountsAtConfiguredSubpath(t *testing.T) {
	authSession := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	handler := NewApplicationHandler(
		config.Config{Version: "dev", Deployment: "selfhosted", Flavor: "graphql", Path: "notes/v1"},
		nil,
		ApplicationHandlers{AuthSession: authSession, AuthMethods: authSession},
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/notes/v1/api/auth/session", nil))
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusAccepted)
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/notes/v1/api/auth/methods", nil))
	if response.Code != http.StatusAccepted {
		t.Fatalf("methods status = %d, want %d", response.Code, http.StatusAccepted)
	}
}

func TestAuthSignInMountsAtConfiguredSubpath(t *testing.T) {
	signIn := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	for _, flavor := range []string{"graphql", "allinone", "sync", "front"} {
		t.Run(flavor, func(t *testing.T) {
			handler := NewApplicationHandler(
				config.Config{Version: "dev", Deployment: "selfhosted", Flavor: flavor, Path: "notes/v1"},
				nil,
				ApplicationHandlers{AuthSignIn: signIn},
			)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/notes/v1/api/auth/sign-in", nil))
			if response.Code != http.StatusAccepted {
				t.Fatalf("sign-in status = %d, want %d", response.Code, http.StatusAccepted)
			}
		})
	}
}

func TestAuthCaptchaMountsAtConfiguredSubpath(t *testing.T) {
	captcha := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	for _, flavor := range []string{"graphql", "allinone", "sync", "front"} {
		t.Run(flavor, func(t *testing.T) {
			handler := NewApplicationHandler(
				config.Config{Version: "dev", Deployment: "selfhosted", Flavor: flavor, Path: "notes/v1"},
				nil,
				ApplicationHandlers{AuthCaptcha: captcha},
			)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/notes/v1/api/auth/captcha", nil))
			if response.Code != http.StatusAccepted {
				t.Fatalf("captcha status = %d, want %d", response.Code, http.StatusAccepted)
			}
		})
	}
}

func TestAuthOpenAppRoutesMountAtConfiguredSubpath(t *testing.T) {
	openApp := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	for _, flavor := range []string{"graphql", "allinone", "sync", "front"} {
		t.Run(flavor, func(t *testing.T) {
			handler := NewApplicationHandler(
				config.Config{Version: "dev", Deployment: "selfhosted", Flavor: flavor, Path: "notes/v1"},
				nil,
				ApplicationHandlers{AuthOpenApp: openApp},
			)
			for _, path := range []string{
				"/notes/v1/api/auth/open-app/sign-in-code",
				"/notes/v1/api/auth/open-app/sign-in",
			} {
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, nil))
				if response.Code != http.StatusAccepted {
					t.Errorf("POST %s status = %d, want %d", path, response.Code, http.StatusAccepted)
				}
			}
		})
	}
}

func TestAuthMagicLinkMountsAtConfiguredSubpath(t *testing.T) {
	magicLink := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	for _, flavor := range []string{"graphql", "allinone", "sync", "front"} {
		t.Run(flavor, func(t *testing.T) {
			handler := NewApplicationHandler(
				config.Config{Version: "dev", Deployment: "selfhosted", Flavor: flavor, Path: "notes/v1"},
				nil,
				ApplicationHandlers{AuthMagicLink: magicLink},
			)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/notes/v1/api/auth/magic-link", nil))
			if response.Code != http.StatusAccepted {
				t.Fatalf("magic-link status = %d, want %d", response.Code, http.StatusAccepted)
			}
		})
	}
}

func TestAuthPreflightMountsSeparatelyFromOAuthAtConfiguredSubpath(t *testing.T) {
	for _, flavor := range []string{"graphql", "allinone", "sync", "front", "worker"} {
		t.Run(flavor, func(t *testing.T) {
			handler := NewApplicationHandler(config.Config{Flavor: flavor, Path: "notes/v1"}, nil, ApplicationHandlers{
				AuthPreflight:  http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusCreated) }),
				OAuthPreflight: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) }),
			})
			for path, want := range map[string]int{"/notes/v1/api/auth/preflight": 201, "/notes/v1/api/oauth/preflight": 202, "/api/auth/preflight": 404} {
				if flavor == "worker" || ((flavor == "sync" || flavor == "front") && strings.Contains(path, "/oauth/")) {
					want = 404
				}
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, nil))
				if response.Code != want {
					t.Errorf("%s = %d want %d", path, response.Code, want)
				}
			}
		})
	}
}

func TestAuthSessionManagementMountsAtConfiguredSubpath(t *testing.T) {
	authSessions := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	handler := NewApplicationHandler(
		config.Config{Version: "dev", Deployment: "selfhosted", Flavor: "graphql", Path: "notes/v1"},
		nil,
		ApplicationHandlers{AuthSessions: authSessions},
	)
	for _, request := range []struct{ method, path string }{
		{http.MethodGet, "/notes/v1/api/auth/sessions"},
		{http.MethodPost, "/notes/v1/api/auth/sessions/revoke-all"},
		{http.MethodDelete, "/notes/v1/api/auth/sessions/00000000-0000-4000-8000-000000000001"},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(request.method, request.path, nil))
		if response.Code != http.StatusAccepted {
			t.Errorf("%s %s = %d, want %d", request.method, request.path, response.Code, http.StatusAccepted)
		}
	}
}

func TestAuthSignOutMountsAtConfiguredSubpath(t *testing.T) {
	signOut := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	handler := NewApplicationHandler(
		config.Config{Version: "dev", Deployment: "selfhosted", Flavor: "graphql", Path: "notes/v1"},
		nil,
		ApplicationHandlers{AuthSignOut: signOut},
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/notes/v1/api/auth/sign-out", nil))
	if response.Code != http.StatusAccepted {
		t.Fatalf("sign-out status = %d, want %d", response.Code, http.StatusAccepted)
	}
}

func TestOAuthPreflightMountsAtConfiguredSubpath(t *testing.T) {
	preflight := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	handler := NewApplicationHandler(
		config.Config{Version: "dev", Deployment: "selfhosted", Flavor: "graphql", Path: "notes/v1"},
		nil,
		ApplicationHandlers{OAuthPreflight: preflight},
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/notes/v1/api/oauth/preflight", nil))
	if response.Code != http.StatusAccepted {
		t.Fatalf("preflight status = %d, want %d", response.Code, http.StatusAccepted)
	}
}

func TestOAuthCallbackMountsAtBothProviderPaths(t *testing.T) {
	callback := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	handler := NewApplicationHandler(
		config.Config{Version: "dev", Deployment: "selfhosted", Flavor: "graphql", Path: "notes/v1"},
		nil,
		ApplicationHandlers{OAuthCallback: callback},
	)
	for _, path := range []string{"/notes/v1/oauth/callback", "/notes/v1/api/oauth/callback"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, nil))
		if response.Code != http.StatusAccepted {
			t.Errorf("POST %s = %d, want %d", path, response.Code, http.StatusAccepted)
		}
	}
}

func TestGraphQLFlavorMountsAPIAndRealtimeButNoUnrelatedHandlers(t *testing.T) {
	accepted := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	handler := NewApplicationHandler(
		config.Config{Version: "dev", Deployment: "selfhosted", Flavor: "graphql"},
		nil,
		ApplicationHandlers{
			GraphQL: accepted, AuthSession: accepted, OAuthCallback: accepted, SocketIO: accepted,
			StorageProxy: accepted, Frontend: accepted,
		},
	)
	for _, request := range []struct{ method, path string }{
		{http.MethodPost, "/graphql"},
		{http.MethodGet, "/api/auth/session"},
		{http.MethodPost, "/api/oauth/callback"},
		{http.MethodGet, "/socket.io"},
		{http.MethodPut, "/api/storage/upload"},
		{http.MethodPut, "/api/storage/multipart"},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(request.method, request.path, nil))
		if response.Code != http.StatusAccepted {
			t.Errorf("%s %s = %d, want %d", request.method, request.path, response.Code, http.StatusAccepted)
		}
	}
}

func TestSyncFlavorMountsAuthAndRealtimeWithoutGraphQLOrOAuthPlugin(t *testing.T) {
	accepted := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	handler := NewApplicationHandler(
		config.Config{Version: "dev", Deployment: "selfhosted", Flavor: "sync"},
		nil,
		ApplicationHandlers{
			GraphQL: accepted, AuthSession: accepted, AuthMethods: accepted,
			AuthSessions: accepted, AuthSignOut: accepted, OAuthPreflight: accepted, SocketIO: accepted,
			StorageProxy: accepted,
		},
	)
	for _, request := range []struct {
		method string
		path   string
		want   int
	}{
		{http.MethodGet, "/info", http.StatusOK},
		{http.MethodGet, "/healthz", http.StatusNotFound},
		{http.MethodGet, "/readyz", http.StatusOK},
		{http.MethodPost, "/graphql", http.StatusNotFound},
		{http.MethodGet, "/api/auth/session", http.StatusAccepted},
		{http.MethodGet, "/api/auth/methods", http.StatusAccepted},
		{http.MethodGet, "/api/auth/sessions", http.StatusAccepted},
		{http.MethodPost, "/api/auth/sign-out", http.StatusAccepted},
		{http.MethodPost, "/api/oauth/preflight", http.StatusNotFound},
		{http.MethodGet, "/socket.io", http.StatusAccepted},
		{http.MethodPut, "/api/storage/upload", http.StatusNotFound},
		{http.MethodGet, "/", http.StatusNotFound},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(request.method, request.path, nil))
		if response.Code != request.want {
			t.Errorf("%s %s = %d, want %d", request.method, request.path, response.Code, request.want)
		}
	}
}

func TestFrontFlavorMountsStaticAuthAndRealtimeOnly(t *testing.T) {
	accepted := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	frontend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("static:" + r.URL.Path))
	})
	handler := NewApplicationHandler(
		config.Config{Version: "dev", Deployment: "selfhosted", Flavor: "front", Path: "notes/v1"},
		nil,
		ApplicationHandlers{
			GraphQL: accepted, AuthSession: accepted, OAuthCallback: accepted,
			SocketIO: accepted, Frontend: frontend,
		},
	)
	for _, request := range []struct {
		method string
		path   string
		want   int
		body   string
	}{
		{http.MethodGet, "/notes/v1/workspace/doc", http.StatusOK, "static:/notes/v1/workspace/doc"},
		{http.MethodGet, "/notes/v1/api/auth/session", http.StatusAccepted, ""},
		{http.MethodGet, "/socket.io", http.StatusAccepted, ""},
		{http.MethodPost, "/notes/v1/graphql", http.StatusNotFound, ""},
		{http.MethodPost, "/notes/v1/oauth/callback", http.StatusNotFound, ""},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(request.method, request.path, nil))
		if response.Code != request.want {
			t.Errorf("%s %s = %d, want %d", request.method, request.path, response.Code, request.want)
		}
		if request.body != "" && response.Body.String() != request.body {
			t.Errorf("body = %q, want %q", response.Body.String(), request.body)
		}
	}
}

func TestRendererFlavorMountsWorkspaceRendererAndFrontendWithoutAPIHandlers(t *testing.T) {
	accepted := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	frontend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	handler := NewApplicationHandler(
		config.Config{Version: "dev", Deployment: "selfhosted", Flavor: "renderer", Path: "notes/v1"},
		nil,
		ApplicationHandlers{GraphQL: accepted, Renderer: accepted, Frontend: frontend},
	)
	for _, request := range []struct {
		method string
		path   string
		want   int
	}{
		{http.MethodGet, "/notes/v1/workspace/space/doc", http.StatusAccepted},
		{http.MethodPost, "/notes/v1/graphql", http.StatusNotFound},
		{http.MethodGet, "/notes/v1/", http.StatusAccepted},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(request.method, request.path, nil))
		if response.Code != request.want {
			t.Errorf("%s %s = %d, want %d", request.method, request.path, response.Code, request.want)
		}
	}
}

func TestNativeSessionEndpointsMountForSyncAndFrontFlavors(t *testing.T) {
	accepted := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	for _, flavor := range []string{"sync", "front"} {
		handler := NewApplicationHandler(
			config.Config{Version: "dev", Deployment: "selfhosted", Flavor: flavor},
			nil,
			ApplicationHandlers{NativeAuthSession: accepted},
		)
		for _, path := range []string{"exchange", "refresh", "revoke"} {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/auth/session/"+path, nil))
			if response.Code != http.StatusAccepted {
				t.Fatalf("%s native session %s status = %d, want %d", flavor, path, response.Code, http.StatusAccepted)
			}
		}
	}
}

func TestCommentAttachmentReadMountsOnlyOnGraphQLServerFlavors(t *testing.T) {
	accepted := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	for _, flavor := range []string{"allinone", "graphql"} {
		handler := NewApplicationHandler(
			config.Config{Version: "dev", Deployment: "selfhosted", Flavor: flavor, Path: "notes/v1"},
			nil,
			ApplicationHandlers{CommentAttachments: accepted},
		)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/notes/v1/api/workspaces/workspace-1/docs/doc-1/comment-attachments/file-1", nil))
		if response.Code != http.StatusAccepted {
			t.Errorf("%s comment attachment route = %d, want %d", flavor, response.Code, http.StatusAccepted)
		}
	}
	for _, flavor := range []string{"sync", "front"} {
		handler := NewApplicationHandler(
			config.Config{Version: "dev", Deployment: "selfhosted", Flavor: flavor},
			nil,
			ApplicationHandlers{CommentAttachments: accepted},
		)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/workspaces/workspace-1/docs/doc-1/comment-attachments/file-1", nil))
		if response.Code != http.StatusNotFound {
			t.Errorf("%s comment attachment route = %d, want 404", flavor, response.Code)
		}
	}
}

func TestAvatarReadMountsOnlyOnGraphQLServerFlavors(t *testing.T) {
	accepted := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	for _, flavor := range []string{"allinone", "graphql"} {
		handler := NewApplicationHandler(
			config.Config{Version: "dev", Deployment: "selfhosted", Flavor: flavor, Path: "notes/v1"},
			nil,
			ApplicationHandlers{Avatars: accepted},
		)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/notes/v1/api/avatars/avatar-1", nil))
		if response.Code != http.StatusAccepted {
			t.Errorf("%s avatar route = %d, want %d", flavor, response.Code, http.StatusAccepted)
		}
	}
	for _, flavor := range []string{"sync", "front"} {
		handler := NewApplicationHandler(
			config.Config{Version: "dev", Deployment: "selfhosted", Flavor: flavor},
			nil,
			ApplicationHandlers{Avatars: accepted},
		)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/avatars/avatar-1", nil))
		if response.Code != http.StatusNotFound {
			t.Errorf("%s avatar route = %d, want 404", flavor, response.Code)
		}
	}
}
