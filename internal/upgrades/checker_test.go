// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package upgrades

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCheckerUsesReleaseChannelHeadersAndLatestRelease(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/worker/releases" || r.URL.Query().Get("channel") != "beta" {
			t.Errorf("request = %s %s, want beta releases GET", r.Method, r.URL.String())
		}
		if r.Header.Get("Accept") != "application/json" || r.Header.Get("Cache-Control") != "no-cache" {
			t.Errorf("headers = %v", r.Header)
		}
		fmt.Fprint(w, `[{"name":"0.27.6","url":"https://example.org/releases/0.27.6","body":null,"published_at":"2026-10-17T12:30:00+08:00"}]`)
	}))
	defer server.Close()

	release, err := NewChecker(server.Client(), server.URL+"/api/worker/releases").Available(context.Background(), "beta", "0.27.5")
	if err != nil {
		t.Fatal(err)
	}
	if release == nil || release.Version != "0.27.6" || release.URL != "https://example.org/releases/0.27.6" || release.Changelog != "" {
		t.Fatalf("release = %#v", release)
	}
	if want := time.Date(2026, time.October, 17, 4, 30, 0, 0, time.UTC); !release.PublishedAt.Equal(want) {
		t.Fatalf("publishedAt = %s, want %s", release.PublishedAt, want)
	}
}

func TestReleaseChannelMapsNamespaces(t *testing.T) {
	for namespace, want := range map[string]string{"dev": "canary", "beta": "beta", "production": "stable", "other": "stable", "": "stable"} {
		if got := releaseChannel(namespace); got != want {
			t.Errorf("releaseChannel(%q) = %q, want %q", namespace, got, want)
		}
	}
}

func TestHasNewerVersionMatchesLegacyComparison(t *testing.T) {
	tests := []struct {
		current string
		next    string
		want    bool
	}{
		{"0.26.5", "0.26.4", false},
		{"0.26.5", "0.26.5", false},
		{"0.26.5", "0.26.6", true},
		{"0.26.5", "0.26.6-beta.1", true},
		{"0.26.6-beta.2", "0.26.6-beta.1", false},
		{"2026.2.6-canary.015", "2026.2.6-canary.016", true},
		{"not-semver", "another-tag", true},
		{" not-semver ", "not-semver", false},
	}
	for _, test := range tests {
		if got := hasNewerVersion(test.current, test.next); got != test.want {
			t.Errorf("hasNewerVersion(%q, %q) = %v, want %v", test.current, test.next, got, test.want)
		}
	}
}

func TestCheckerReturnsNilForNoUpgradeAndErrorsForBadResponses(t *testing.T) {
	for _, test := range []struct {
		name      string
		status    int
		body      string
		wantNil   bool
		wantError bool
	}{
		{name: "empty", status: http.StatusOK, body: `[]`, wantNil: true},
		{name: "not newer", status: http.StatusOK, body: `[{"name":"0.27.5","published_at":"2026-10-17T00:00:00Z"}]`, wantNil: true},
		{name: "http error", status: http.StatusBadGateway, body: "unavailable", wantNil: true, wantError: true},
		{name: "invalid json", status: http.StatusOK, body: `{`, wantNil: true, wantError: true},
		{name: "invalid date", status: http.StatusOK, body: `[{"name":"0.27.6","published_at":"invalid"}]`, wantNil: true, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				fmt.Fprint(w, test.body)
			}))
			defer server.Close()

			release, err := NewChecker(server.Client(), server.URL).Available(context.Background(), "production", "0.27.5")
			if (err != nil) != test.wantError {
				t.Fatalf("error = %v, wantError %v", err, test.wantError)
			}
			if (release == nil) != test.wantNil {
				t.Fatalf("release = %#v, want nil %v", release, test.wantNil)
			}
		})
	}
}
