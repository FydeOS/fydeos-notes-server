// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/comments"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/graphql/model"
	"github.com/fydeos/fydeos-notes-server/internal/notifications"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

type commentReadRepositoryFake struct {
	total       int
	items       []comments.Comment
	changes     []comments.Change
	listAfter   *int64
	listLimit   int
	changeAfter *time.Time
	replyAfter  *time.Time
	changeLimit int
	listCalls   int
}

type commentWriteRepositoryFake struct {
	comment       callsCommentCreate
	commentResult comments.Comment
	replyResult   comments.Reply
	createErr     error
}

type commentChangePublisherFake struct {
	workspaceID string
	docID       string
	calls       int
}

func (f *commentChangePublisherFake) PublishCommentChanged(workspaceID, docID string) {
	f.workspaceID, f.docID = workspaceID, docID
	f.calls++
}

type callsCommentCreate struct {
	actorID      string
	workspaceID  string
	docID        string
	content      map[string]any
	notification comments.NotificationInput
}

func (f *commentWriteRepositoryFake) CreateComment(_ context.Context, actorID, workspaceID, docID string, content map[string]any, notification comments.NotificationInput) (comments.Comment, error) {
	f.comment = callsCommentCreate{actorID: actorID, workspaceID: workspaceID, docID: docID, content: content, notification: notification}
	return f.commentResult, f.createErr
}

func (f *commentWriteRepositoryFake) UpdateComment(context.Context, string, string, map[string]any) (comments.Comment, error) {
	return f.commentResult, nil
}
func (f *commentWriteRepositoryFake) ResolveComment(context.Context, string, string, bool) (comments.Comment, error) {
	return f.commentResult, nil
}
func (f *commentWriteRepositoryFake) DeleteComment(context.Context, string, string) (comments.Comment, error) {
	return f.commentResult, nil
}
func (f *commentWriteRepositoryFake) CreateReply(context.Context, string, string, map[string]any, comments.NotificationInput) (comments.Reply, error) {
	return f.replyResult, nil
}
func (f *commentWriteRepositoryFake) UpdateReply(context.Context, string, string, map[string]any) (comments.Reply, error) {
	return f.replyResult, nil
}
func (f *commentWriteRepositoryFake) DeleteReply(context.Context, string, string) (comments.Reply, error) {
	return f.replyResult, nil
}

func (f *commentReadRepositoryFake) Count(context.Context, string, string) (int, error) {
	return f.total, nil
}

func (f *commentReadRepositoryFake) List(_ context.Context, _, _ string, after *int64, limit int) ([]comments.Comment, error) {
	f.listCalls++
	f.listAfter, f.listLimit = after, limit
	return f.items, nil
}

func (f *commentReadRepositoryFake) ListChanges(_ context.Context, _, _ string, commentAfter, replyAfter *time.Time, limit int) ([]comments.Change, error) {
	f.changeAfter, f.replyAfter, f.changeLimit = commentAfter, replyAfter, limit
	return f.changes, nil
}

type commentDocumentAuthorizerFake struct {
	access       workspaces.DocumentAuthorization
	accessByUser map[string]workspaces.DocumentAuthorization
	userID       string
	workspaceID  string
	docID        string
	called       bool
	calls        []string
}

type commentWorkspaceRepositoryFake struct {
	workspace workspaces.Workspace
}

func (f commentWorkspaceRepositoryFake) CreateForUser(context.Context, string) (workspaces.Workspace, error) {
	return f.workspace, nil
}

func (f commentWorkspaceRepositoryFake) ListForUser(context.Context, string) ([]workspaces.Workspace, error) {
	return []workspaces.Workspace{f.workspace}, nil
}

func (f commentWorkspaceRepositoryFake) GetForUser(_ context.Context, _, _ string) (workspaces.Workspace, error) {
	return f.workspace, nil
}

func (commentWorkspaceRepositoryFake) ReadPresentationMetadata(context.Context, string) (workspaces.PresentationMetadata, error) {
	return workspaces.PresentationMetadata{}, nil
}

func (commentWorkspaceRepositoryFake) ReadQuota(context.Context, string) (workspaces.Quota, error) {
	return workspaces.Quota{}, nil
}

func (commentWorkspaceRepositoryFake) HasCommonActiveWorkspace(context.Context, string, string) (bool, error) {
	return false, nil
}

func (commentWorkspaceRepositoryFake) UpsertRootDocument(context.Context, string, string, []byte) error {
	return nil
}

func (f commentWorkspaceRepositoryFake) UpdateForUser(context.Context, string, string, workspaces.SettingsUpdate) (workspaces.Workspace, error) {
	return f.workspace, nil
}

func (commentWorkspaceRepositoryFake) DeleteForUser(context.Context, string, string) ([]string, error) {
	return nil, nil
}

func (f *commentDocumentAuthorizerFake) AuthorizeDocument(_ context.Context, userID, workspaceID, docID string) (workspaces.DocumentAuthorization, error) {
	f.called, f.userID, f.workspaceID, f.docID = true, userID, workspaceID, docID
	f.calls = append(f.calls, userID)
	if access, ok := f.accessByUser[userID]; ok {
		return access, nil
	}
	return f.access, nil
}

func TestCommentsResolverAppliesReadAuthorizationAndLegacyCursorPagination(t *testing.T) {
	updated := time.Date(2026, 10, 17, 3, 0, 0, 0, time.UTC)
	after, _ := base64.StdEncoding.DecodeString(`eyJzaWQiOjk5LCJjb21tZW50VXBkYXRlZEF0IjoiMjAyNi0xMC0xN1QwMzowMDowMFoifQ==`)
	afterValue := base64.StdEncoding.EncodeToString(after)
	first := 1
	repository := &commentReadRepositoryFake{
		total: 3,
		items: []comments.Comment{{
			SID: 101, ID: "comment-1", WorkspaceID: "workspace-1", DocID: "doc-1",
			Content: map[string]any{"text": "comment"}, CreatedAt: updated, UpdatedAt: updated,
			User:    comments.User{ID: "author-1", Name: "Author", AvatarURL: stringPointer("https://example.test/avatar.png")},
			Replies: []comments.Reply{{ID: "reply-1", CommentID: "comment-1", Content: map[string]any{"text": "reply"}, CreatedAt: updated, UpdatedAt: updated, User: comments.User{ID: "author-2", Name: "Replier"}}},
		}},
	}
	authorizer := &commentDocumentAuthorizerFake{access: workspaces.DocumentAuthorization{Read: true}}
	resolver := &workspaceTypeResolver{Resolver: &Resolver{CommentReader: repository, CommentAuth: authorizer}}
	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{User: auth.User{ID: "viewer-1"}})
	result, err := resolver.Comments(ctx, &model.WorkspaceType{ID: "workspace-1"}, "doc-1", &model.PaginationInput{After: &afterValue, First: &first})
	if err != nil {
		t.Fatal(err)
	}
	if !authorizer.called || authorizer.userID != "viewer-1" || authorizer.workspaceID != "workspace-1" || authorizer.docID != "doc-1" {
		t.Fatalf("authorization input = %+v", authorizer)
	}
	if repository.listCalls != 1 || repository.listAfter == nil || *repository.listAfter != 99 || repository.listLimit != 1 {
		t.Fatalf("repository pagination = after %v, limit %d, calls %d", repository.listAfter, repository.listLimit, repository.listCalls)
	}
	if result.TotalCount != 3 || len(result.Edges) != 1 || result.PageInfo.HasPreviousPage || !result.PageInfo.HasNextPage {
		t.Fatalf("connection = %+v", result)
	}
	comment := result.Edges[0].Node
	if comment.ID != "comment-1" || comment.User.ID != "author-1" || comment.User.AvatarURL == nil || len(comment.Replies) != 1 || comment.Replies[0].User.ID != "author-2" {
		t.Fatalf("comment projection = %+v", comment)
	}
	var start struct {
		SID              int64     `json:"sid"`
		CommentUpdatedAt time.Time `json:"commentUpdatedAt"`
		ReplyUpdatedAt   time.Time `json:"replyUpdatedAt"`
	}
	decoded, err := base64.StdEncoding.DecodeString(*result.PageInfo.StartCursor)
	if err != nil || json.Unmarshal(decoded, &start) != nil || start.SID != 101 || !start.CommentUpdatedAt.Equal(updated) || !start.ReplyUpdatedAt.Equal(updated) {
		t.Fatalf("start cursor = %q decoded=%+v err=%v", *result.PageInfo.StartCursor, start, err)
	}
}

func TestCommentsResolverDeniesUnreadableAndAnonymousRequests(t *testing.T) {
	repository := &commentReadRepositoryFake{total: 1}
	authorizer := &commentDocumentAuthorizerFake{access: workspaces.DocumentAuthorization{Read: false}}
	resolver := &workspaceTypeResolver{Resolver: &Resolver{CommentReader: repository, CommentAuth: authorizer}}
	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{User: auth.User{ID: "reader-1"}})
	if _, err := resolver.Comments(ctx, &model.WorkspaceType{ID: "workspace-1"}, "doc-1", nil); !errors.Is(err, ErrCommentReadForbidden) {
		t.Fatalf("unreadable document error = %v", err)
	}
	if repository.listCalls != 0 {
		t.Fatal("comment rows were loaded before read authorization")
	}
	if _, err := resolver.Comments(context.Background(), &model.WorkspaceType{ID: "workspace-1"}, "doc-1", nil); !errors.Is(err, ErrAuthenticationRequired) {
		t.Fatalf("anonymous read error = %v", err)
	}
}

func TestCommentsResolverEncodesEmptyPageCursorsLikeLegacyGraphQL(t *testing.T) {
	reader := &commentReadRepositoryFake{}
	resolver := &workspaceTypeResolver{Resolver: &Resolver{
		CommentReader: reader,
		CommentAuth:   &commentDocumentAuthorizerFake{access: workspaces.DocumentAuthorization{Read: true}},
	}}
	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{User: auth.User{ID: "reader-1"}})
	result, err := resolver.Comments(ctx, &model.WorkspaceType{ID: "workspace-1"}, "doc-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.PageInfo.StartCursor == nil || *result.PageInfo.StartCursor != "e30=" || result.PageInfo.EndCursor == nil || *result.PageInfo.EndCursor != "e30=" {
		t.Fatalf("empty page cursors = %+v, want base64 encoded empty objects", result.PageInfo)
	}
}

func TestGraphQLCommentsPaginationDefaultsMatchLegacy(t *testing.T) {
	for _, test := range []struct {
		name     string
		argument string
		want     int
	}{
		{name: "pagination omitted", want: 100},
		{name: "pagination object uses schema first default", argument: ", pagination: {}", want: 10},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := &commentReadRepositoryFake{}
			authenticator := requestAuthenticatorFunc(func(context.Context, *http.Request) (*auth.Principal, error) {
				return &auth.Principal{User: auth.User{ID: "reader-1"}}, nil
			})
			handler := NewHandlerWithBlobService(
				config.Config{Version: "0.27.5"}, authenticator, nil,
				commentWorkspaceRepositoryFake{workspace: workspaces.Workspace{ID: "workspace-1", OwnerID: "owner-1"}},
				auth.NewReadLimiter(auth.DefaultReadConcurrency), nil, nil, nil,
				CommentReadDependencies{Repository: reader, Authorizer: &commentDocumentAuthorizerFake{access: workspaces.DocumentAuthorization{Read: true}}},
			)
			query := `query { workspace(id: "workspace-1") { comments(docId: "doc-1"` + test.argument + `) { totalCount } } }`
			body, err := json.Marshal(map[string]any{"query": query})
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			response := newDeadlineTestResponseWriter()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK || strings.Contains(response.Body.String(), `"errors"`) {
				t.Fatalf("GraphQL response status=%d body=%s", response.Code, response.Body.String())
			}
			if reader.listLimit != test.want {
				t.Fatalf("repository limit = %d, want %d", reader.listLimit, test.want)
			}
		})
	}
}

func TestGraphQLListCommentsClientOperation(t *testing.T) {
	updated := time.Date(2026, 10, 17, 3, 0, 0, 0, time.UTC)
	reader := &commentReadRepositoryFake{
		total: 1,
		items: []comments.Comment{{
			SID: 12, ID: "comment-1", WorkspaceID: "workspace-1", DocID: "doc-1",
			Content: map[string]any{"type": "paragraph"}, CreatedAt: updated, UpdatedAt: updated,
			User: comments.User{ID: "author-1", Name: "Author"},
		}},
	}
	authenticator := requestAuthenticatorFunc(func(context.Context, *http.Request) (*auth.Principal, error) {
		return &auth.Principal{User: auth.User{ID: "reader-1"}}, nil
	})
	handler := NewHandlerWithBlobService(
		config.Config{Version: "0.27.5"}, authenticator, nil,
		commentWorkspaceRepositoryFake{workspace: workspaces.Workspace{ID: "workspace-1", OwnerID: "owner-1"}},
		auth.NewReadLimiter(auth.DefaultReadConcurrency), nil, nil, nil,
		CommentReadDependencies{Repository: reader, Authorizer: &commentDocumentAuthorizerFake{access: workspaces.DocumentAuthorization{Read: true}}},
	)
	body, err := json.Marshal(map[string]any{
		"query": `query listComments($workspaceId: String!, $docId: String!, $pagination: PaginationInput) {
			workspace(id: $workspaceId) {
				comments(docId: $docId, pagination: $pagination) {
					totalCount edges { cursor node { id content resolved createdAt updatedAt user { id name avatarUrl } replies { id commentId content createdAt updatedAt user { id name avatarUrl } } } }
					pageInfo { startCursor endCursor hasNextPage hasPreviousPage }
				}
			}
		}`,
		"variables": map[string]any{"workspaceId": "workspace-1", "docId": "doc-1", "pagination": map[string]any{"first": 10}},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := newDeadlineTestResponseWriter()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GraphQL status = %d, body = %s", response.Code, response.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result["errors"] != nil {
		t.Fatalf("GraphQL errors = %#v", result["errors"])
	}
	data := result["data"].(map[string]any)
	workspace := data["workspace"].(map[string]any)
	connection := workspace["comments"].(map[string]any)
	if connection["totalCount"] != float64(1) {
		t.Fatalf("comments connection = %#v", connection)
	}
	edges := connection["edges"].([]any)
	node := edges[0].(map[string]any)["node"].(map[string]any)
	if node["id"] != "comment-1" || node["content"].(map[string]any)["type"] != "paragraph" || node["user"].(map[string]any)["name"] != "Author" {
		t.Fatalf("comment node = %#v", node)
	}
	if strings.Contains(response.Body.String(), "email") {
		t.Fatalf("comment response exposed an unrequested private user field: %s", response.Body.String())
	}
}

func TestGraphQLCreateCommentMutationUsesAuthenticatedActorAndLegacyInput(t *testing.T) {
	createdAt := time.Date(2026, 10, 17, 4, 0, 0, 0, time.UTC)
	writer := &commentWriteRepositoryFake{commentResult: comments.Comment{
		ID: "comment-1", WorkspaceID: "workspace-1", DocID: "doc-1",
		Content: map[string]any{"text": "hello"}, CreatedAt: createdAt, UpdatedAt: createdAt,
		User: comments.User{ID: "author-1", Name: "Author"}, Replies: []comments.Reply{},
	}}
	publisher := &commentChangePublisherFake{}
	authenticator := requestAuthenticatorFunc(func(context.Context, *http.Request) (*auth.Principal, error) {
		return &auth.Principal{User: auth.User{ID: "author-1"}}, nil
	})
	handler := NewHandlerWithBlobService(
		config.Config{Version: "0.27.5"}, authenticator, nil,
		commentWorkspaceRepositoryFake{workspace: workspaces.Workspace{ID: "workspace-1", OwnerID: "owner-1"}},
		auth.NewReadLimiter(auth.DefaultReadConcurrency), nil, nil, nil,
		CommentReadDependencies{Repository: &commentReadRepositoryFake{}, Writer: writer, Publisher: publisher},
	)
	body, err := json.Marshal(map[string]any{
		"query": `mutation createComment($input: CommentCreateInput!) {
			createComment(input: $input) { id content resolved createdAt updatedAt user { id name avatarUrl } replies { id } }
		}`,
		"variables": map[string]any{"input": map[string]any{
			"workspaceId": "workspace-1", "docId": "doc-1", "docTitle": "Title", "docMode": "page",
			"content": map[string]any{"text": "hello"}, "mentions": []string{"mentioned-user"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := newDeadlineTestResponseWriter()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GraphQL status = %d, body = %s", response.Code, response.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result["errors"] != nil {
		t.Fatalf("GraphQL errors = %#v", result["errors"])
	}
	if writer.comment.actorID != "author-1" || writer.comment.workspaceID != "workspace-1" || writer.comment.docID != "doc-1" || writer.comment.content["text"] != "hello" || writer.comment.notification.DocTitle != "Title" || writer.comment.notification.DocMode != "page" || len(writer.comment.notification.Mentions) != 1 || writer.comment.notification.Mentions[0] != "mentioned-user" {
		t.Fatalf("create comment command = %+v", writer.comment)
	}
	data := result["data"].(map[string]any)["createComment"].(map[string]any)
	if data["id"] != "comment-1" || data["user"].(map[string]any)["id"] != "author-1" {
		t.Fatalf("created comment response = %#v", data)
	}
	if publisher.calls != 1 || publisher.workspaceID != "workspace-1" || publisher.docID != "doc-1" {
		t.Fatalf("comment change publication = %+v", publisher)
	}
}

func TestGraphQLCommentMutationWritesPublishDocumentChanges(t *testing.T) {
	createdAt := time.Date(2026, 10, 17, 4, 0, 0, 0, time.UTC)
	writer := &commentWriteRepositoryFake{
		commentResult: comments.Comment{ID: "comment-1", WorkspaceID: "workspace-1", DocID: "doc-1", CreatedAt: createdAt, UpdatedAt: createdAt},
		replyResult:   comments.Reply{ID: "reply-1", CommentID: "comment-1", WorkspaceID: "workspace-1", DocID: "doc-1", CreatedAt: createdAt, UpdatedAt: createdAt},
	}
	publisher := &commentChangePublisherFake{}
	authenticator := requestAuthenticatorFunc(func(context.Context, *http.Request) (*auth.Principal, error) {
		return &auth.Principal{User: auth.User{ID: "author-1"}}, nil
	})
	handler := NewHandlerWithBlobService(
		config.Config{Version: "0.27.5"}, authenticator, nil,
		commentWorkspaceRepositoryFake{workspace: workspaces.Workspace{ID: "workspace-1", OwnerID: "owner-1"}},
		auth.NewReadLimiter(auth.DefaultReadConcurrency), nil, nil, nil,
		CommentReadDependencies{Repository: &commentReadRepositoryFake{}, Writer: writer, Publisher: publisher},
	)
	body, err := json.Marshal(map[string]any{"query": `mutation {
		updateComment(input: {id: "comment-1", content: {text: "edited"}})
		resolveComment(input: {id: "comment-1", resolved: true})
		deleteComment(id: "comment-1")
		createReply(input: {commentId: "comment-1", content: {text: "reply"}, docTitle: "Title", docMode: page}) { id }
		updateReply(input: {id: "reply-1", content: {text: "edited reply"}})
		deleteReply(id: "reply-1")
	}`})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := newDeadlineTestResponseWriter()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GraphQL status = %d, body = %s", response.Code, response.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result["errors"] != nil {
		t.Fatalf("GraphQL errors = %#v", result["errors"])
	}
	data := result["data"].(map[string]any)
	for _, field := range []string{"updateComment", "resolveComment", "deleteComment", "updateReply", "deleteReply"} {
		if data[field] != true {
			t.Fatalf("%s result = %#v", field, data[field])
		}
	}
	if data["createReply"].(map[string]any)["id"] != "reply-1" {
		t.Fatalf("createReply result = %#v", data["createReply"])
	}
	if publisher.calls != 6 || publisher.workspaceID != "workspace-1" || publisher.docID != "doc-1" {
		t.Fatalf("comment change publications = %+v", publisher)
	}
}

func TestGraphQLCommentCreationPublishesCountForCommittedNotificationRecipients(t *testing.T) {
	createdAt := time.Date(2026, 10, 17, 4, 0, 0, 0, time.UTC)
	writer := &commentWriteRepositoryFake{commentResult: comments.Comment{
		ID: "comment-1", WorkspaceID: "workspace-1", DocID: "doc-1", CreatedAt: createdAt, UpdatedAt: createdAt,
		NotificationReceivers: []comments.CreatedNotification{{ID: "notification-1", UserID: "owner-1"}, {ID: "notification-2", UserID: "mentioned-1"}},
	}}
	changePublisher := &commentChangePublisherFake{}
	notificationRepository := &notificationResolverFake{items: []notifications.Notification{
		{ID: "notification-1", Type: "Comment", CreatedAt: createdAt},
		{ID: "notification-2", Type: "CommentMention", CreatedAt: createdAt},
	}}
	countPublisher := &notificationCountPublisherFake{}
	authenticator := requestAuthenticatorFunc(func(context.Context, *http.Request) (*auth.Principal, error) {
		return &auth.Principal{User: auth.User{ID: "author-1"}}, nil
	})
	handler := NewHandlerWithServices(
		config.Config{Version: "0.27.5"}, authenticator, nil,
		commentWorkspaceRepositoryFake{workspace: workspaces.Workspace{ID: "workspace-1", OwnerID: "owner-1"}},
		auth.NewReadLimiter(auth.DefaultReadConcurrency), nil, nil, nil,
		HandlerDependencies{
			Comments:      CommentReadDependencies{Repository: &commentReadRepositoryFake{}, Writer: writer, Publisher: changePublisher},
			Notifications: NotificationDependencies{Repository: notificationRepository, Publisher: countPublisher},
		},
	)
	body, err := json.Marshal(map[string]any{"query": `mutation {
		createComment(input: {workspaceId: "workspace-1", docId: "doc-1", docTitle: "Title", docMode: page, content: {text: "hello"}, mentions: ["mentioned-1"]}) { id }
	}`})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := newDeadlineTestResponseWriter()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), `"errors"`) {
		t.Fatalf("GraphQL response = %d %s", response.Code, response.Body.String())
	}
	if countPublisher.calls != 2 || len(countPublisher.events) != 2 {
		t.Fatalf("notification count publications = %+v", countPublisher.events)
	}
	want := map[string]bool{"owner-1": false, "mentioned-1": false}
	for _, event := range countPublisher.events {
		if _, ok := want[event.userID]; !ok || event.reason != "created" || event.count != 2 {
			t.Fatalf("notification count event = %+v", event)
		}
		want[event.userID] = true
	}
	if !want["owner-1"] || !want["mentioned-1"] || changePublisher.calls != 1 {
		t.Fatalf("notification recipients or comment event = %#v / %+v", want, changePublisher)
	}
}

func TestCommentMutationErrorsKeepLegacyGraphQLEnvelope(t *testing.T) {
	permissionError := presentGraphQLError(context.Background(), &comments.PermissionDeniedError{
		Action: "Doc.Comments.Create", WorkspaceID: "workspace-1", DocID: "doc-1", Cause: comments.ErrCommentsCreateForbidden,
	})
	if permissionError.Message != "You do not have permission to perform Doc.Comments.Create action on doc doc-1." || permissionError.Extensions["status"] != http.StatusForbidden || permissionError.Extensions["type"] != "NO_PERMISSION" || permissionError.Extensions["name"] != "DOC_ACTION_DENIED" {
		t.Fatalf("permission error presentation = %+v", permissionError)
	}
	data := permissionError.Extensions["data"].(map[string]string)
	if data["spaceId"] != "workspace-1" || data["docId"] != "doc-1" || data["action"] != "Doc.Comments.Create" {
		t.Fatalf("permission error data = %#v", data)
	}
	notFound := presentGraphQLError(context.Background(), comments.ErrNotFound)
	if notFound.Message != "Comment not found." || notFound.Extensions["status"] != http.StatusNotFound || notFound.Extensions["name"] != "COMMENT_NOT_FOUND" {
		t.Fatalf("not found error presentation = %+v", notFound)
	}
}

func TestCommentChangesResolverKeepsIndependentUpdateCursors(t *testing.T) {
	commentAt := time.Date(2026, 10, 17, 3, 1, 0, 0, time.UTC)
	replyAt := commentAt.Add(time.Minute)
	repository := &commentReadRepositoryFake{
		changes: []comments.Change{
			{Action: "update", ID: "comment-1", UpdatedAt: commentAt, Item: map[string]any{"id": "comment-1"}},
			{Action: "delete", ID: "reply-1", CommentID: stringPointer("comment-1"), UpdatedAt: replyAt, Item: map[string]any{"updatedAt": replyAt.Format(time.RFC3339Nano)}},
		},
	}
	authorizer := &commentDocumentAuthorizerFake{access: workspaces.DocumentAuthorization{Read: true}}
	resolver := &workspaceTypeResolver{Resolver: &Resolver{CommentReader: repository, CommentAuth: authorizer}}
	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{User: auth.User{ID: "reader-1"}})
	result, err := resolver.CommentChanges(ctx, &model.WorkspaceType{ID: "workspace-1"}, "doc-1", model.PaginationInput{})
	if err != nil {
		t.Fatal(err)
	}
	if result.TotalCount != 2 || result.Edges[0].Node.Action != model.CommentChangeActionUpdate || result.Edges[1].Node.CommentID == nil {
		t.Fatalf("comment changes = %+v", result)
	}
	if repository.changeLimit != 10 || repository.changeAfter != nil || repository.replyAfter != nil {
		t.Fatalf("default change pagination = %+v", repository)
	}
	if *result.PageInfo.StartCursor != "bnVsbA==" {
		t.Fatalf("legacy start cursor = %q", *result.PageInfo.StartCursor)
	}
	decoded, err := base64.StdEncoding.DecodeString(*result.PageInfo.EndCursor)
	if err != nil {
		t.Fatal(err)
	}
	var cursor commentChangeCursor
	if err := json.Unmarshal(decoded, &cursor); err != nil {
		t.Fatal(err)
	}
	if cursor.CommentUpdatedAt == nil || !cursor.CommentUpdatedAt.Equal(commentAt) || cursor.ReplyUpdatedAt == nil || !cursor.ReplyUpdatedAt.Equal(replyAt) {
		t.Fatalf("independent end cursor = %+v", cursor)
	}
}
