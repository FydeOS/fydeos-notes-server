// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gqlhandler "github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/transport"
	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/users"
)

type userSettingsServiceFake struct {
	settings  users.UserSettings
	avatarURL *string
}

func (s *userSettingsServiceFake) Get(context.Context, string) (*users.User, error) {
	return &users.User{ID: "user-1", Name: "User", Email: "user@example.test"}, nil
}

func (*userSettingsServiceFake) GetPublic(context.Context, string) (*users.User, error) {
	return nil, nil
}

func (*userSettingsServiceFake) GetVisibleByEmail(context.Context, string, string) (*users.User, error) {
	return nil, nil
}

func (*userSettingsServiceFake) UpdateProfile(context.Context, string, *string) (*users.User, error) {
	return nil, nil
}

func (s *userSettingsServiceFake) UpdateAvatar(_ context.Context, _ string, avatarURL *string) (*users.User, error) {
	s.avatarURL = avatarURL
	return &users.User{ID: "user-1", Name: "User", Email: "user@example.test", AvatarURL: avatarURL}, nil
}

func (s *userSettingsServiceFake) GetSettings(context.Context, string) (users.UserSettings, error) {
	return s.settings, nil
}

func (s *userSettingsServiceFake) UpdateSettings(_ context.Context, _ string, update users.UserSettingsUpdate) (users.UserSettings, error) {
	if update.ReceiveInvitationEmail != nil {
		s.settings.ReceiveInvitationEmail = *update.ReceiveInvitationEmail
	}
	if update.ReceiveMentionEmail != nil {
		s.settings.ReceiveMentionEmail = *update.ReceiveMentionEmail
	}
	if update.ReceiveCommentEmail != nil {
		s.settings.ReceiveCommentEmail = *update.ReceiveCommentEmail
	}
	return s.settings, nil
}

func TestUserSettingsGraphQLPartialUpdateNullAndAuthentication(t *testing.T) {
	service := &userSettingsServiceFake{settings: users.UserSettings{
		ReceiveInvitationEmail: true,
		ReceiveMentionEmail:    true,
		ReceiveCommentEmail:    true,
	}}
	publisher := &userSettingsPublisherRecorder{}
	server := gqlhandler.New(NewExecutableSchema(Config{Resolvers: &Resolver{
		UserService:           service,
		UserSettingsPublisher: publisher,
	}}))
	server.AddTransport(transport.POST{})
	principal := &auth.Principal{User: auth.User{ID: "user-1"}}

	mutation := `mutation updateUserSettings($input: UpdateUserSettingsInput!) { updateSettings(input: $input) }`
	first := serveUserSettingsGraphQL(t, server, mutation, map[string]any{"input": map[string]any{
		"receiveInvitationEmail": false,
	}}, principal)
	if first["errors"] != nil || graphData(t, first)["updateSettings"] != true {
		t.Fatalf("update result = %#v", first)
	}
	if service.settings != (users.UserSettings{ReceiveInvitationEmail: false, ReceiveMentionEmail: true, ReceiveCommentEmail: true}) {
		t.Fatalf("partial update lost defaults or existing values: %+v", service.settings)
	}
	if len(publisher.userIDs) != 1 || publisher.userIDs[0] != "user-1" {
		t.Fatalf("published changes = %#v", publisher.userIDs)
	}

	nullUpdate := serveUserSettingsGraphQL(t, server, mutation, map[string]any{"input": map[string]any{
		"receiveMentionEmail": nil,
	}}, principal)
	if !strings.Contains(graphErrors(nullUpdate), "Expected boolean, received null") {
		t.Fatalf("explicit null result = %#v", nullUpdate)
	}
	if len(publisher.userIDs) != 1 {
		t.Fatalf("invalid update published changes: %#v", publisher.userIDs)
	}

	unauthenticated := serveUserSettingsGraphQL(t, server, mutation, map[string]any{"input": map[string]any{
		"receiveMentionEmail": false,
	}}, nil)
	if !strings.Contains(graphErrors(unauthenticated), ErrAuthenticationRequired.Error()) {
		t.Fatalf("unauthenticated result = %#v", unauthenticated)
	}
	if len(publisher.userIDs) != 1 {
		t.Fatalf("unauthenticated update published changes: %#v", publisher.userIDs)
	}

	query := `query getUserSettings { currentUser { settings { receiveInvitationEmail receiveMentionEmail receiveCommentEmail } } }`
	result := serveUserSettingsGraphQL(t, server, query, nil, principal)
	want := users.UserSettings{ReceiveInvitationEmail: false, ReceiveMentionEmail: true, ReceiveCommentEmail: true}
	if got := graphUserSettings(t, result); got != want {
		t.Fatalf("settings query = %+v, want %+v", got, want)
	}
}

func serveUserSettingsGraphQL(t *testing.T, server *gqlhandler.Server, query string, variables map[string]any, principal *auth.Principal) map[string]any {
	t.Helper()
	body, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if principal != nil {
		request = request.WithContext(auth.WithPrincipal(request.Context(), principal))
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	var result map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode GraphQL response: %v; body=%s", err, response.Body.String())
	}
	return result
}

var _ UserService = (*userSettingsServiceFake)(nil)
