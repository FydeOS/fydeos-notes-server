// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package realtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/comments"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

type commentChangesRepositoryFake struct {
	workspaceID string
	docID       string
	commentAt   *time.Time
	replyAt     *time.Time
	limit       int
	changes     []comments.Change
	calls       int
}

func (f *commentChangesRepositoryFake) ListChanges(_ context.Context, workspaceID, docID string, commentAt, replyAt *time.Time, limit int) ([]comments.Change, error) {
	f.workspaceID, f.docID = workspaceID, docID
	f.commentAt, f.replyAt, f.limit = commentAt, replyAt, limit
	f.calls++
	return f.changes, nil
}

type commentSyncAuthorizerFake struct {
	access workspaces.WorkspaceSyncAccess
	err    error
	userID string
	space  string
	docIDs []string
}

func (f *commentSyncAuthorizerFake) AuthorizeWorkspaceSync(_ context.Context, userID, workspaceID string, docIDs []string) (workspaces.WorkspaceSyncAccess, error) {
	f.userID, f.space, f.docIDs = userID, workspaceID, append([]string(nil), docIDs...)
	return f.access, f.err
}

func TestRealtimeCommentChangesChecksReadAccessAndPaginates(t *testing.T) {
	commentAt := time.Date(2026, 10, 17, 3, 0, 0, 0, time.UTC)
	replyAt := commentAt.Add(time.Minute)
	previousCommentAt := commentAt.Add(-time.Minute)
	previousReplyAt := replyAt.Add(-time.Minute)
	cursor, err := json.Marshal(commentChangeCursor{CommentUpdatedAt: &previousCommentAt, ReplyUpdatedAt: &previousReplyAt})
	if err != nil {
		t.Fatal(err)
	}
	repository := &commentChangesRepositoryFake{changes: []comments.Change{
		{Action: "update", ID: "reply-1", CommentID: commentTestStringPointer("comment-1"), UpdatedAt: replyAt, Item: map[string]any{"id": "reply-1"}},
		{Action: "delete", ID: "reply-2", CommentID: commentTestStringPointer("comment-1"), UpdatedAt: replyAt.Add(time.Second)},
	}}
	authorizer := &commentSyncAuthorizerFake{access: workspaces.WorkspaceSyncAccess{
		CanSync: true, Documents: map[string]workspaces.DocumentSyncAccess{"doc-1": {Read: true}},
	}}
	server := &Server{}
	server.SetCommentDependencies(repository, authorizer)
	result, err := server.commentChanges(context.Background(), "reader-1", map[string]any{
		"workspaceId": "workspace-1", "docId": "doc-1", "after": base64.StdEncoding.EncodeToString(cursor), "first": float64(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	if authorizer.userID != "reader-1" || authorizer.space != "workspace-1" || len(authorizer.docIDs) != 1 || authorizer.docIDs[0] != "doc-1" {
		t.Fatalf("authorization request = %+v", authorizer)
	}
	if repository.workspaceID != "workspace-1" || repository.docID != "doc-1" || repository.limit != 2 || repository.calls != 1 || repository.commentAt == nil || repository.replyAt == nil {
		t.Fatalf("change query = %+v", repository)
	}
	if result["startCursor"] != "" || result["hasNextPage"] != true || len(result["changes"].([]map[string]any)) != 1 {
		t.Fatalf("change page = %#v", result)
	}
	encodedCursor := result["endCursor"].(string)
	decodedCursor, err := base64.StdEncoding.DecodeString(encodedCursor)
	if err != nil {
		t.Fatal(err)
	}
	var endCursor commentChangeCursor
	if err := json.Unmarshal(decodedCursor, &endCursor); err != nil {
		t.Fatal(err)
	}
	if endCursor.CommentUpdatedAt == nil || !endCursor.CommentUpdatedAt.Equal(previousCommentAt) || endCursor.ReplyUpdatedAt == nil || !endCursor.ReplyUpdatedAt.Equal(replyAt) {
		t.Fatalf("independent end cursor = %+v", endCursor)
	}
}

func TestRealtimeCommentChangesTreatsZeroFirstAsUnpaginated(t *testing.T) {
	repository := &commentChangesRepositoryFake{changes: []comments.Change{
		{Action: "update", ID: "comment-1", UpdatedAt: time.Now()},
	}}
	authorizer := &commentSyncAuthorizerFake{access: workspaces.WorkspaceSyncAccess{
		Documents: map[string]workspaces.DocumentSyncAccess{"doc-1": {Read: true}},
	}}
	server := &Server{}
	server.SetCommentDependencies(repository, authorizer)
	result, err := server.commentChanges(context.Background(), "reader-1", map[string]any{
		"workspaceId": "workspace-1", "docId": "doc-1", "first": float64(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if repository.limit != 10000 || result["hasNextPage"] != false || len(result["changes"].([]map[string]any)) != 1 {
		t.Fatalf("zero-first response = %#v, repository limit=%d", result, repository.limit)
	}
}

func TestRealtimeCommentChangesRejectsUnreadableAndInvalidRequestsBeforeQuery(t *testing.T) {
	for _, test := range []struct {
		name   string
		access workspaces.WorkspaceSyncAccess
		input  map[string]any
	}{
		{name: "unreadable document", input: map[string]any{"workspaceId": "workspace-1", "docId": "doc-1"}},
		{name: "malformed cursor", access: workspaces.WorkspaceSyncAccess{Documents: map[string]workspaces.DocumentSyncAccess{"doc-1": {Read: true}}}, input: map[string]any{"workspaceId": "workspace-1", "docId": "doc-1", "after": "%%%"}},
		{name: "cursor is not a string", access: workspaces.WorkspaceSyncAccess{Documents: map[string]workspaces.DocumentSyncAccess{"doc-1": {Read: true}}}, input: map[string]any{"workspaceId": "workspace-1", "docId": "doc-1", "after": float64(1)}},
		{name: "negative page size", access: workspaces.WorkspaceSyncAccess{Documents: map[string]workspaces.DocumentSyncAccess{"doc-1": {Read: true}}}, input: map[string]any{"workspaceId": "workspace-1", "docId": "doc-1", "first": float64(-1)}},
		{name: "invalid page size type", access: workspaces.WorkspaceSyncAccess{Documents: map[string]workspaces.DocumentSyncAccess{"doc-1": {Read: true}}}, input: map[string]any{"workspaceId": "workspace-1", "docId": "doc-1", "first": "1"}},
		{name: "null page size", access: workspaces.WorkspaceSyncAccess{Documents: map[string]workspaces.DocumentSyncAccess{"doc-1": {Read: true}}}, input: map[string]any{"workspaceId": "workspace-1", "docId": "doc-1", "first": nil}},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := &commentChangesRepositoryFake{}
			authorizer := &commentSyncAuthorizerFake{access: test.access}
			server := &Server{}
			server.SetCommentDependencies(repository, authorizer)
			if _, err := server.commentChanges(context.Background(), "reader-1", test.input); err == nil {
				t.Fatal("expected comment changes request to be rejected")
			}
			if repository.calls != 0 {
				t.Fatalf("repository queried %d times for rejected request", repository.calls)
			}
		})
	}
	server := &Server{}
	if err := server.authorizeCommentRead(context.Background(), "reader-1", "workspace-1", "doc-1"); err == nil {
		t.Fatal("missing authorization dependency should fail closed")
	}
}

func commentTestStringPointer(value string) *string { return &value }
