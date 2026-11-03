// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/users"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
	"github.com/jackc/pgx/v5/pgxpool"
)

type userSettingsPublisherRecorder struct {
	userIDs []string
}

func (p *userSettingsPublisherRecorder) PublishUserSettingsChanged(userID string) {
	p.userIDs = append(p.userIDs, userID)
}

func TestUserSettingsGraphQLCompatibility(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fixture := seedGraphQLFixture(t, ctx, pool)
	handler, publisher := userSettingsHandler(pool)

	query := `query getUserSettings { currentUser { settings { receiveInvitationEmail receiveMentionEmail receiveCommentEmail } } }`
	result := graphRequest(t, handler, query, nil, fixture.sessionID, fixture.userID)
	settings := graphUserSettings(t, result)
	if settings != (users.UserSettings{ReceiveInvitationEmail: true, ReceiveMentionEmail: true, ReceiveCommentEmail: true}) {
		t.Fatalf("default settings = %+v", settings)
	}

	mutation := `mutation updateUserSettings($input: UpdateUserSettingsInput!) { updateSettings(input: $input) }`
	first := graphRequest(t, handler, mutation, map[string]any{"input": map[string]any{
		"receiveInvitationEmail": false,
		"receiveCommentEmail":    false,
	}}, fixture.sessionID, fixture.userID)
	if first["errors"] != nil || graphData(t, first)["updateSettings"] != true {
		t.Fatalf("first update result = %#v", first)
	}
	second := graphRequest(t, handler, mutation, map[string]any{"input": map[string]any{
		"receiveMentionEmail": false,
	}}, fixture.sessionID, fixture.userID)
	if second["errors"] != nil || graphData(t, second)["updateSettings"] != true {
		t.Fatalf("second update result = %#v", second)
	}
	result = graphRequest(t, handler, query, nil, fixture.sessionID, fixture.userID)
	settings = graphUserSettings(t, result)
	want := users.UserSettings{ReceiveInvitationEmail: false, ReceiveMentionEmail: false, ReceiveCommentEmail: false}
	if settings != want {
		t.Fatalf("persisted settings = %+v, want %+v", settings, want)
	}
	if len(publisher.userIDs) != 2 || publisher.userIDs[0] != fixture.userID || publisher.userIDs[1] != fixture.userID {
		t.Fatalf("published user settings changes = %#v", publisher.userIDs)
	}

	invalid := graphRequest(t, handler, mutation, map[string]any{"input": map[string]any{
		"receiveInvitationEmail": nil,
	}}, fixture.sessionID, fixture.userID)
	if !strings.Contains(graphErrors(invalid), "Expected boolean, received null") {
		t.Fatalf("explicit null was not rejected: %#v", invalid)
	}
	unauthenticated := graphRequest(t, handler, mutation, map[string]any{"input": map[string]any{
		"receiveInvitationEmail": true,
	}}, "", "")
	if !strings.Contains(graphErrors(unauthenticated), ErrAuthenticationRequired.Error()) {
		t.Fatalf("unauthenticated update was not rejected: %#v", unauthenticated)
	}
	if len(publisher.userIDs) != 2 {
		t.Fatalf("invalid or unauthenticated update published changes: %#v", publisher.userIDs)
	}
}

func userSettingsHandler(pool *pgxpool.Pool) (http.Handler, *userSettingsPublisherRecorder) {
	sessionResolver := auth.NewSessionResolver(pool)
	workspaceRepository := workspaces.NewRepository(pool)
	userRepository := users.NewRepository(pool)
	service := users.NewService(userRepository, workspaces.NewAuthorizationService(workspaceRepository))
	publisher := &userSettingsPublisherRecorder{}
	handler := NewHandlerWithServices(
		config.Config{Version: "0.27.5", Deployment: "selfhosted", Host: "localhost", Port: 3010},
		sessionResolver,
		service,
		workspaceRepository,
		auth.NewReadLimiter(auth.DefaultReadConcurrency),
		nil,
		nil,
		nil,
		HandlerDependencies{UserSettings: publisher},
	)
	return handler, publisher
}

func graphData(t *testing.T, result map[string]any) map[string]any {
	t.Helper()
	data, ok := result["data"].(map[string]any)
	if !ok {
		t.Fatalf("GraphQL data missing: %#v", result)
	}
	return data
}

func graphErrors(result map[string]any) string {
	data, _ := result["errors"].([]any)
	var messages []string
	for _, value := range data {
		if item, ok := value.(map[string]any); ok {
			if message, ok := item["message"].(string); ok {
				messages = append(messages, message)
			}
		}
	}
	return strings.Join(messages, "\n")
}

func graphUserSettings(t *testing.T, result map[string]any) users.UserSettings {
	t.Helper()
	currentUser, ok := graphData(t, result)["currentUser"].(map[string]any)
	if !ok {
		t.Fatalf("currentUser missing: %#v", result)
	}
	settings, ok := currentUser["settings"].(map[string]any)
	if !ok {
		t.Fatalf("settings missing: %#v", result)
	}
	return users.UserSettings{
		ReceiveInvitationEmail: settings["receiveInvitationEmail"] == true,
		ReceiveMentionEmail:    settings["receiveMentionEmail"] == true,
		ReceiveCommentEmail:    settings["receiveCommentEmail"] == true,
	}
}
