// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gqlhandler "github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/transport"
	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/graphql/model"
	"github.com/fydeos/fydeos-notes-server/internal/notifications"
	"github.com/fydeos/fydeos-notes-server/internal/users"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

type notificationResolverFake struct {
	items       []notifications.Notification
	recipients  map[string]string
	createdUser string
	createdWork string
	createdBy   string
	createdDoc  map[string]any
	listUserID  string
	listAfter   *time.Time
	listFirst   int
	listOffset  int
	markedIDs   []string
	markAllUser string
}

func (f *notificationResolverFake) CreateMention(_ context.Context, userID, workspaceID, createdByUserID string, doc map[string]any) (string, error) {
	f.createdUser, f.createdWork, f.createdBy, f.createdDoc = userID, workspaceID, createdByUserID, doc
	if f.recipients == nil {
		f.recipients = make(map[string]string)
	}
	f.recipients["mention-created"] = userID
	f.items = append(f.items, notifications.Notification{
		ID: "mention-created", Type: "Mention", CreatedAt: time.Now().UTC(), Body: map[string]any{
			"workspaceId": workspaceID, "createdByUserId": createdByUserID, "doc": doc,
		},
	})
	return "mention-created", nil
}

func (f *notificationResolverFake) CountUnread(context.Context, string) (int, error) {
	count := 0
	for _, item := range f.items {
		if !item.Read {
			count++
		}
	}
	return count, nil
}

func (f *notificationResolverFake) ListUnread(_ context.Context, userID string, after *time.Time, first, offset int) ([]notifications.Notification, error) {
	f.listUserID, f.listAfter, f.listFirst, f.listOffset = userID, after, first, offset
	items := make([]notifications.Notification, 0, len(f.items))
	for _, item := range f.items {
		if item.Read || (after != nil && !item.CreatedAt.Before(*after)) {
			continue
		}
		items = append(items, item)
	}
	if offset >= len(items) {
		return []notifications.Notification{}, nil
	}
	items = items[offset:]
	if len(items) > first {
		items = items[:first]
	}
	return items, nil
}

func (f *notificationResolverFake) MarkAsRead(_ context.Context, userID, notificationID string) error {
	if f.recipients[notificationID] != userID {
		return notifications.ErrNotFound
	}
	f.markedIDs = append(f.markedIDs, notificationID)
	for i := range f.items {
		if f.items[i].ID == notificationID {
			f.items[i].Read = true
		}
	}
	return nil
}

func (f *notificationResolverFake) MarkAllAsRead(_ context.Context, userID string) error {
	f.markAllUser = userID
	for i := range f.items {
		f.items[i].Read = true
	}
	return nil
}

type notificationCountPublisherFake struct {
	userID string
	count  int
	reason string
	calls  int
	events []notificationCountEvent
}

type notificationCountEvent struct {
	userID string
	count  int
	reason string
}

func (f *notificationCountPublisherFake) PublishNotificationCountChanged(userID string, count int, reason string) {
	f.userID, f.count, f.reason = userID, count, reason
	f.calls++
	f.events = append(f.events, notificationCountEvent{userID: userID, count: count, reason: reason})
}

type notificationUserServiceFake struct{ user *users.User }

func (f notificationUserServiceFake) Get(context.Context, string) (*users.User, error) {
	return f.user, nil
}
func (f notificationUserServiceFake) GetPublic(context.Context, string) (*users.User, error) {
	return f.user, nil
}
func (f notificationUserServiceFake) GetVisibleByEmail(context.Context, string, string) (*users.User, error) {
	return nil, nil
}
func (f notificationUserServiceFake) UpdateProfile(context.Context, string, *string) (*users.User, error) {
	return f.user, nil
}
func (f notificationUserServiceFake) UpdateAvatar(context.Context, string, *string) (*users.User, error) {
	return f.user, nil
}
func (f notificationUserServiceFake) GetSettings(context.Context, string) (users.UserSettings, error) {
	return users.UserSettings{ReceiveInvitationEmail: true, ReceiveMentionEmail: true, ReceiveCommentEmail: true}, nil
}
func (f notificationUserServiceFake) UpdateSettings(_ context.Context, _ string, update users.UserSettingsUpdate) (users.UserSettings, error) {
	settings := users.UserSettings{ReceiveInvitationEmail: true, ReceiveMentionEmail: true, ReceiveCommentEmail: true}
	if update.ReceiveInvitationEmail != nil {
		settings.ReceiveInvitationEmail = *update.ReceiveInvitationEmail
	}
	if update.ReceiveMentionEmail != nil {
		settings.ReceiveMentionEmail = *update.ReceiveMentionEmail
	}
	if update.ReceiveCommentEmail != nil {
		settings.ReceiveCommentEmail = *update.ReceiveCommentEmail
	}
	return settings, nil
}

func TestNotificationGraphQLHTTPListsCurrentUsersUnreadNotifications(t *testing.T) {
	createdAt := time.Date(2026, 10, 17, 3, 0, 0, 123_000_000, time.UTC)
	repository := &notificationResolverFake{items: []notifications.Notification{{
		ID: "notification-1", Level: "Default", Type: "Mention", CreatedAt: createdAt,
		UpdatedAt: createdAt, Body: map[string]any{"workspaceId": "workspace-1", "type": "Mention"},
	}}, recipients: map[string]string{"notification-1": "recipient-1"}}
	resolver := &Resolver{
		UserService:   notificationUserServiceFake{user: &users.User{ID: "recipient-1", Name: "Recipient", Email: "recipient@example.test"}},
		Notifications: repository,
	}
	server := gqlhandler.New(NewExecutableSchema(Config{Resolvers: resolver}))
	server.AddTransport(transport.POST{})
	body := `{"query":"query { currentUser { notifications(pagination: {first: 10, offset: 0}) { totalCount edges { cursor node { id level type read createdAt body } } pageInfo { hasNextPage hasPreviousPage startCursor endCursor } } } }"}`
	request := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(auth.WithPrincipal(request.Context(), &auth.Principal{User: auth.User{ID: "recipient-1"}}))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GraphQL HTTP status = %d, body=%s", response.Code, response.Body.String())
	}
	var result struct {
		Data struct {
			CurrentUser struct {
				Notifications struct {
					TotalCount int `json:"totalCount"`
					Edges      []struct {
						Cursor string `json:"cursor"`
						Node   struct {
							ID        string         `json:"id"`
							Level     string         `json:"level"`
							Type      string         `json:"type"`
							Read      bool           `json:"read"`
							CreatedAt string         `json:"createdAt"`
							Body      map[string]any `json:"body"`
						} `json:"node"`
					} `json:"edges"`
					PageInfo struct {
						HasNextPage     bool `json:"hasNextPage"`
						HasPreviousPage bool `json:"hasPreviousPage"`
					} `json:"pageInfo"`
				} `json:"notifications"`
			} `json:"currentUser"`
		} `json:"data"`
		Errors []map[string]any `json:"errors"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode GraphQL response: %v; body=%s", err, response.Body.String())
	}
	if len(result.Errors) != 0 {
		t.Fatalf("GraphQL errors = %#v", result.Errors)
	}
	connection := result.Data.CurrentUser.Notifications
	if repository.listUserID != "recipient-1" || repository.listFirst != 10 || repository.listOffset != 0 {
		t.Fatalf("repository list arguments = user %q first %d offset %d", repository.listUserID, repository.listFirst, repository.listOffset)
	}
	if connection.TotalCount != 1 || len(connection.Edges) != 1 || connection.PageInfo.HasNextPage || connection.PageInfo.HasPreviousPage {
		t.Fatalf("notification connection = %+v", connection)
	}
	edge := connection.Edges[0]
	wantCursor := base64.StdEncoding.EncodeToString([]byte("2026-10-17T03:00:00.123Z"))
	if edge.Cursor != wantCursor || edge.Node.ID != "notification-1" || edge.Node.Level != "Default" || edge.Node.Type != "Mention" || edge.Node.Read {
		t.Fatalf("notification edge = %+v", edge)
	}
	if edge.Node.Body["workspaceId"] != "workspace-1" || edge.Node.Body["type"] != "Mention" {
		t.Fatalf("notification body = %#v", edge.Node.Body)
	}
}

func TestMentionUserGraphQLCreatesNotificationAndPublishesCount(t *testing.T) {
	repository := &notificationResolverFake{}
	publisher := &notificationCountPublisherFake{}
	authorizer := &commentDocumentAuthorizerFake{accessByUser: map[string]workspaces.DocumentAuthorization{
		"actor-1":     {Update: true},
		"recipient-1": {Read: true},
	}}
	resolver := &Resolver{
		UserService:           notificationUserServiceFake{user: &users.User{ID: "actor-1", Name: "Actor", Email: "actor@example.test"}},
		Notifications:         repository,
		NotificationAuth:      authorizer,
		NotificationPublisher: publisher,
	}
	server := gqlhandler.New(NewExecutableSchema(Config{Resolvers: resolver}))
	server.AddTransport(transport.POST{})
	body := `{"query":"mutation Mention($input: MentionInput!) { mentionUser(input: $input) }","variables":{"input":{"userId":" recipient-1 ","workspaceId":" workspace-1 ","doc":{"id":" doc-1 ","title":"  Draft title  ","mode":"page","blockId":" block-1 "}}}}`
	request := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(auth.WithPrincipal(request.Context(), &auth.Principal{User: auth.User{ID: "actor-1"}}))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GraphQL HTTP status = %d, body=%s", response.Code, response.Body.String())
	}
	var result struct {
		Data struct {
			MentionUser string `json:"mentionUser"`
		} `json:"data"`
		Errors []map[string]any `json:"errors"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode mention response: %v; body=%s", err, response.Body.String())
	}
	if len(result.Errors) != 0 || result.Data.MentionUser != "mention-created" {
		t.Fatalf("mention response = %+v, body=%s", result, response.Body.String())
	}
	if len(authorizer.calls) != 2 || authorizer.calls[0] != "actor-1" || authorizer.calls[1] != "recipient-1" {
		t.Fatalf("mention authorization order = %#v", authorizer.calls)
	}
	if repository.createdUser != "recipient-1" || repository.createdWork != "workspace-1" || repository.createdBy != "actor-1" ||
		repository.createdDoc["id"] != "doc-1" || repository.createdDoc["title"] != "Draft title" || repository.createdDoc["mode"] != "page" || repository.createdDoc["blockId"] != "block-1" {
		t.Fatalf("created mention arguments = user %q workspace %q actor %q doc %#v", repository.createdUser, repository.createdWork, repository.createdBy, repository.createdDoc)
	}
	if publisher.userID != "recipient-1" || publisher.count != 1 || publisher.reason != "created" || publisher.calls != 1 {
		t.Fatalf("mention count event = %+v", publisher)
	}
}

func TestMentionUserRejectsSelfMentionsAndPermissionFailures(t *testing.T) {
	baseInput := model.MentionInput{UserID: "recipient-1", WorkspaceID: "workspace-1", Doc: &model.MentionDocInput{ID: "doc-1", Title: "Draft", Mode: model.DocModePage}}
	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{User: auth.User{ID: "actor-1"}})
	for _, test := range []struct {
		name        string
		input       model.MentionInput
		permissions map[string]workspaces.DocumentAuthorization
		want        error
	}{
		{name: "self mention", input: model.MentionInput{UserID: "actor-1", WorkspaceID: "workspace-1", Doc: baseInput.Doc}, permissions: map[string]workspaces.DocumentAuthorization{}, want: ErrMentionUserOneselfDenied},
		{name: "actor cannot update", input: baseInput, permissions: map[string]workspaces.DocumentAuthorization{"actor-1": {Read: true}, "recipient-1": {Read: true}}, want: ErrMentionDocumentUpdateDenied},
		{name: "recipient cannot read", input: baseInput, permissions: map[string]workspaces.DocumentAuthorization{"actor-1": {Update: true}, "recipient-1": {}}, want: ErrMentionUserDocAccessDenied},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := &notificationResolverFake{}
			authorizer := &commentDocumentAuthorizerFake{accessByUser: test.permissions}
			resolver := &mutationResolver{Resolver: &Resolver{Notifications: repository, NotificationAuth: authorizer}}
			if _, err := resolver.MentionUser(ctx, test.input); !errors.Is(err, test.want) {
				t.Fatalf("mention error = %v, want %v", err, test.want)
			}
			if repository.createdUser != "" {
				t.Fatal("notification was created after a denied mention")
			}
		})
	}
	tooLong := strings.Repeat("x", 256)
	resolver := &mutationResolver{Resolver: &Resolver{Notifications: &notificationResolverFake{}, NotificationAuth: &commentDocumentAuthorizerFake{}}}
	invalid := baseInput
	invalid.Doc = &model.MentionDocInput{ID: "doc-1", Title: tooLong, Mode: model.DocModePage}
	if _, err := resolver.MentionUser(ctx, invalid); !errors.Is(err, ErrInvalidMentionInput) {
		t.Fatalf("invalid mention input error = %v", err)
	}
	denied := presentGraphQLError(ctx, &MentionUserDocAccessDeniedError{DocID: "doc-1"})
	if denied.Message != "Mentioned user can not access doc doc-1." || denied.Extensions["name"] != "MENTION_USER_DOC_ACCESS_DENIED" || denied.Extensions["type"] != "NO_PERMISSION" {
		t.Fatalf("legacy mention permission error = %+v", denied)
	}
}

func TestNotificationMutationsScopeActorAndPublishUpdatedCounts(t *testing.T) {
	createdAt := time.Date(2026, 10, 17, 3, 0, 0, 0, time.UTC)
	repository := &notificationResolverFake{
		items: []notifications.Notification{
			{ID: "notification-1", CreatedAt: createdAt},
			{ID: "notification-2", CreatedAt: createdAt},
		},
		recipients: map[string]string{"notification-1": "recipient-1", "notification-2": "recipient-1"},
	}
	publisher := &notificationCountPublisherFake{}
	resolver := &mutationResolver{Resolver: &Resolver{Notifications: repository, NotificationPublisher: publisher}}
	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{User: auth.User{ID: "recipient-1"}})
	if ok, err := resolver.ReadNotification(ctx, "notification-1"); err != nil || !ok {
		t.Fatalf("readNotification = %v, %v", ok, err)
	}
	if publisher.userID != "recipient-1" || publisher.count != 1 || publisher.reason != "read" || publisher.calls != 1 {
		t.Fatalf("read notification count event = %+v", publisher)
	}
	if ok, err := resolver.ReadAllNotifications(ctx); err != nil || !ok {
		t.Fatalf("readAllNotifications = %v, %v", ok, err)
	}
	if repository.markAllUser != "recipient-1" || publisher.userID != "recipient-1" || publisher.count != 0 || publisher.reason != "read-all" || publisher.calls != 2 {
		t.Fatalf("read-all notification state/event = user %q publisher %+v", repository.markAllUser, publisher)
	}
	if _, err := resolver.ReadNotification(context.Background(), "notification-1"); !errors.Is(err, ErrAuthenticationRequired) {
		t.Fatalf("anonymous read notification error = %v", err)
	}
}

func TestNotificationPaginationRejectsInvalidCursorAndOffsetCombination(t *testing.T) {
	resolver := &userTypeResolver{Resolver: &Resolver{Notifications: &notificationResolverFake{}}}
	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{User: auth.User{ID: "recipient-1"}})
	offset := 1
	badCursor := "%%%"
	for _, pagination := range []model.PaginationInput{
		{After: &badCursor},
		{After: stringPointer(base64.StdEncoding.EncodeToString([]byte("2026-10-17T03:00:00.000Z"))), Offset: &offset},
	} {
		if _, err := resolver.Notifications(ctx, &model.UserType{ID: "other-user"}, pagination); !errors.Is(err, ErrInvalidNotificationPagination) {
			t.Fatalf("invalid pagination %+v error = %v", pagination, err)
		}
	}
}
