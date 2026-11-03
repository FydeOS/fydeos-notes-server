// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package httpserver

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fydeos/fydeos-notes-server/internal/config"
)

func TestApplicationCORSMatchesLegacy(t *testing.T) {
	for _, origin := range []string{"", "http://notes.test", "http://NOTES.TEST:80", "ws://notes.test", "https://localhost", "capacitor://localhost", "ionic://localhost", "assets://.", "assets://another-host", "file://"} {
		t.Run(origin, func(t *testing.T) {
			called := false
			h := NewApplicationHandler(config.Config{ExternalURL: "http://notes.test", NodeEnv: "production"}, nil, ApplicationHandlers{
				GraphQL: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }),
			})
			r := httptest.NewRequest(http.MethodOptions, "/graphql", nil)
			r.Header.Set("Origin", origin)
			r.Header.Set("Access-Control-Request-Method", "POST")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if called || w.Code != http.StatusNoContent || w.Body.Len() != 0 {
				t.Fatalf("preflight = %d %q, handler called=%v", w.Code, w.Body.String(), called)
			}
			for name, value := range map[string]string{
				"Access-Control-Allow-Origin": origin, "Access-Control-Allow-Credentials": "true", "Vary": "Origin",
				"Access-Control-Allow-Methods":  "GET,HEAD,PUT,PATCH,POST,DELETE,OPTIONS",
				"Access-Control-Allow-Headers":  "accept,authorization,content-type,x-affine-version,x-affine-client-kind,x-operation-name,x-request-id,x-captcha-token,x-captcha-challenge,x-captcha-provider,x-affine-csrf-token,x-requested-with,range",
				"Access-Control-Expose-Headers": "content-length,content-range,x-request-id", "Access-Control-Max-Age": "86400", "Content-Length": "0",
			} {
				if got := w.Header().Get(name); got != value {
					t.Errorf("%s = %q, want %q", name, got, value)
				}
			}
		})
	}
}

func TestApplicationCORSRejectedOriginsDoNotGrantBrowserAccess(t *testing.T) {
	h := NewApplicationHandler(config.Config{ExternalURL: "http://notes.test", Host: "127.0.0.1", NodeEnv: "production"}, nil, ApplicationHandlers{})
	for _, origin := range []string{"http://notes.test.evil.test", "https://notes.test", "http://127.0.0.1", "null", "not-an-origin"} {
		r := httptest.NewRequest(http.MethodGet, "/info", nil)
		r.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 || w.Header().Get("Access-Control-Allow-Origin") != "" || w.Header().Get("Access-Control-Allow-Credentials") != "" || w.Header().Get("Vary") != "" {
			t.Errorf("rejected origin %q: status=%d headers=%v", origin, w.Code, w.Header())
		}
	}
}
