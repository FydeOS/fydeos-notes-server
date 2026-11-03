// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package calendar

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCalDAVSyncDueAndTokenRecovery(t *testing.T) {
	databaseURL := os.Getenv("NOTES_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set NOTES_TEST_DATABASE_URL to a disposable local notes_test database")
	}
	parsed, err := url.Parse(databaseURL)
	if err != nil || (parsed.Hostname() != "localhost" && parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "::1") {
		t.Fatal("CalDAV sync integration test requires a loopback PostgreSQL URL")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	schema := "caldav_sync_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
		`CREATE TABLE calendar_accounts(id text PRIMARY KEY,user_id text NOT NULL,provider text NOT NULL,provider_account_id text NOT NULL,display_name text,email text,provider_preset_id text,server_url text,principal_url text,calendar_home_url text,username text,auth_type text,access_token text,refresh_token text,expires_at timestamptz,scope text,status text NOT NULL,last_error text,refresh_interval_minutes int NOT NULL DEFAULT 30,created_at timestamptz NOT NULL DEFAULT now(),updated_at timestamptz NOT NULL DEFAULT now(),UNIQUE(user_id,provider,provider_account_id))`,
		`CREATE TABLE calendar_subscriptions(id text PRIMARY KEY,account_id text NOT NULL REFERENCES calendar_accounts(id) ON DELETE CASCADE,provider text NOT NULL,external_calendar_id text NOT NULL,display_name text,timezone text,color text,enabled boolean NOT NULL DEFAULT true,sync_token text,last_sync_at timestamptz,sync_retry_count int NOT NULL DEFAULT 0,sync_claimed_until timestamptz,next_sync_at timestamptz NOT NULL DEFAULT now(),created_at timestamptz NOT NULL DEFAULT now(),updated_at timestamptz NOT NULL DEFAULT now(),UNIQUE(account_id,external_calendar_id))`,
		`CREATE TABLE calendar_events(id text PRIMARY KEY,subscription_id text NOT NULL REFERENCES calendar_subscriptions(id) ON DELETE CASCADE,external_event_id text NOT NULL,recurrence_id text,etag text,status text,title text,description text,location text,start_at_utc timestamptz NOT NULL,end_at_utc timestamptz NOT NULL,original_timezone text,all_day boolean NOT NULL DEFAULT false,provider_updated_at timestamptz,raw jsonb NOT NULL,created_at timestamptz NOT NULL DEFAULT now(),updated_at timestamptz NOT NULL DEFAULT now())`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	requests := 0
	providerStatus := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "REPORT" {
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
			return
		}
		requests++
		if providerStatus != 0 {
			w.WriteHeader(providerStatus)
			return
		}
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read REPORT body: %v", err)
		}
		body := string(bodyBytes)
		switch {
		case strings.Contains(body, "sync-token>bad"):
			w.WriteHeader(http.StatusForbidden)
		case strings.Contains(body, "sync-token>sync-1"):
			_, _ = fmt.Fprint(w, calDAVMultiStatus("sync-2", `
				<d:response><d:href>/cal/user/home/a.ics</d:href><d:propstat><d:prop><d:getetag>"2"</d:getetag><c:calendar-data>`+html.EscapeString(testICalendar("Updated event"))+`</c:calendar-data></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>
				<d:response><d:href>/cal/user/home/b.ics</d:href><d:status>HTTP/1.1 404 Not Found</d:status></d:response>`))
		default:
			if !strings.Contains(body, "calendar-query") {
				t.Errorf("initial request was not calendar-query: %s", body)
			}
			_, _ = fmt.Fprint(w, calDAVMultiStatus("sync-1", `
				<d:response><d:href>/cal/user/home/a.ics</d:href><d:propstat><d:prop><d:getetag>"1"</d:getetag><c:calendar-data>`+html.EscapeString(testICalendar("Initial event"))+`</c:calendar-data></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>
				<d:response><d:href>/cal/user/home/b.ics</d:href><d:propstat><d:prop><d:getetag>"b"</d:getetag><c:calendar-data>`+html.EscapeString(testICalendar("Deleted event"))+`</c:calendar-data></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`))
		}
	}))
	defer server.Close()
	configJSON := fmt.Sprintf(`{"enabled":true,"allowInsecureHttp":true,"blockPrivateNetwork":false,"providers":[{"id":"test","serverUrl":%q,"authType":"basic"}]}`, server.URL)
	if _, err := pool.Exec(ctx, `INSERT INTO app_configs(id,value) VALUES('calendar.caldav',$1::jsonb)`, configJSON); err != nil {
		t.Fatal(err)
	}
	password, err := encryptCredential("test-key", "app-password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO calendar_accounts(id,user_id,provider,provider_account_id,provider_preset_id,server_url,username,auth_type,access_token,status)
		VALUES('account-1','user-1','CalDAV','principal','test',$1,'user','basic',$2,'active')`, server.URL, password); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO calendar_subscriptions(id,account_id,provider,external_calendar_id,timezone,enabled,next_sync_at)
		VALUES('sub-1','account-1','CalDAV',$1,'America/Los_Angeles',true,clock_timestamp())`, server.URL+"/cal/user/home/"); err != nil {
		t.Fatal(err)
	}
	service := NewService(pool, "", func(context.Context) (string, error) { return "test-key", nil })
	service.SetResolver(func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	})
	if err := service.SyncDue(ctx); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*)::int FROM calendar_events WHERE subscription_id='sub-1'`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("initial event count=%d err=%v", count, err)
	}
	var title, token string
	var start time.Time
	var allDay bool
	if err := pool.QueryRow(ctx, `SELECT title,start_at_utc,all_day FROM calendar_events WHERE external_event_id=$1`, server.URL+"/cal/user/home/a.ics").Scan(&title, &start, &allDay); err != nil {
		t.Fatal(err)
	}
	if title != "Initial event" || start.UTC().Hour() != 8 || !allDay {
		t.Fatalf("mapped event title=%q start=%s allDay=%t", title, start, allDay)
	}
	if err := pool.QueryRow(ctx, `SELECT sync_token FROM calendar_subscriptions WHERE id='sub-1'`).Scan(&token); err != nil || token != "sync-1" {
		t.Fatalf("initial sync token=%q err=%v", token, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE calendar_subscriptions SET next_sync_at=clock_timestamp()-interval '1 second' WHERE id='sub-1'`); err != nil {
		t.Fatal(err)
	}
	if err := service.SyncDue(ctx); err != nil {
		t.Fatalf("incremental sync: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*)::int FROM calendar_events WHERE subscription_id='sub-1'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("incremental event count=%d err=%v", count, err)
	}
	if err := pool.QueryRow(ctx, `SELECT title,sync_token FROM calendar_events event JOIN calendar_subscriptions subscription ON subscription.id=event.subscription_id WHERE subscription.id='sub-1' AND event.external_event_id=$1`, server.URL+"/cal/user/home/a.ics").Scan(&title, &token); err != nil || title != "Updated event" || token != "sync-2" {
		t.Fatalf("incremental title=%q token=%q err=%v", title, token, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE calendar_subscriptions SET sync_token='bad',next_sync_at=clock_timestamp()-interval '1 second' WHERE id='sub-1'`); err != nil {
		t.Fatal(err)
	}
	if err := service.SyncDue(ctx); err != nil {
		t.Fatalf("expired token full retry: %v", err)
	}
	if requests != 4 {
		t.Fatalf("CalDAV requests=%d, want initial, incremental, token rejection and full retry", requests)
	}

	providerStatus = http.StatusUnauthorized
	if _, err := pool.Exec(ctx, `UPDATE calendar_subscriptions SET next_sync_at=clock_timestamp()-interval '1 second' WHERE id='sub-1'`); err != nil {
		t.Fatal(err)
	}
	if err := service.SyncDue(ctx); err != nil {
		t.Fatalf("invalid credential cleanup: %v", err)
	}
	var accountStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM calendar_accounts WHERE id='account-1'`).Scan(&accountStatus); err != nil || accountStatus != "invalid" {
		t.Fatalf("account status after 401=%q err=%v", accountStatus, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*)::int FROM calendar_events WHERE subscription_id='sub-1'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("events after 401=%d err=%v", count, err)
	}

	if _, err := pool.Exec(ctx, `INSERT INTO calendar_accounts(id,user_id,provider,provider_account_id,provider_preset_id,server_url,username,auth_type,access_token,status)
		VALUES('account-2','user-1','CalDAV','principal-2','test',$1,'user','basic',$2,'active')`, server.URL, password); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO calendar_subscriptions(id,account_id,provider,external_calendar_id,timezone,enabled,next_sync_at,sync_token)
		VALUES('sub-2','account-2','CalDAV',$1,'UTC',true,clock_timestamp(),'sync-1')`, server.URL+"/cal/user/home/"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO calendar_events(id,subscription_id,external_event_id,start_at_utc,end_at_utc,raw)
		VALUES('event-2','sub-2','old-event',clock_timestamp(),clock_timestamp(),'{}'::jsonb)`); err != nil {
		t.Fatal(err)
	}
	providerStatus = http.StatusNotFound
	if err := service.SyncDue(ctx); err != nil {
		t.Fatalf("removed calendar cleanup: %v", err)
	}
	var enabled bool
	if err := pool.QueryRow(ctx, `SELECT enabled FROM calendar_subscriptions WHERE id='sub-2'`).Scan(&enabled); err != nil || enabled {
		t.Fatalf("subscription enabled after 404=%t err=%v", enabled, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*)::int FROM calendar_events WHERE subscription_id='sub-2'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("events after 404=%d err=%v", count, err)
	}
}

func TestParseICalendarEventPreservesRecurrenceAndEscapedText(t *testing.T) {
	content := "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:repeat-1\r\nRECURRENCE-ID;TZID=America/Los_Angeles:20260308T013000\r\nDTSTART;TZID=America/Los_Angeles:20260308T013000\r\nDTEND;TZID=America/Los_Angeles:20260308T033000\r\nSUMMARY:Design\\, review\\nPlanning\r\nEND:VEVENT\r\nEND:VCALENDAR"
	events := parseCalDAVICalendar(content, "https://calendar.test/event.ics", "etag", "UTC")
	if len(events) != 1 {
		t.Fatalf("parsed %d events", len(events))
	}
	event := events[0]
	if event.Summary != "Design, review\nPlanning" || event.RecurrenceID == nil || *event.RecurrenceID != "2026-03-08T09:30:00Z" {
		t.Fatalf("event mapping=%#v", event)
	}
	if event.Start.Hour() != 9 || event.End.Hour() != 10 || event.Timezone == nil || *event.Timezone != "America/Los_Angeles" {
		t.Fatalf("event times start=%s end=%s timezone=%v", event.Start, event.End, event.Timezone)
	}
}

func testICalendar(summary string) string {
	return "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:test-event\r\nDTSTART;VALUE=DATE:20260308\r\nDTEND;VALUE=DATE:20260309\r\nSUMMARY:" + summary + "\r\nEND:VEVENT\r\nEND:VCALENDAR"
}

func calDAVMultiStatus(token, responses string) string {
	return `<?xml version="1.0" encoding="UTF-8"?><d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:sync-token>` + token + `</d:sync-token>` + responses + `</d:multistatus>`
}
