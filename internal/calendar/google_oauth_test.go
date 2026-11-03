// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package calendar

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestGoogleCalendarOAuthProviderRequestsAndPaging(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/token":
			if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
				t.Errorf("token request=%s %s headers=%v", r.Method, r.URL.Path, r.Header)
			}
			if err := r.ParseForm(); err != nil || r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != "code-value" {
				t.Errorf("token form=%v err=%v", r.Form, err)
			}
			_, _ = w.Write([]byte(`{"access_token":"access-secret","refresh_token":"refresh-secret","expires_in":3600,"scope":"calendar.readonly"}`))
		case "/userinfo":
			if r.Header.Get("Authorization") != "Bearer access-secret" {
				t.Errorf("userinfo authorization=%q", r.Header.Get("Authorization"))
			}
			_, _ = w.Write([]byte(`{"id":"google-account","email":"calendar@example.test","name":"Calendar User"}`))
		case "/calendarList":
			if r.Header.Get("Authorization") != "Bearer access-secret" || r.URL.Query().Get("maxResults") != "250" {
				t.Errorf("calendar list request=%v headers=%v", r.URL, r.Header)
			}
			if r.URL.Query().Get("pageToken") == "next-page" {
				_, _ = w.Write([]byte(`{"items":[{"id":"secondary","summary":"Secondary"}]}`))
			} else {
				_, _ = w.Write([]byte(`{"items":[{"id":"primary","summary":"Primary","timeZone":"UTC","colorId":"9"}],"nextPageToken":"next-page"}`))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	linker := NewGoogleAccountLinkerWithEndpoints(nil, "", nil, "https://notes.example.test", GoogleEndpoints{
		AuthorizationURL: server.URL + "/authorize",
		TokenURL:         server.URL + "/token",
		UserInfoURL:      server.URL + "/userinfo",
		CalendarListURL:  server.URL + "/calendarList",
	})
	provider := googleCalendarConfig{ClientID: "client", ClientSecret: "secret"}
	token, err := linker.exchangeCode(context.Background(), provider, "code-value", "https://notes.example.test/api/calendar/oauth/callback")
	if err != nil || token.AccessToken != "access-secret" || token.RefreshToken != "refresh-secret" || token.ExpiresIn != 3600 {
		t.Fatalf("token=%#v err=%v", token, err)
	}
	profile, err := linker.loadProfile(context.Background(), token.AccessToken)
	if err != nil || profile.ID != "google-account" || profile.Email != "calendar@example.test" {
		t.Fatalf("profile=%#v err=%v", profile, err)
	}
	calendars, err := linker.listCalendars(context.Background(), token.AccessToken)
	if err != nil || len(calendars) != 2 || calendars[0].ID != "primary" || calendars[1].ID != "secondary" {
		t.Fatalf("calendars=%#v err=%v", calendars, err)
	}
	if len(requests) != 4 {
		t.Fatalf("Google requests=%v", requests)
	}
}

func TestGoogleCalendarAuthorizationURLAndRedirectPolicy(t *testing.T) {
	got := googleAuthorizationURL("https://accounts.google.test/oauth2/auth", "client-id", "state-token", "https://notes.example.test/api/calendar/oauth/callback")
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	if parsed.Host != "accounts.google.test" || query.Get("client_id") != "client-id" || query.Get("state") != "state-token" || query.Get("redirect_uri") != "https://notes.example.test/api/calendar/oauth/callback" || query.Get("access_type") != "offline" || query.Get("prompt") != "consent" || !strings.Contains(query.Get("scope"), "https://www.googleapis.com/auth/calendar.readonly") {
		t.Fatalf("authorization URL params=%v", query)
	}

	for _, test := range []struct {
		input string
		base  string
		allow bool
	}{
		{input: "https://notes.example.test/settings?tab=calendar", base: "https://notes.example.test/base", allow: true},
		{input: "https://app.other.example/settings", base: "https://notes.example.test/base", allow: false},
		{input: "http://localhost:3000/settings", base: "http://localhost:3000", allow: true},
		{input: "http://localhost:3001/settings", base: "https://notes.example.test/base", allow: false},
		{input: "https://notes.example.test.evil.test/", base: "https://notes.example.test/base", allow: false},
		{input: "https://evil.example.test/", base: "https://notes.example.test/base", allow: false},
		{input: "javascript:alert(1)", base: "https://notes.example.test/base", allow: false},
	} {
		_, got := canonicalCalendarRedirect(test.input, test.base)
		if got != test.allow {
			t.Errorf("canonicalCalendarRedirect(%q) allowed=%t, want %t", test.input, got, test.allow)
		}
	}

}
