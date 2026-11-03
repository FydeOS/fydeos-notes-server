// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/cors"
)

type Info struct {
	Compatibility string `json:"compatibility"`
	Message       string `json:"message"`
	Type          string `json:"type"`
	Flavor        string `json:"flavor"`
}

type ReadinessCheck func(context.Context) error

type ApplicationHandlers struct {
	GraphQL                http.Handler
	AuthSession            http.Handler
	AuthPreflight          http.Handler
	AuthSignIn             http.Handler
	AuthCaptcha            http.Handler
	AuthOpenApp            http.Handler
	AuthMagicLink          http.Handler
	NativeAuthSession      http.Handler
	AuthMethods            http.Handler
	AuthSessions           http.Handler
	AuthSignOut            http.Handler
	OAuthPreflight         http.Handler
	OAuthCallback          http.Handler
	CalendarOAuth          http.Handler
	CalendarWebhook        http.Handler
	Copilot                http.Handler
	CopilotActions         http.Handler
	CopilotAttachments     http.Handler
	SocketIO               http.Handler
	StorageProxy           http.Handler
	CommentAttachments     http.Handler
	BlobManifests          http.Handler
	MCP                    http.Handler
	WorkspaceDocuments     http.Handler
	WorkspacePublicRootDoc http.Handler
	Avatars                http.Handler
	Frontend               http.Handler
	Renderer               http.Handler
	// Setup creates the first administrator of a new self-hosted server.
	Setup http.Handler
}

func NewHandler(cfg config.Config) http.Handler {
	return NewApplicationHandler(cfg, nil, ApplicationHandlers{})
}

func NewHandlerWithReadiness(cfg config.Config, readiness ReadinessCheck) http.Handler {
	return NewApplicationHandler(cfg, readiness, ApplicationHandlers{})
}

func NewApplicationHandler(cfg config.Config, readiness ReadinessCheck, handlers ApplicationHandlers) http.Handler {
	switch cfg.Flavor {
	case "", "allinone", "graphql":
		// These roles retain self-hosted setup/static fallbacks when configured.
	case "sync":
		handlers = ApplicationHandlers{
			AuthSession:        handlers.AuthSession,
			AuthPreflight:      handlers.AuthPreflight,
			AuthSignIn:         handlers.AuthSignIn,
			AuthCaptcha:        handlers.AuthCaptcha,
			AuthOpenApp:        handlers.AuthOpenApp,
			AuthMagicLink:      handlers.AuthMagicLink,
			NativeAuthSession:  handlers.NativeAuthSession,
			AuthMethods:        handlers.AuthMethods,
			AuthSessions:       handlers.AuthSessions,
			AuthSignOut:        handlers.AuthSignOut,
			CalendarOAuth:      handlers.CalendarOAuth,
			CalendarWebhook:    handlers.CalendarWebhook,
			Copilot:            handlers.Copilot,
			CopilotActions:     handlers.CopilotActions,
			CopilotAttachments: handlers.CopilotAttachments,
			SocketIO:           handlers.SocketIO,
			Frontend:           handlers.Frontend,
		}
	case "front":
		handlers = ApplicationHandlers{
			AuthSession:       handlers.AuthSession,
			AuthPreflight:     handlers.AuthPreflight,
			AuthSignIn:        handlers.AuthSignIn,
			AuthCaptcha:       handlers.AuthCaptcha,
			AuthOpenApp:       handlers.AuthOpenApp,
			AuthMagicLink:     handlers.AuthMagicLink,
			NativeAuthSession: handlers.NativeAuthSession,
			AuthMethods:       handlers.AuthMethods,
			AuthSessions:      handlers.AuthSessions,
			AuthSignOut:       handlers.AuthSignOut,
			CalendarWebhook:   handlers.CalendarWebhook,
			SocketIO:          handlers.SocketIO,
			Frontend:          handlers.Frontend,
			Setup:             handlers.Setup,
		}
	case "renderer":
		handlers = ApplicationHandlers{Frontend: handlers.Frontend, Renderer: handlers.Renderer}
	default:
		handlers = ApplicationHandlers{}
	}

	mux := http.NewServeMux()
	base := ""
	if cfg.Path != "" {
		base = "/" + strings.Trim(cfg.Path, "/")
	}

	mux.HandleFunc("GET "+base+"/info", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, Info{
			Compatibility: cfg.Version,
			Message:       "FydeOS Notes " + cfg.Version + " Server",
			Type:          cfg.Deployment,
			Flavor:        cfg.Flavor,
		})
	})
	mux.HandleFunc("GET "+base+"/readyz", func(w http.ResponseWriter, r *http.Request) {
		if readiness != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			if err := readiness(ctx); err != nil {
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready"})
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	if handlers.GraphQL != nil {
		mux.Handle(base+"/graphql", handlers.GraphQL)
		mux.Handle(base+"/graphql/", handlers.GraphQL)
	}
	if handlers.AuthSession != nil {
		mux.Handle("GET "+base+"/api/auth/session", handlers.AuthSession)
	}
	if handlers.AuthSignIn != nil {
		mux.Handle("POST "+base+"/api/auth/sign-in", handlers.AuthSignIn)
	}
	if handlers.AuthPreflight != nil {
		mux.Handle("POST "+base+"/api/auth/preflight", handlers.AuthPreflight)
	}
	if handlers.AuthCaptcha != nil {
		mux.Handle("GET "+base+"/api/auth/captcha", handlers.AuthCaptcha)
	}
	if handlers.AuthOpenApp != nil {
		mux.Handle("POST "+base+"/api/auth/open-app/sign-in-code", handlers.AuthOpenApp)
		mux.Handle("POST "+base+"/api/auth/open-app/sign-in", handlers.AuthOpenApp)
	}
	if handlers.AuthMagicLink != nil {
		mux.Handle("POST "+base+"/api/auth/magic-link", handlers.AuthMagicLink)
	}
	if handlers.NativeAuthSession != nil {
		mux.Handle("POST "+base+"/api/auth/session/exchange", handlers.NativeAuthSession)
		mux.Handle("POST "+base+"/api/auth/session/refresh", handlers.NativeAuthSession)
		mux.Handle("POST "+base+"/api/auth/session/revoke", handlers.NativeAuthSession)
	}
	if handlers.AuthMethods != nil {
		mux.Handle("GET "+base+"/api/auth/methods", handlers.AuthMethods)
	}
	if handlers.Setup != nil {
		mux.Handle("POST "+base+"/api/setup/create-admin-user", handlers.Setup)
	}
	if handlers.AuthSessions != nil {
		mux.Handle("GET "+base+"/api/auth/sessions", handlers.AuthSessions)
		mux.Handle("POST "+base+"/api/auth/sessions/revoke-all", handlers.AuthSessions)
		mux.Handle("DELETE "+base+"/api/auth/sessions/{id}", handlers.AuthSessions)
	}
	if handlers.AuthSignOut != nil {
		mux.Handle("POST "+base+"/api/auth/sign-out", handlers.AuthSignOut)
	}
	if handlers.OAuthPreflight != nil {
		mux.Handle("POST "+base+"/api/oauth/preflight", handlers.OAuthPreflight)
	}
	if handlers.OAuthCallback != nil {
		mux.Handle("POST "+base+"/oauth/callback", handlers.OAuthCallback)
		mux.Handle("POST "+base+"/api/oauth/callback", handlers.OAuthCallback)
	}
	if handlers.CalendarOAuth != nil {
		mux.Handle("GET "+base+"/api/calendar/oauth/callback", handlers.CalendarOAuth)
		mux.Handle("POST "+base+"/api/calendar/oauth/callback", handlers.CalendarOAuth)
	}
	if handlers.CalendarWebhook != nil {
		mux.Handle("POST "+base+"/api/calendar/webhook/google", handlers.CalendarWebhook)
	}
	if handlers.Copilot != nil {
		mux.Handle("GET "+base+"/api/copilot/chat/{sessionId}/{endpoint}", handlers.Copilot)
	}
	if handlers.CopilotActions != nil {
		mux.Handle("GET "+base+"/api/copilot/actions/{sessionId}/stream", handlers.CopilotActions)
	}
	if handlers.CopilotAttachments != nil {
		mux.Handle(base+"/api/copilot/chat/{sessionId}/attachments/{key}", handlers.CopilotAttachments)
	}
	if handlers.SocketIO != nil {
		mux.Handle("/socket.io", handlers.SocketIO)
		mux.Handle("/socket.io/", handlers.SocketIO)
	}
	if handlers.StorageProxy != nil {
		proxy := http.StripPrefix(base, handlers.StorageProxy)
		mux.Handle("PUT "+base+"/api/storage/upload", proxy)
		mux.Handle("PUT "+base+"/api/storage/multipart", proxy)
	}
	if handlers.CommentAttachments != nil {
		mux.Handle("GET "+base+"/api/workspaces/{id}/docs/{docId}/comment-attachments/{key}", handlers.CommentAttachments)
	}
	if handlers.BlobManifests != nil {
		mux.Handle("GET "+base+"/api/workspaces/{id}/blob-manifest/v1", handlers.BlobManifests)
		mux.Handle("GET "+base+"/api/workspaces/{id}/readable-blob-manifest/v1", handlers.BlobManifests)
		mux.Handle("GET "+base+"/api/workspaces/{id}/blobs/v1/{name}", handlers.BlobManifests)
	}
	if handlers.MCP != nil {
		mux.Handle(base+"/api/workspaces/{id}/mcp", handlers.MCP)
		mux.Handle(base+"/api/workspaces/{id}/mcp/", handlers.MCP)
	}
	if handlers.WorkspaceDocuments != nil {
		mux.Handle("GET "+base+"/api/workspaces/{id}/docs/{docId}", handlers.WorkspaceDocuments)
		mux.Handle("GET "+base+"/api/workspaces/{id}/public-docs/{docId}", handlers.WorkspaceDocuments)
		mux.Handle("HEAD "+base+"/api/workspaces/{id}/public-docs/{docId}", handlers.WorkspaceDocuments)
	}
	if handlers.WorkspacePublicRootDoc != nil {
		mux.Handle("GET "+base+"/api/workspaces/{id}/public-docs/{docId}/root-doc", handlers.WorkspacePublicRootDoc)
	}
	if handlers.Avatars != nil {
		mux.Handle("GET "+base+"/api/avatars/{id}", handlers.Avatars)
	}
	if handlers.Frontend != nil {
		if base != "" {
			mux.Handle(base, handlers.Frontend)
		}
		mux.Handle(base+"/", handlers.Frontend)
	}
	if handlers.Renderer != nil {
		mux.Handle("GET "+base+"/workspace/", handlers.Renderer)
	}

	handler := cors.HTTP(cfg, mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if handlers.SocketIO != nil && (r.URL.Path == "/socket.io" || strings.HasPrefix(r.URL.Path, "/socket.io/")) {
			// Engine.IO owns its CORS response contract and handles upgrades directly.
			mux.ServeHTTP(w, r)
			return
		}
		handler.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
