// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package realtime

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/httpserver"
)

func TestRealtimeCORSUsesNativeClientOriginsWithoutHTTPOnlyHeaders(t *testing.T) {
	cfg := config.Config{ExternalURL: "http://notes.test", Host: "127.0.0.1", NodeEnv: "production", Flavor: "graphql"}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for name, h := range map[string]http.Handler{"direct": s.Handler(), "application": httpserver.NewApplicationHandler(cfg, nil, httpserver.ApplicationHandlers{SocketIO: s.Handler()})} {
		for _, origin := range []string{"", "http://notes.test", "capacitor://localhost", "assets://.", "ws://notes.test", "http://NOTES.TEST:80", "http://notes.test.evil.test", "http://127.0.0.1"} {
			t.Run(name+"/"+origin, func(t *testing.T) {
				r := httptest.NewRequest(http.MethodOptions, "/socket.io/?EIO=4&transport=polling", nil)
				r.Header.Set("Origin", origin)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if origin == "http://notes.test.evil.test" || origin == "http://127.0.0.1" {
					if w.Code != 400 || w.Header().Get("Access-Control-Allow-Origin") != "" || w.Header().Get("Access-Control-Allow-Credentials") != "" || !strings.Contains(w.Body.String(), "Bad handshake method") {
						t.Fatalf("rejected origin response = %d %v %s", w.Code, w.Header(), w.Body.String())
					}
					return
				}
				if w.Code != 204 || w.Header().Get("Access-Control-Allow-Origin") != origin || w.Header().Get("Access-Control-Allow-Credentials") != "true" || w.Header().Get("Vary") != "Origin" || w.Header().Get("Access-Control-Expose-Headers") != "" || w.Header().Get("Access-Control-Max-Age") != "" {
					t.Fatalf("accepted origin response = %d %v", w.Code, w.Header())
				}
			})
		}
	}
}
