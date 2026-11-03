// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"testing"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/users"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

type googleCalendarLinkRecorder struct {
	userID      string
	redirectURI *string
	calls       int
}

func (r *googleCalendarLinkRecorder) CreateAuthorizationURL(_ context.Context, userID string, redirectURI *string) (string, error) {
	r.calls++
	r.userID = userID
	r.redirectURI = redirectURI
	return "https://accounts.google.test/oauth?state=fixture", nil
}

func (r *googleCalendarLinkRecorder) AvailableCalendarProviders(context.Context) ([]string, error) {
	return []string{"Google"}, nil
}

func TestLinkCalendarAccountGraphQLRequiresAuthenticationAndGoogleProvider(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	repo := workspaces.NewRepository(pool)
	userService := users.NewService(users.NewRepository(pool), workspaces.NewAuthorizationService(repo))
	linker := &googleCalendarLinkRecorder{}
	handler := NewHandlerWithServices(config.Config{Deployment: "cloud"}, auth.NewSessionResolver(pool), userService, repo, nil, nil, nil, nil, HandlerDependencies{
		GoogleCalendarAccountLinker: linker,
		CalendarProviderCatalog:     linker,
	})
	mutation := `mutation($input:LinkCalendarAccountInput!){linkCalendarAccount(input:$input)}`
	redirect := "https://notes.example.test/workspace/settings?tab=calendar"
	variables := map[string]any{"input": map[string]any{"provider": "Google", "redirectUri": redirect}}
	result := graphRequest(t, handler, mutation, variables, fixture.sessionID, fixture.userID)
	if result["errors"] != nil || result["data"].(map[string]any)["linkCalendarAccount"] != "https://accounts.google.test/oauth?state=fixture" {
		t.Fatalf("link calendar account=%#v", result)
	}
	if linker.calls != 1 || linker.userID != fixture.userID || linker.redirectURI == nil || *linker.redirectURI != redirect {
		t.Fatalf("Google linker input: calls=%d user=%q redirect=%v", linker.calls, linker.userID, linker.redirectURI)
	}
	serverConfig := graphRequest(t, handler, `{serverConfig{calendarProviders}}`, nil, fixture.sessionID, fixture.userID)
	if serverConfig["errors"] != nil {
		t.Fatalf("calendar provider availability=%#v", serverConfig)
	}
	serverConfigData, ok := serverConfig["data"].(map[string]any)
	if !ok {
		t.Fatalf("calendar provider availability has no data: %#v", serverConfig)
	}
	configData, ok := serverConfigData["serverConfig"].(map[string]any)
	if !ok {
		t.Fatalf("calendar provider availability has no serverConfig: %#v", serverConfig)
	}
	providers, ok := configData["calendarProviders"].([]any)
	if !ok || len(providers) != 1 || providers[0] != "Google" {
		t.Fatalf("calendar provider availability=%#v", serverConfig)
	}
	unauthenticated := graphRequest(t, handler, mutation, variables, "", "")
	if unauthenticated["errors"] == nil || linker.calls != 1 {
		t.Fatalf("anonymous calendar link=%#v calls=%d", unauthenticated, linker.calls)
	}
	nonOAuthProvider := graphRequest(t, handler, mutation, map[string]any{"input": map[string]any{"provider": "CalDAV"}}, fixture.sessionID, fixture.userID)
	if nonOAuthProvider["errors"] == nil || linker.calls != 1 {
		t.Fatalf("CalDAV OAuth link=%#v calls=%d", nonOAuthProvider, linker.calls)
	}
}
