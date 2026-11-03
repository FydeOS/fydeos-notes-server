// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package commentattachments

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/storage"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

func TestCommentAttachmentReadMatchesLegacyHeadersAndAuthorization(t *testing.T) {
	lastModified := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	reader := &attachmentReaderFake{body: "<script>unsafe</script>", metadata: storage.ObjectMetadata{
		ContentType: "text/html; charset=utf-8", ContentLength: int64(len("<script>unsafe</script>")), LastModified: lastModified,
	}}
	authorizer := &documentAuthorizerFake{access: workspaces.DocumentAuthorization{Read: true}}
	handler := commentAttachmentRoute(NewHandler(authenticatorFake{principal: &auth.Principal{User: auth.User{ID: "viewer"}}}, authorizer, reader))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/workspaces/workspace-1/docs/doc-1/comment-attachments/attachment-1", nil))
	if response.Code != http.StatusOK || response.Body.String() != reader.body {
		t.Fatalf("attachment response = %d %q", response.Code, response.Body.String())
	}
	for header, want := range map[string]string{
		"Content-Type":           "text/html; charset=utf-8",
		"Content-Length":         "23",
		"Last-Modified":          lastModified.Format(http.TimeFormat),
		"Cache-Control":          "private, no-store",
		"X-Content-Type-Options": "nosniff",
		"Content-Disposition":    `attachment; filename="attachment-1"; filename*=UTF-8''attachment-1`,
	} {
		if got := response.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if authorizer.userID != "viewer" || authorizer.workspaceID != "workspace-1" || authorizer.docID != "doc-1" {
		t.Fatalf("document authorization args = %q/%q/%q", authorizer.userID, authorizer.workspaceID, authorizer.docID)
	}
	if reader.workspaceID != "workspace-1" || reader.docID != "doc-1" || reader.key != "attachment-1" {
		t.Fatalf("attachment lookup args = %q/%q/%q", reader.workspaceID, reader.docID, reader.key)
	}
}

func TestCommentAttachmentReadRejectsMissingSessionAndUnreadableDocument(t *testing.T) {
	for _, test := range []struct {
		name          string
		authErr       error
		authenticated bool
		access        workspaces.DocumentAuthorization
		wantStatus    int
	}{
		{name: "unauthenticated", authErr: auth.ErrNoSession, wantStatus: http.StatusUnauthorized},
		{name: "unreadable document", authenticated: true, access: workspaces.DocumentAuthorization{CommentsCreate: true}, wantStatus: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := &attachmentReaderFake{body: "attachment"}
			var principal *auth.Principal
			if test.authenticated {
				principal = &auth.Principal{User: auth.User{ID: "viewer"}}
			}
			handler := commentAttachmentRoute(NewHandler(
				authenticatorFake{principal: principal, err: test.authErr},
				&documentAuthorizerFake{access: test.access}, reader,
			))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/workspaces/workspace-1/docs/doc-1/comment-attachments/attachment-1", nil))
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
			body := decodeUserError(t, response)
			if body.Status != test.wantStatus {
				t.Errorf("error body status = %d, want %d", body.Status, test.wantStatus)
			}
			if test.name == "unauthenticated" && (body.Code != "Unauthorized" || body.Type != "AUTHENTICATION_REQUIRED" || body.Name != "AUTHENTICATION_REQUIRED" || body.Message != "You must sign in first to access this resource.") {
				t.Errorf("unauthenticated error body = %+v", body)
			}
			if test.name == "unreadable document" && (body.Code != "Forbidden" || body.Type != "NO_PERMISSION" || body.Name != "DOC_ACTION_DENIED" || body.Message != "You do not have permission to perform Doc.Read action on doc doc-1.") {
				t.Errorf("permission error body = %+v", body)
			}
			if test.name == "unreadable document" && (body.Data["spaceId"] != "workspace-1" || body.Data["docId"] != "doc-1" || body.Data["action"] != "Doc.Read") {
				t.Errorf("permission error data = %#v", body.Data)
			}
			if reader.called {
				t.Fatal("attachment storage was read after authorization failed")
			}
		})
	}
}

func TestCommentAttachmentReadHidesMissingAndDeletedObjects(t *testing.T) {
	reader := &attachmentReaderFake{err: fs.ErrNotExist}
	handler := commentAttachmentRoute(NewHandler(
		authenticatorFake{principal: &auth.Principal{User: auth.User{ID: "viewer"}}},
		&documentAuthorizerFake{access: workspaces.DocumentAuthorization{Read: true}}, reader,
	))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/workspaces/workspace-1/docs/doc-1/comment-attachments/missing", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("missing attachment status = %d, want 404", response.Code)
	}
	body := decodeUserError(t, response)
	if body.Code != "Not Found" || body.Type != "RESOURCE_NOT_FOUND" || body.Name != "COMMENT_ATTACHMENT_NOT_FOUND" || body.Message != "Comment attachment not found." {
		t.Fatalf("missing attachment error body = %+v", body)
	}
}

func TestCommentAttachmentReadHidesInternalStorageErrors(t *testing.T) {
	reader := &attachmentReaderFake{err: errors.New("sensitive storage diagnostic")}
	handler := commentAttachmentRoute(NewHandler(
		authenticatorFake{principal: &auth.Principal{User: auth.User{ID: "viewer"}}},
		&documentAuthorizerFake{access: workspaces.DocumentAuthorization{Read: true}}, reader,
	))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/workspaces/workspace-1/docs/doc-1/comment-attachments/file-1", nil))
	body := decodeUserError(t, response)
	if response.Code != http.StatusInternalServerError || body.Name != "INTERNAL_SERVER_ERROR" || body.Message != "An internal error occurred." {
		t.Fatalf("internal storage error response = %d %+v", response.Code, body)
	}
	if strings.Contains(response.Body.String(), "sensitive storage diagnostic") {
		t.Fatal("internal storage error leaked to the client")
	}
}

type decodedUserError struct {
	Status  int               `json:"status"`
	Code    string            `json:"code"`
	Type    string            `json:"type"`
	Name    string            `json:"name"`
	Message string            `json:"message"`
	Data    map[string]string `json:"data"`
}

func decodeUserError(t *testing.T, response *httptest.ResponseRecorder) decodedUserError {
	t.Helper()
	var body decodedUserError
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode user-friendly error: %v; body=%s", err, response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("error content-type = %q", got)
	}
	return body
}

type authenticatorFake struct {
	principal *auth.Principal
	err       error
}

func (a authenticatorFake) FromRequest(context.Context, *http.Request) (*auth.Principal, error) {
	return a.principal, a.err
}

type documentAuthorizerFake struct {
	access                     workspaces.DocumentAuthorization
	err                        error
	userID, workspaceID, docID string
}

func (a *documentAuthorizerFake) AuthorizeDocument(_ context.Context, userID, workspaceID, docID string) (workspaces.DocumentAuthorization, error) {
	a.userID, a.workspaceID, a.docID = userID, workspaceID, docID
	return a.access, a.err
}

type attachmentReaderFake struct {
	body                    string
	metadata                storage.ObjectMetadata
	err                     error
	called                  bool
	workspaceID, docID, key string
}

func (a *attachmentReaderFake) GetCommentAttachment(_ context.Context, workspaceID, docID, key string) (io.ReadCloser, storage.ObjectMetadata, error) {
	a.called = true
	a.workspaceID, a.docID, a.key = workspaceID, docID, key
	if a.err != nil {
		return nil, storage.ObjectMetadata{}, a.err
	}
	return io.NopCloser(strings.NewReader(a.body)), a.metadata, nil
}

func commentAttachmentRoute(handler http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /api/workspaces/{id}/docs/{docId}/comment-attachments/{key}", handler)
	return mux
}
