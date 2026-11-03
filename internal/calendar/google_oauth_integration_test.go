// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package calendar

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestGoogleCalendarOAuthCallbackPersistsAccountAndCalendars(t *testing.T) {
	databaseURL := os.Getenv("NOTES_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set NOTES_TEST_DATABASE_URL to a disposable local notes_test database")
	}
	parsed, err := url.Parse(databaseURL)
	if err != nil || (parsed.Hostname() != "localhost" && parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "::1") {
		t.Fatal("Google Calendar integration test requires a loopback PostgreSQL URL")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect to integration PostgreSQL: %v", err)
	}
	schema := "calendar_oauth_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	poolConfig.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, `SET search_path TO `+schema)
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		if _, err := admin.Exec(ctx, `DROP SCHEMA `+schema+` CASCADE`); err != nil {
			t.Errorf("drop test schema: %v", err)
		}
		admin.Close()
	})
	for _, statement := range []string{
		`CREATE TABLE app_configs(id text PRIMARY KEY,value jsonb NOT NULL)`,
		`CREATE TABLE runtime_states(purpose text NOT NULL,token_hash text NOT NULL,payload jsonb NOT NULL,consumed_at timestamptz,expires_at timestamptz NOT NULL,created_at timestamptz NOT NULL DEFAULT now(),updated_at timestamptz NOT NULL DEFAULT now(),PRIMARY KEY(purpose,token_hash))`,
		`CREATE TABLE calendar_accounts(id text PRIMARY KEY,user_id text NOT NULL,provider text NOT NULL,provider_account_id text NOT NULL,display_name text,email text,access_token text,refresh_token text,expires_at timestamptz,scope text,status text NOT NULL,last_error text,refresh_interval_minutes integer NOT NULL DEFAULT 30,created_at timestamptz NOT NULL,updated_at timestamptz NOT NULL,UNIQUE(user_id,provider,provider_account_id))`,
		`CREATE TABLE calendar_subscriptions(id text PRIMARY KEY,account_id text NOT NULL REFERENCES calendar_accounts(id) ON DELETE CASCADE,provider text NOT NULL,external_calendar_id text NOT NULL,display_name text,timezone text,color text,enabled boolean NOT NULL,sync_token text,last_sync_at timestamptz,sync_retry_count integer NOT NULL DEFAULT 0,sync_claimed_until timestamptz,next_sync_at timestamptz NOT NULL,custom_channel_id text,custom_resource_id text,channel_expiration timestamptz,created_at timestamptz NOT NULL,updated_at timestamptz NOT NULL,UNIQUE(account_id,external_calendar_id))`,
		`CREATE TABLE calendar_events(id text PRIMARY KEY,subscription_id text NOT NULL REFERENCES calendar_subscriptions(id) ON DELETE CASCADE,external_event_id text NOT NULL,recurrence_id text,etag text,status text,title text,description text,location text,start_at_utc timestamptz NOT NULL,end_at_utc timestamptz NOT NULL,original_timezone text,all_day boolean NOT NULL DEFAULT false,provider_updated_at timestamptz,raw jsonb NOT NULL,created_at timestamptz NOT NULL DEFAULT now(),updated_at timestamptz NOT NULL DEFAULT now())`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	providerConfig := `{"enabled":true,"allowNewAccounts":true,"clientId":"google-client","clientSecret":"google-secret"}`
	if _, err := pool.Exec(ctx, `INSERT INTO app_configs(id,value) VALUES('calendar.google',$1::jsonb)`, providerConfig); err != nil {
		t.Fatal(err)
	}
	providerRequests := 0
	google := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerRequests++
		switch r.URL.Path {
		case "/token":
			if err := r.ParseForm(); err != nil || r.Form.Get("client_secret") != "google-secret" {
				t.Errorf("token form=%v err=%v", r.Form, err)
			}
			if r.Form.Get("grant_type") == "refresh_token" {
				if r.Form.Get("refresh_token") != "plain-refresh" {
					t.Errorf("refresh token form=%v", r.Form)
				}
				_, _ = w.Write([]byte(`{"access_token":"refreshed-access","expires_in":3600}`))
				return
			}
			if r.Form.Get("redirect_uri") != "https://notes.example.test/api/calendar/oauth/callback" {
				t.Errorf("authorization token form=%v", r.Form)
			}
			accessToken := "plain-access"
			if r.Form.Get("code") == "new-account" {
				accessToken = "new-access"
			}
			_, _ = fmt.Fprintf(w, `{"access_token":%q,"refresh_token":"plain-refresh","expires_in":3600,"scope":"calendar.readonly"}`, accessToken)
		case "/userinfo":
			if r.Header.Get("Authorization") == "Bearer new-access" {
				_, _ = w.Write([]byte(`{"id":"google-subject-2","email":"new@example.test","name":"New Calendar User"}`))
			} else {
				_, _ = w.Write([]byte(`{"id":"google-subject-1","email":"calendar@example.test","name":"Calendar User"}`))
			}
		case "/calendarList":
			_, _ = w.Write([]byte(`{"items":[{"id":"primary","summary":"Primary","timeZone":"UTC","colorId":"9"}]}`))
		case "/calendars/primary/events":
			if r.Header.Get("Authorization") != "Bearer plain-access" && r.Header.Get("Authorization") != "Bearer refreshed-access" {
				t.Errorf("calendar events authorization=%q", r.Header.Get("Authorization"))
			}
			switch r.URL.Query().Get("syncToken") {
			case "expired-token":
				w.WriteHeader(http.StatusGone)
				return
			case "sync-1":
				_, _ = w.Write([]byte(`{"items":[{"id":"event-1","etag":"etag-2","status":"confirmed","summary":"Updated","start":{"dateTime":"2026-06-01T11:00:00Z"},"end":{"dateTime":"2026-06-01T12:00:00Z"},"updated":"2026-06-01T10:00:00Z"},{"id":"event-2","status":"cancelled","start":{"dateTime":"2026-06-02T10:00:00Z"},"end":{"dateTime":"2026-06-02T11:00:00Z"}}],"nextSyncToken":"sync-2"}`))
			default:
				_, _ = w.Write([]byte(`{"items":[{"id":"event-1","etag":"etag-1","status":"confirmed","summary":"Initial","description":"Agenda","location":"Room 1","start":{"dateTime":"2026-06-01T09:00:00Z"},"end":{"dateTime":"2026-06-01T10:00:00Z"},"updated":"2026-06-01T08:00:00Z"},{"id":"event-2","status":"confirmed","summary":"Cancelled later","start":{"dateTime":"2026-06-02T10:00:00Z"},"end":{"dateTime":"2026-06-02T11:00:00Z"}}],"nextSyncToken":"sync-1"}`))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer google.Close()
	linker := NewGoogleAccountLinkerWithEndpoints(pool, "", func(context.Context) (string, error) { return "test-encryption-key", nil }, "https://notes.example.test", GoogleEndpoints{
		AuthorizationURL: google.URL + "/authorize", TokenURL: google.URL + "/token", UserInfoURL: google.URL + "/userinfo", CalendarListURL: google.URL + "/calendarList", EventsURL: google.URL + "/calendars",
	})
	redirect := "https://notes.example.test/workspace/settings?tab=calendar"
	authURL, err := linker.CreateAuthorizationURL(ctx, "user-google-1", &redirect)
	if err != nil {
		t.Fatal(err)
	}
	parsedAuthURL, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	state := parsedAuthURL.Query().Get("state")
	if state == "" || parsedAuthURL.Query().Get("scope") == "" {
		t.Fatalf("authorization URL missing state/scopes: %s", authURL)
	}

	callback := httptest.NewRecorder()
	linker.ServeHTTP(callback, httptest.NewRequest(http.MethodGet, "/api/calendar/oauth/callback?code=code-value&state="+url.QueryEscape(state), nil))
	if callback.Code != http.StatusFound || callback.Header().Get("Location") != redirect {
		t.Fatalf("callback status=%d location=%q body=%q", callback.Code, callback.Header().Get("Location"), callback.Body.String())
	}
	if providerRequests != 4 {
		t.Fatalf("Google provider requests=%d, want token/profile/calendar-list/initial-events", providerRequests)
	}
	var accountID, accessToken, refreshToken, status string
	if err := pool.QueryRow(ctx, `SELECT id,access_token,refresh_token,status FROM calendar_accounts WHERE user_id='user-google-1'`).Scan(&accountID, &accessToken, &refreshToken, &status); err != nil {
		t.Fatal(err)
	}
	if accessToken == "plain-access" || refreshToken == "plain-refresh" || status != "active" {
		t.Fatalf("account credentials/status were not stored safely: access=%q refresh=%q status=%q", accessToken, refreshToken, status)
	}
	var subscriptionCount int
	var calendarName, timezone string
	if err := pool.QueryRow(ctx, `SELECT count(*)::int FROM calendar_subscriptions WHERE account_id=$1`, accountID).Scan(&subscriptionCount); err != nil {
		t.Fatal(err)
	}
	if subscriptionCount != 1 {
		t.Fatalf("calendar subscription count=%d", subscriptionCount)
	}
	if err := pool.QueryRow(ctx, `SELECT display_name,timezone FROM calendar_subscriptions WHERE account_id=$1`, accountID).Scan(&calendarName, &timezone); err != nil || calendarName != "Primary" || timezone != "UTC" {
		t.Fatalf("calendar metadata name=%q timezone=%q err=%v", calendarName, timezone, err)
	}
	var eventCount int
	var eventSummary, syncToken string
	if err := pool.QueryRow(ctx, `SELECT count(*)::int FROM calendar_events`).Scan(&eventCount); err != nil || eventCount != 2 {
		t.Fatalf("initial synced event count=%d err=%v", eventCount, err)
	}
	if err := pool.QueryRow(ctx, `SELECT title FROM calendar_events WHERE external_event_id='event-1'`).Scan(&eventSummary); err != nil || eventSummary != "Initial" {
		t.Fatalf("initial event title=%q err=%v", eventSummary, err)
	}
	if err := pool.QueryRow(ctx, `SELECT sync_token FROM calendar_subscriptions WHERE account_id=$1`, accountID).Scan(&syncToken); err != nil || syncToken != "sync-1" {
		t.Fatalf("initial sync token=%q err=%v", syncToken, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE calendar_subscriptions SET next_sync_at=clock_timestamp()-interval '1 second' WHERE account_id=$1`, accountID); err != nil {
		t.Fatal(err)
	}
	if err := linker.SyncDue(ctx); err != nil {
		t.Fatalf("incremental Google Calendar sync: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*)::int FROM calendar_events`).Scan(&eventCount); err != nil || eventCount != 1 {
		t.Fatalf("incremental synced event count=%d err=%v", eventCount, err)
	}
	if err := pool.QueryRow(ctx, `SELECT title FROM calendar_events WHERE external_event_id='event-1'`).Scan(&eventSummary); err != nil || eventSummary != "Updated" {
		t.Fatalf("updated event title=%q err=%v", eventSummary, err)
	}
	if err := pool.QueryRow(ctx, `SELECT sync_token FROM calendar_subscriptions WHERE account_id=$1`, accountID).Scan(&syncToken); err != nil || syncToken != "sync-2" {
		t.Fatalf("incremental sync token=%q err=%v", syncToken, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE calendar_accounts SET expires_at=clock_timestamp()-interval '1 minute' WHERE id=$1`, accountID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE calendar_subscriptions SET sync_token='expired-token',next_sync_at=clock_timestamp()-interval '1 second' WHERE account_id=$1`, accountID); err != nil {
		t.Fatal(err)
	}
	if err := linker.SyncDue(ctx); err != nil {
		t.Fatalf("Google token refresh and expired sync-token recovery: %v", err)
	}
	var refreshedAccess string
	if err := pool.QueryRow(ctx, `SELECT access_token FROM calendar_accounts WHERE id=$1`, accountID).Scan(&accessToken); err != nil {
		t.Fatal(err)
	}
	refreshedAccess, err = decryptCalendarToken("test-encryption-key", accessToken)
	if err != nil || refreshedAccess != "refreshed-access" {
		t.Fatalf("refreshed access token=%q err=%v", refreshedAccess, err)
	}
	if err := pool.QueryRow(ctx, `SELECT sync_token FROM calendar_subscriptions WHERE account_id=$1`, accountID).Scan(&syncToken); err != nil || syncToken != "sync-1" {
		t.Fatalf("full resync token=%q err=%v", syncToken, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*)::int FROM calendar_events`).Scan(&eventCount); err != nil || eventCount != 2 {
		t.Fatalf("full resync event count=%d err=%v", eventCount, err)
	}
	var stateConsumed bool
	if err := pool.QueryRow(ctx, `SELECT consumed_at IS NOT NULL FROM runtime_states WHERE purpose=$1 AND token_hash=$2`, calendarOAuthStatePurpose, calendarStateHash(state)).Scan(&stateConsumed); err != nil || !stateConsumed {
		t.Fatalf("OAuth state consumed=%t err=%v", stateConsumed, err)
	}
	replay := httptest.NewRecorder()
	linker.ServeHTTP(replay, httptest.NewRequest(http.MethodGet, "/api/calendar/oauth/callback?code=code-value&state="+url.QueryEscape(state), nil))
	if replay.Code != http.StatusBadGateway || providerRequests != 8 {
		t.Fatalf("replayed callback status=%d Google requests=%d body=%s", replay.Code, providerRequests, replay.Body.String())
	}
	secondURL, err := linker.CreateAuthorizationURL(ctx, "user-google-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	secondAuth, err := url.Parse(secondURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE app_configs SET value=jsonb_set(value,'{allowNewAccounts}','false'::jsonb) WHERE id='calendar.google'`); err != nil {
		t.Fatal(err)
	}
	newAccountCallback := httptest.NewRecorder()
	linker.ServeHTTP(newAccountCallback, httptest.NewRequest(http.MethodGet, "/api/calendar/oauth/callback?code=new-account&state="+url.QueryEscape(secondAuth.Query().Get("state")), nil))
	if newAccountCallback.Code != http.StatusBadGateway || providerRequests != 10 {
		t.Fatalf("new-account callback after disable status=%d Google requests=%d body=%s", newAccountCallback.Code, providerRequests, newAccountCallback.Body.String())
	}
	var accountCount int
	if err := pool.QueryRow(ctx, `SELECT count(*)::int FROM calendar_accounts WHERE user_id='user-google-1'`).Scan(&accountCount); err != nil || accountCount != 1 {
		t.Fatalf("new account was persisted after linking was disabled: count=%d err=%v", accountCount, err)
	}
}
