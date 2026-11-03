// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/calendar"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/users"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

func TestWorkspaceCalendarSettingsAndEventsGraphQLParity(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	for _, statement := range []string{
		`CREATE TABLE calendar_accounts(id text PRIMARY KEY,user_id text NOT NULL,provider text NOT NULL,provider_account_id text NOT NULL,display_name text,email text,provider_preset_id text,server_url text,principal_url text,calendar_home_url text,username text,auth_type text,access_token text,refresh_token text,expires_at timestamptz,scope text,status text NOT NULL DEFAULT 'active',last_error text,refresh_interval_minutes int NOT NULL DEFAULT 30,created_at timestamptz NOT NULL DEFAULT now(),updated_at timestamptz NOT NULL DEFAULT now(),UNIQUE(user_id,provider,provider_account_id))`,
		`CREATE TABLE calendar_subscriptions(id text PRIMARY KEY,account_id text NOT NULL REFERENCES calendar_accounts(id) ON DELETE CASCADE,provider text NOT NULL,external_calendar_id text NOT NULL,display_name text,timezone text,color text,enabled boolean NOT NULL DEFAULT true,sync_token text,last_sync_at timestamptz,sync_retry_count int NOT NULL DEFAULT 0,sync_claimed_until timestamptz,next_sync_at timestamptz NOT NULL DEFAULT now(),created_at timestamptz NOT NULL DEFAULT now(),updated_at timestamptz NOT NULL DEFAULT now(),UNIQUE(account_id,external_calendar_id))`,
		`CREATE TABLE workspace_calendars(id text PRIMARY KEY,workspace_id text NOT NULL,created_by_user_id text NOT NULL,display_name_override text,color_override text,enabled boolean NOT NULL DEFAULT true,created_at timestamptz NOT NULL DEFAULT now(),updated_at timestamptz NOT NULL DEFAULT now())`,
		`CREATE TABLE workspace_calendar_items(id text PRIMARY KEY,workspace_calendar_id text NOT NULL REFERENCES workspace_calendars(id) ON DELETE CASCADE,subscription_id text NOT NULL REFERENCES calendar_subscriptions(id) ON DELETE CASCADE,sort_order int,color_override text,enabled boolean NOT NULL DEFAULT true,created_at timestamptz NOT NULL DEFAULT now(),updated_at timestamptz NOT NULL DEFAULT now(),UNIQUE(workspace_calendar_id,subscription_id))`,
		`CREATE TABLE calendar_events(id text PRIMARY KEY,subscription_id text NOT NULL REFERENCES calendar_subscriptions(id) ON DELETE CASCADE,external_event_id text NOT NULL,recurrence_id text,etag text,status text,title text,description text,location text,start_at_utc timestamptz NOT NULL,end_at_utc timestamptz NOT NULL,original_timezone text,all_day boolean NOT NULL DEFAULT false,provider_updated_at timestamptz,raw jsonb NOT NULL DEFAULT '{}'::jsonb,created_at timestamptz NOT NULL DEFAULT now(),updated_at timestamptz NOT NULL DEFAULT now())`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO calendar_accounts(id,user_id,provider,provider_account_id,display_name,email,access_token) VALUES('account-1',$1,'CalDAV','user@example.com','Personal','user@example.com','secret-token')`, fixture.userID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO calendar_subscriptions(id,account_id,provider,external_calendar_id,display_name) VALUES('sub-1','account-1','CalDAV','cal-1','Work'),('sub-2','account-1','CalDAV','cal-2','Personal')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO calendar_events(id,subscription_id,external_event_id,title,start_at_utc,end_at_utc) VALUES('event-1','sub-1','event-1','Review','2026-10-19T10:00:00Z','2026-10-19T11:00:00Z'),('event-outside','sub-2','outside','Outside','2026-10-20T10:00:00Z','2026-10-20T11:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	repo := workspaces.NewRepositoryWithDeployment(pool, "cloud")
	userService := users.NewService(users.NewRepository(pool), workspaces.NewAuthorizationService(repo))
	caldavServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/.well-known/caldav":
			w.WriteHeader(http.StatusNotImplemented)
		case r.Method == "PROPFIND" && r.URL.Path == "/caldav/":
			_, _ = w.Write([]byte(`<multistatus xmlns="DAV:"><response><propstat><prop><current-user-principal><href>/principals/user/</href></current-user-principal></prop></propstat></response></multistatus>`))
		case r.Method == "PROPFIND" && r.URL.Path == "/principals/user/":
			_, _ = w.Write([]byte(`<multistatus xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><response><propstat><prop><C:calendar-home-set><href>/calendars/user/</href></C:calendar-home-set></prop></propstat></response></multistatus>`))
		case r.Method == "PROPFIND" && r.URL.Path == "/calendars/user/":
			_, _ = w.Write([]byte(`<multistatus xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><response><href>/calendars/user/home/</href><propstat><prop><displayname>Home</displayname><resourcetype><collection/><C:calendar/></resourcetype></prop></propstat></response></multistatus>`))
		case r.Method == "REPORT" && r.URL.Path == "/calendars/user/home/":
			_, _ = w.Write([]byte(`<d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:sync-token>fixture-token</d:sync-token><d:response><d:href>/calendars/user/home/event.ics</d:href><d:propstat><d:prop><d:getetag>"one"</d:getetag><c:calendar-data>BEGIN:VCALENDAR` + "\r\n" + `BEGIN:VEVENT` + "\r\n" + `UID:fixture-event` + "\r\n" + `DTSTART:20261020T100000Z` + "\r\n" + `DTEND:20261020T110000Z` + "\r\n" + `SUMMARY:Fixture` + "\r\n" + `END:VEVENT` + "\r\n" + `END:VCALENDAR</c:calendar-data></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer caldavServer.Close()
	if _, err := pool.Exec(ctx, `INSERT INTO app_configs(id,value) VALUES('calendar.caldav',$1::jsonb),('crypto.privateKey','"calendar-integration-key"'::jsonb)`, fmt.Sprintf(`{"enabled":true,"allowInsecureHttp":true,"blockPrivateNetwork":false,"providers":[{"id":"test","serverUrl":%q,"authType":"basic"}]}`, caldavServer.URL+"/caldav/")); err != nil {
		t.Fatal(err)
	}
	caldavService := calendar.NewService(pool, "", func(context.Context) (string, error) { return "calendar-integration-key", nil })
	caldavService.SetResolver(func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	})
	handler := NewHandlerWithServices(config.Config{Deployment: "cloud"}, auth.NewSessionResolver(pool), userService, repo, nil, nil, nil, nil, HandlerDependencies{
		CalDAVAccountLinker:       caldavService,
		WorkspacePermissionReader: workspaces.NewAuthorizationService(repo),
	})
	accountsQuery := `{currentUser{calendarAccounts{id provider providerAccountId displayName email status refreshIntervalMinutes calendarsCount calendars{id accountId provider externalCalendarId displayName enabled}}}}`
	accountResult := graphRequest(t, handler, accountsQuery, nil, fixture.sessionID, fixture.userID)
	if accountResult["errors"] != nil {
		t.Fatalf("current user's calendar accounts=%#v", accountResult)
	}
	accountData := accountResult["data"].(map[string]any)["currentUser"].(map[string]any)["calendarAccounts"].([]any)
	if len(accountData) != 1 || accountData[0].(map[string]any)["calendarsCount"] != float64(2) || len(accountData[0].(map[string]any)["calendars"].([]any)) != 2 {
		t.Fatalf("calendar account projection=%#v", accountData)
	}
	if strings.Contains(fmt.Sprint(accountResult), "secret-token") {
		t.Fatal("calendar credentials leaked into GraphQL response")
	}
	linkMutation := `mutation($input:LinkCalDAVAccountInput!){linkCalDAVAccount(input:$input){id provider providerAccountId displayName email status calendarsCount}}`
	linked := graphRequest(t, handler, linkMutation, map[string]any{"input": map[string]any{"providerPresetId": "test", "username": "cal-user", "password": "app-password", "displayName": "Test calendar"}}, fixture.sessionID, fixture.userID)
	if linked["errors"] != nil {
		t.Fatalf("link CalDAV account=%#v", linked)
	}
	linkedAccount := linked["data"].(map[string]any)["linkCalDAVAccount"].(map[string]any)
	if linkedAccount["provider"] != "CalDAV" || linkedAccount["displayName"] != "Test calendar" || linkedAccount["calendarsCount"] != float64(1) || strings.Contains(fmt.Sprint(linked), "app-password") {
		t.Fatalf("linked CalDAV account projection=%#v", linkedAccount)
	}
	var storedCredential string
	if err := pool.QueryRow(ctx, `SELECT access_token FROM calendar_accounts WHERE id=$1`, linkedAccount["id"]).Scan(&storedCredential); err != nil || storedCredential == "app-password" {
		t.Fatalf("CalDAV credential was not encrypted: value=%q err=%v", storedCredential, err)
	}
	updateAccount := `mutation($id:String!,$minutes:Int!){updateCalendarAccount(accountId:$id,refreshIntervalMinutes:$minutes){id refreshIntervalMinutes calendarsCount}}`
	updated := graphRequest(t, handler, updateAccount, map[string]any{"id": "account-1", "minutes": 60}, fixture.sessionID, fixture.userID)
	if updated["errors"] != nil || updated["data"].(map[string]any)["updateCalendarAccount"].(map[string]any)["refreshIntervalMinutes"] != float64(60) {
		t.Fatalf("update calendar account interval=%#v", updated)
	}
	var nextSync string
	if err := pool.QueryRow(ctx, `SELECT next_sync_at::text FROM calendar_subscriptions WHERE id='sub-1'`).Scan(&nextSync); err != nil {
		t.Fatal(err)
	}
	if nextSync == "" {
		t.Fatal("refresh interval did not schedule the next sync")
	}
	otherUserUpdate := graphRequest(t, handler, updateAccount, map[string]any{"id": "account-1", "minutes": 5}, fixture.sessionID, fixture.sharedUserID)
	if otherUserUpdate["errors"] != nil || otherUserUpdate["data"].(map[string]any)["updateCalendarAccount"] != nil {
		t.Fatalf("another user updated the calendar account: %#v", otherUserUpdate)
	}
	mutation := `mutation($input:UpdateWorkspaceCalendarsInput!){updateWorkspaceCalendars(input:$input){id workspaceId createdByUserId enabled items{id subscriptionId sortOrder colorOverride enabled}}}`
	input := map[string]any{"workspaceId": fixture.workspaceID, "items": []any{
		map[string]any{"subscriptionId": "sub-1", "sortOrder": 1, "colorOverride": "#ff0000"},
		map[string]any{"subscriptionId": "sub-2", "sortOrder": 2},
	}}
	result := graphRequest(t, handler, mutation, map[string]any{"input": input}, fixture.sessionID, fixture.userID)
	if result["errors"] != nil {
		t.Fatalf("save workspace calendars=%#v", result)
	}
	calendar := result["data"].(map[string]any)["updateWorkspaceCalendars"].(map[string]any)
	if calendar["workspaceId"] != fixture.workspaceID || len(calendar["items"].([]any)) != 2 {
		t.Fatalf("saved workspace calendar=%#v", calendar)
	}

	badInput := map[string]any{"workspaceId": fixture.workspaceID, "items": []any{map[string]any{"subscriptionId": "sub-2"}, map[string]any{"subscriptionId": "missing"}}}
	failed := graphRequest(t, handler, mutation, map[string]any{"input": badInput}, fixture.sessionID, fixture.userID)
	if failed["errors"] == nil {
		t.Fatalf("invalid subscription update unexpectedly succeeded: %#v", failed)
	}

	query := `query($id:String!,$from:DateTime!,$to:DateTime!){workspace(id:$id){calendars{id items{subscriptionId sortOrder colorOverride} events(from:$from,to:$to){id title subscriptionId startAtUtc endAtUtc}}}}`
	vars := map[string]any{"id": fixture.workspaceID, "from": "2026-10-19T00:00:00Z", "to": "2026-10-20T00:00:00Z"}
	result = graphRequest(t, handler, query, vars, fixture.sessionID, fixture.userID)
	if result["errors"] != nil {
		t.Fatalf("read workspace calendars and events=%#v", result)
	}
	calendars := result["data"].(map[string]any)["workspace"].(map[string]any)["calendars"].([]any)
	if len(calendars) != 1 {
		t.Fatalf("workspace calendars=%#v", calendars)
	}
	calendar = calendars[0].(map[string]any)
	if len(calendar["items"].([]any)) != 2 || len(calendar["events"].([]any)) != 1 || calendar["events"].([]any)[0].(map[string]any)["id"] != "event-1" {
		t.Fatalf("workspace calendar view=%#v", calendar)
	}
	denied := graphRequest(t, handler, query, vars, fixture.sessionID, fixture.sharedUserID)
	if denied["errors"] == nil {
		t.Fatalf("collaborator calendar settings query unexpectedly succeeded: %#v", denied)
	}
	unlink := `mutation($id:String!){unlinkCalendarAccount(accountId:$id)}`
	otherUserUnlink := graphRequest(t, handler, unlink, map[string]any{"id": "account-1"}, fixture.sessionID, fixture.sharedUserID)
	if otherUserUnlink["errors"] != nil || otherUserUnlink["data"].(map[string]any)["unlinkCalendarAccount"] != false {
		t.Fatalf("another user unlinked the calendar account: %#v", otherUserUnlink)
	}
	unlinked := graphRequest(t, handler, unlink, map[string]any{"id": "account-1"}, fixture.sessionID, fixture.userID)
	if unlinked["errors"] != nil || unlinked["data"].(map[string]any)["unlinkCalendarAccount"] != true {
		t.Fatalf("unlink own calendar account=%#v", unlinked)
	}
	var remaining int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM calendar_accounts WHERE id='account-1')+(SELECT count(*) FROM calendar_subscriptions WHERE account_id='account-1')`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("unlink did not cascade calendar subscriptions: remaining=%d err=%v", remaining, err)
	}
}
