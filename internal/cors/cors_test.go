// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package cors

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fydeos/fydeos-notes-server/internal/config"
)

func TestOriginPolicyConfiguredHostsAndDevelopment(t *testing.T) {
	for _, test := range []struct {
		name   string
		cfg    config.Config
		origin string
		allow  bool
	}{
		{"primary hostname", config.Config{Host: "notes.test", Port: 3010}, "http://notes.test", true},
		{"primary loopback port", config.Config{Host: "localhost", Port: 3010}, "http://localhost:3010", true},
		{"primary port required", config.Config{Host: "localhost", Port: 3010}, "http://localhost", false},
		{"configured alias", config.Config{ExternalURL: "https://notes.test/path", HTTPS: true, Hosts: []string{"ALIAS.TEST:8443"}}, "https://alias.test:8443", true},
		{"configured IPv6", config.Config{ExternalURL: "https://[::1]:8443/path"}, "https://[0:0:0:0:0:0:0:1]:8443", true},
		{"configured IDNA", config.Config{ExternalURL: "https://bücher.test"}, "https://xn--bcher-kva.test", true},
		{"alias scheme", config.Config{ExternalURL: "https://notes.test", HTTPS: true, Hosts: []string{"alias.test"}}, "http://alias.test", false},
		{"external URL replaces host", config.Config{ExternalURL: "https://notes.test", Host: "internal.test"}, "http://internal.test", false},
		{"arbitrary mobile host", config.Config{}, "capacitor://evil.test", false},
		{"desktop path", config.Config{}, "assets://another-host/path", false},
		{"development loopback", config.Config{NodeEnv: "development"}, "http://127.0.0.1:8123", true},
		{"test IPv6", config.Config{NodeEnv: "test"}, "https://[::1]:8123", true},
		{"development ws loopback", config.Config{NodeEnv: "development"}, "ws://127.0.0.1:8123", false},
		{"production loopback", config.Config{NodeEnv: "production"}, "http://127.0.0.1:8123", false},
		{"development suffix", config.Config{NodeEnv: "development"}, "http://localhost.evil.test:8123", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			called := false
			h := HTTP(test.cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(http.StatusUnauthorized) }))
			r := httptest.NewRequest(http.MethodGet, "/private", nil)
			r.Header.Set("Origin", test.origin)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if !called || w.Code != 401 {
				t.Fatalf("CORS changed authentication flow: %v/%d", called, w.Code)
			}
			got := w.Header().Get("Access-Control-Allow-Origin")
			if test.allow && got != test.origin || !test.allow && got != "" {
				t.Fatalf("allow-origin=%q, allow=%v", got, test.allow)
			}
		})
	}
}

func TestVaryPreservesExistingCacheDimensions(t *testing.T) {
	for _, test := range []struct{ before, want string }{{"Accept-Encoding", "Accept-Encoding, Origin"}, {"origin, Accept-Encoding", "origin, Accept-Encoding"}, {"*", "*"}} {
		r := httptest.NewRequest(http.MethodGet, "/info", nil)
		w := httptest.NewRecorder()
		w.Header().Set("Vary", test.before)
		HTTP(config.Config{}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(w, r)
		if got := w.Header().Get("Vary"); got != test.want {
			t.Errorf("Vary=%q, want %q", got, test.want)
		}
	}
}

func TestDuplicateOriginsDoNotSelectFirstValue(t *testing.T) {
	for _, origins := range [][]string{
		{"http://notes.test", "https://evil.test"},
		{"https://evil.test", "http://notes.test"},
		{"http://notes.test", "http://notes.test"},
		{"", "http://notes.test"},
	} {
		for name, wrap := range map[string]func(config.Config, http.Handler) http.Handler{"HTTP": HTTP, "Realtime": Realtime} {
			t.Run(name+"/"+origins[0]+"/"+origins[1], func(t *testing.T) {
				r := httptest.NewRequest(http.MethodGet, "/private", nil)
				for _, origin := range origins {
					r.Header.Add("Origin", origin)
				}
				w := httptest.NewRecorder()
				wrap(config.Config{ExternalURL: "http://notes.test"}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusUnauthorized)
				})).ServeHTTP(w, r)
				if w.Code != http.StatusUnauthorized || w.Header().Get("Access-Control-Allow-Origin") != "" || w.Header().Get("Access-Control-Allow-Credentials") != "" {
					t.Fatalf("duplicate origins changed auth or CORS contract: %d %v", w.Code, w.Header())
				}
			})
		}
	}
}
