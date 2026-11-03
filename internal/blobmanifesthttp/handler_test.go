// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package blobmanifesthttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/blobs"
)

type requestAuthenticator func(context.Context, *http.Request) (*auth.Principal, error)

func (f requestAuthenticator) FromRequest(ctx context.Context, r *http.Request) (*auth.Principal, error) {
	return f(ctx, r)
}

type manifestReaderStub struct {
	docCalls        int
	docActor        string
	docSource       blobs.BlobSource
	workspaceCalls  int
	workspaceActor  string
	workspaceID     string
	workspaceCursor *string
	workspaceLimit  *int
	blobCalls       int
	blobActor       string
	blobSource      blobs.BlobSource
	blobKey         string
	blobBody        io.ReadCloser
	blobMetadata    blobs.BlobReadMetadata
	blobErr         error
	manifest        blobs.BlobManifest
}

func (s *manifestReaderStub) DocManifest(_ context.Context, actor string, source blobs.BlobSource) (blobs.BlobManifest, error) {
	s.docCalls++
	s.docActor, s.docSource = actor, source
	return s.manifest, nil
}

func (s *manifestReaderStub) ReadableWorkspaceManifest(_ context.Context, actor, workspace string, cursor *string, limit *int) (blobs.BlobManifest, error) {
	s.workspaceCalls++
	s.workspaceActor, s.workspaceID, s.workspaceCursor, s.workspaceLimit = actor, workspace, cursor, limit
	return s.manifest, nil
}

func (s *manifestReaderStub) OpenBlobV1(_ context.Context, actor string, source blobs.BlobSource, key string) (io.ReadCloser, blobs.BlobReadMetadata, error) {
	s.blobCalls++
	s.blobActor, s.blobSource, s.blobKey = actor, source, key
	return s.blobBody, s.blobMetadata, s.blobErr
}

func TestDocManifestAllowsAnonymousCurrentSourceAndCanonicalizesIdentity(t *testing.T) {
	reader := &manifestReaderStub{manifest: blobs.BlobManifest{Version: 1, Entries: []blobs.BlobManifestEntry{}}}
	handler := NewHandler(requestAuthenticator(func(context.Context, *http.Request) (*auth.Principal, error) {
		return nil, auth.ErrNoSession
	}), reader)
	request := httptest.NewRequest(http.MethodGet, "/api/workspaces/space/blob-manifest/v1?sourceType=currentDoc&docId=space%3Adoc", nil)
	request.SetPathValue("id", "space")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("response status/cache = %d/%q", response.Code, response.Header().Get("Cache-Control"))
	}
	if reader.docCalls != 1 || reader.docActor != "" || reader.docSource.WorkspaceID != "space" || reader.docSource.DocID != "doc" || reader.docSource.Type != "currentDoc" {
		t.Fatalf("manifest call = actor %q, source %+v, calls %d", reader.docActor, reader.docSource, reader.docCalls)
	}
}

func TestDocManifestRequiresPresentSafeHistoryTimestamp(t *testing.T) {
	reader := &manifestReaderStub{}
	handler := NewHandler(requestAuthenticator(func(context.Context, *http.Request) (*auth.Principal, error) {
		return nil, auth.ErrNoSession
	}), reader)
	for _, url := range []string{
		"/api/workspaces/space/blob-manifest/v1?sourceType=history&docId=doc",
		"/api/workspaces/space/blob-manifest/v1?sourceType=history&docId=doc&timestampMs=9007199254740992",
	} {
		request := httptest.NewRequest(http.MethodGet, url, nil)
		request.SetPathValue("id", "space")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Errorf("GET %s status = %d, want %d", url, response.Code, http.StatusBadRequest)
		}
	}
	if reader.docCalls != 0 {
		t.Fatalf("invalid history timestamp reached service %d times", reader.docCalls)
	}
}

func TestReadableWorkspaceManifestRequiresSessionAndPassesPagingOptions(t *testing.T) {
	reader := &manifestReaderStub{}
	noSession := NewHandler(requestAuthenticator(func(context.Context, *http.Request) (*auth.Principal, error) {
		return nil, auth.ErrNoSession
	}), reader)
	request := httptest.NewRequest(http.MethodGet, "/api/workspaces/space/readable-blob-manifest/v1", nil)
	request.SetPathValue("id", "space")
	response := httptest.NewRecorder()
	noSession.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || reader.workspaceCalls != 0 {
		t.Fatalf("anonymous workspace manifest response = %d, service calls %d", response.Code, reader.workspaceCalls)
	}

	principal := &auth.Principal{User: auth.User{ID: "reader"}}
	authenticated := NewHandler(requestAuthenticator(func(context.Context, *http.Request) (*auth.Principal, error) {
		return principal, nil
	}), reader)
	request = httptest.NewRequest(http.MethodGet, "/api/workspaces/space/readable-blob-manifest/v1?cursor=&limit=0x10", nil)
	request.SetPathValue("id", "space")
	response = httptest.NewRecorder()
	authenticated.ServeHTTP(response, request)
	if response.Code != http.StatusOK || reader.workspaceCalls != 1 || reader.workspaceActor != "reader" || reader.workspaceID != "space" {
		t.Fatalf("authenticated workspace manifest response = %d, call %+v", response.Code, reader)
	}
	if reader.workspaceCursor == nil || *reader.workspaceCursor != "" || reader.workspaceLimit == nil || *reader.workspaceLimit != 16 {
		t.Fatalf("paging options = cursor %v, limit %v", reader.workspaceCursor, reader.workspaceLimit)
	}
}

func TestManifestServiceErrorsRemainServerErrorsAtHTTPBoundary(t *testing.T) {
	reader := &manifestReaderErrorStub{err: errors.New("database unavailable")}
	handler := NewHandler(requestAuthenticator(func(context.Context, *http.Request) (*auth.Principal, error) {
		return nil, auth.ErrNoSession
	}), reader)
	request := httptest.NewRequest(http.MethodGet, "/api/workspaces/space/blob-manifest/v1?sourceType=currentDoc&docId=doc", nil)
	request.SetPathValue("id", "space")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("manifest error status = %d, want %d", response.Code, http.StatusInternalServerError)
	}
}

func TestBlobReadStreamsAnonymousSourceWithSafeHeaders(t *testing.T) {
	lastModified := time.Date(2026, 10, 19, 8, 30, 0, 0, time.UTC)
	reader := &manifestReaderStub{
		blobBody: io.NopCloser(bytes.NewReader([]byte("hello"))),
		blobMetadata: blobs.BlobReadMetadata{
			MIME: "text/html; charset=utf-8", Size: 5, LastModified: lastModified,
		},
	}
	handler := NewHandler(requestAuthenticator(func(context.Context, *http.Request) (*auth.Principal, error) {
		return nil, auth.ErrNoSession
	}), reader)
	request := httptest.NewRequest(http.MethodGet, "/api/workspaces/space/blobs/v1/x%22%3E%3Cscript%3E.html?sourceType=currentDoc&docId=space%3Adoc", nil)
	request.SetPathValue("id", "space")
	request.SetPathValue("name", `x"><script>.html`)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || response.Body.String() != "hello" {
		t.Fatalf("blob response = %d %q, want 200 %q", response.Code, response.Body.String(), "hello")
	}
	if response.Header().Get("Content-Type") != "text/html; charset=utf-8" || response.Header().Get("Content-Length") != "5" {
		t.Fatalf("blob response content headers = %v", response.Header())
	}
	if response.Header().Get("Last-Modified") != lastModified.Format(http.TimeFormat) || response.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("blob response cache/last-modified headers = %v", response.Header())
	}
	if response.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.HasPrefix(response.Header().Get("Content-Disposition"), "attachment;") {
		t.Fatalf("dangerous MIME response headers = %v", response.Header())
	}
	if strings.Contains(response.Header().Get("Content-Disposition"), "<script>") {
		t.Fatalf("unsafe filename remained in Content-Disposition: %q", response.Header().Get("Content-Disposition"))
	}
	if reader.blobCalls != 1 || reader.blobActor != "" || reader.blobSource.WorkspaceID != "space" || reader.blobSource.DocID != "doc" || reader.blobKey != `x"><script>.html` {
		t.Fatalf("blob opener call = actor %q, source %+v, key %q, calls %d", reader.blobActor, reader.blobSource, reader.blobKey, reader.blobCalls)
	}
}

func TestBlobReadValidatesSourceAndMapsBlobErrors(t *testing.T) {
	reader := &manifestReaderStub{blobErr: blobs.ErrManifestBlobNotReferenced}
	handler := NewHandler(requestAuthenticator(func(context.Context, *http.Request) (*auth.Principal, error) {
		return nil, auth.ErrNoSession
	}), reader)
	request := httptest.NewRequest(http.MethodGet, "/api/workspaces/space/blobs/v1/key?sourceType=currentDoc&docId=doc", nil)
	request.SetPathValue("id", "space")
	request.SetPathValue("name", "key")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || reader.blobCalls != 1 {
		t.Fatalf("unreferenced blob response = %d, opener calls %d", response.Code, reader.blobCalls)
	}
	var errorResponse userErrorResponse
	if err := json.NewDecoder(response.Body).Decode(&errorResponse); err != nil {
		t.Fatal(err)
	}
	if errorResponse.Name != "BLOB_INVALID" {
		t.Fatalf("unreferenced blob error = %+v", errorResponse)
	}

	for _, path := range []string{
		"/api/workspaces/space/blobs/v1/key?sourceType=currentDoc",
		"/api/workspaces/space/blobs/v1/key?sourceType=history&docId=doc",
	} {
		request = httptest.NewRequest(http.MethodGet, path, nil)
		request.SetPathValue("id", "space")
		request.SetPathValue("name", "key")
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Errorf("GET %s status = %d, want %d", path, response.Code, http.StatusBadRequest)
		}
	}
	if reader.blobCalls != 1 {
		t.Fatalf("invalid source reached blob opener %d times", reader.blobCalls-1)
	}
}

type manifestReaderErrorStub struct{ err error }

func (s *manifestReaderErrorStub) DocManifest(context.Context, string, blobs.BlobSource) (blobs.BlobManifest, error) {
	return blobs.BlobManifest{}, s.err
}

func (s *manifestReaderErrorStub) ReadableWorkspaceManifest(context.Context, string, string, *string, *int) (blobs.BlobManifest, error) {
	return blobs.BlobManifest{}, s.err
}
