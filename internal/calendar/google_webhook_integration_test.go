// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package calendar

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestGoogleCalendarWebhookValidatesTokenAndSchedulesKnownChannel(t *testing.T) {
	databaseURL := os.Getenv("NOTES_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set NOTES_TEST_DATABASE_URL to a disposable local notes_test database")
	}
	parsed, err := url.Parse(databaseURL)
	if err != nil || (parsed.Hostname() != "localhost" && parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "::1") {
		t.Fatal("Google Calendar webhook integration test requires loopback PostgreSQL")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	schema := "calendar_webhook_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		admin.Close()
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
		`CREATE TABLE calendar_subscriptions(id text PRIMARY KEY,provider text NOT NULL,external_calendar_id text NOT NULL,enabled boolean NOT NULL,custom_channel_id text,custom_resource_id text,channel_expiration timestamptz,sync_claimed_until timestamptz,next_sync_at timestamptz NOT NULL,updated_at timestamptz NOT NULL)`,
		`INSERT INTO app_configs(id,value) VALUES('calendar.google','{"enabled":true,"externalWebhookUrl":"https://notes.example.test","webhookVerificationToken":"secret-token"}'::jsonb)`,
		`INSERT INTO calendar_subscriptions(id,provider,external_calendar_id,enabled,custom_channel_id,sync_claimed_until,next_sync_at,updated_at) VALUES('known','Google','primary',true,'channel-known',clock_timestamp()+interval '30 minutes',clock_timestamp()+interval '1 day',clock_timestamp()),('disabled','Google','disabled',false,'channel-disabled',NULL,clock_timestamp()+interval '1 day',clock_timestamp()),('wrong-provider','CalDAV','other',true,'channel-other',NULL,clock_timestamp()+interval '1 day',clock_timestamp())`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	linker := NewGoogleAccountLinkerWithEndpoints(pool, "", nil, "https://notes.example.test", GoogleEndpoints{})
	handler := linker.WebhookHandler()
	request := func(channel, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/calendar/webhook/google", nil)
		if channel != "" {
			req.Header.Set("x-goog-channel-id", channel)
		}
		if token != "" {
			req.Header.Set("x-goog-channel-token", token)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	before := time.Now().Add(-time.Second)
	if response := request("channel-known", "wrong"); response.Code != http.StatusUnauthorized {
		t.Fatalf("invalid token status = %d, want 401", response.Code)
	}
	if response := request("channel-unknown", "secret-token"); response.Code != http.StatusOK {
		t.Fatalf("unknown channel status = %d, want 200", response.Code)
	}
	if response := request("channel-known", "secret-token"); response.Code != http.StatusOK {
		t.Fatalf("known channel status = %d, want 200", response.Code)
	}
	var dueAt time.Time
	if err := pool.QueryRow(ctx, `SELECT next_sync_at FROM calendar_subscriptions WHERE id='known'`).Scan(&dueAt); err != nil {
		t.Fatal(err)
	}
	if !dueAt.After(before) || dueAt.After(time.Now().Add(time.Second)) {
		t.Fatalf("webhook did not schedule sync promptly: %s", dueAt)
	}
	for _, id := range []string{"disabled", "wrong-provider"} {
		var untouched bool
		if err := pool.QueryRow(ctx, `SELECT next_sync_at>clock_timestamp()+interval '23 hours' FROM calendar_subscriptions WHERE id=$1`, id).Scan(&untouched); err != nil || !untouched {
			t.Fatalf("subscription %s was scheduled: untouched=%t err=%v", id, untouched, err)
		}
	}

	watchCalls, stopCalls := 0, 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer access" {
			t.Errorf("provider authorization = %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/calendars/primary/events/watch":
			watchCalls++
			var body struct {
				ID      string `json:"id"`
				Type    string `json:"type"`
				Address string `json:"address"`
				Token   string `json:"token"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" || body.Type != "web_hook" || body.Address != "https://notes.example.test/api/calendar/webhook/google" || body.Token != "secret-token" {
				t.Errorf("watch payload = %#v err=%v", body, err)
			}
			_, _ = fmt.Fprintf(w, `{"id":%q,"resourceId":"resource-%d","expiration":%q}`, body.ID, watchCalls, fmt.Sprint(time.Now().Add(7*24*time.Hour).UnixMilli()))
		case "/channels/stop":
			stopCalls++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()
	linker = NewGoogleAccountLinkerWithEndpoints(pool, "", nil, "https://notes.example.test", GoogleEndpoints{
		EventsURL: provider.URL + "/calendars", ChannelsStopURL: provider.URL + "/channels/stop",
	})
	claim := calendarSyncClaim{ID: "known"}
	if err := pool.QueryRow(ctx, `SELECT sync_claimed_until FROM calendar_subscriptions WHERE id='known'`).Scan(&claim.ClaimedUntil); err != nil {
		t.Fatal(err)
	}
	providerConfig := googleCalendarConfig{ExternalWebhookURL: "https://notes.example.test", WebhookVerificationToken: "secret-token"}
	subscription := calendarSyncSubscription{ID: claim.ID, ExternalCalendarID: "primary"}
	if err := linker.ensureWebhookChannel(ctx, claim, subscription, "access", providerConfig); err != nil {
		t.Fatalf("create Google watch channel: %v", err)
	}
	var channelID, resourceID string
	var expiration pgtype.Timestamptz
	if err := pool.QueryRow(ctx, `SELECT custom_channel_id,custom_resource_id,channel_expiration FROM calendar_subscriptions WHERE id='known'`).Scan(&channelID, &resourceID, &expiration); err != nil || channelID == "" || resourceID != "resource-1" || !expiration.Valid {
		t.Fatalf("saved watch channel=%q resource=%q expiration=%v err=%v", channelID, resourceID, expiration, err)
	}
	soon := time.Now().Add(30 * time.Minute)
	subscription.CustomChannelID = &channelID
	subscription.CustomResourceID = &resourceID
	subscription.ChannelExpiration = pgtype.Timestamptz{Time: soon, Valid: true}
	if err := linker.ensureWebhookChannel(ctx, claim, subscription, "access", providerConfig); err != nil {
		t.Fatalf("renew Google watch channel: %v", err)
	}
	if watchCalls != 2 || stopCalls != 1 {
		t.Fatalf("watch calls=%d stop calls=%d, want 2 and 1", watchCalls, stopCalls)
	}
}
