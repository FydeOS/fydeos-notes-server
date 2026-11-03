// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package documenthttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Deln0r/ygo"
	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/documents"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

type requestAuthenticator func(context.Context, *http.Request) (*auth.Principal, error)

func (f requestAuthenticator) FromRequest(ctx context.Context, r *http.Request) (*auth.Principal, error) {
	return f(ctx, r)
}

type documentAuthorizerStub struct {
	access workspaces.DocumentAuthorization
	err    error
	calls  int
	actor  string
	space  string
	doc    string
}

func (s *documentAuthorizerStub) AuthorizeDocument(_ context.Context, actor, space, doc string) (workspaces.DocumentAuthorization, error) {
	s.calls++
	s.actor, s.space, s.doc = actor, space, doc
	return s.access, s.err
}

type workspaceDocumentReaderStub struct {
	content []byte
	err     error
	calls   int
	space   string
	doc     string
}

func (s *workspaceDocumentReaderStub) DiffWorkspace(_ context.Context, space, doc string, _ []byte) ([]byte, []byte, int64, error) {
	s.calls++
	s.space, s.doc = space, doc
	return s.content, nil, int64(len(s.content)), s.err
}

type documentModeReaderStub struct {
	mode  int
	err   error
	calls int
	space string
	doc   string
}

func (s *documentModeReaderStub) ReadDocumentMode(_ context.Context, space, doc string) (int, error) {
	s.calls++
	s.space, s.doc = space, doc
	return s.mode, s.err
}

type workspaceReadStub struct {
	workspace            workspaces.Workspace
	err                  error
	calls                int
	publicDocs           []workspaces.DocumentInfo
	publicDocsErr        error
	publicDocumentsCalls int
}

func (s *workspaceReadStub) ReadWorkspace(context.Context, string, string) (workspaces.Workspace, error) {
	s.calls++
	return s.workspace, s.err
}

func (s *workspaceReadStub) ReadPublicDocuments(context.Context, string) ([]workspaces.DocumentInfo, error) {
	s.publicDocumentsCalls++
	return s.publicDocs, s.publicDocsErr
}

type viewRecorderStub struct {
	calls     int
	workspace string
	doc       string
	visitor   string
	guest     bool
	user      string
	err       error
	done      chan struct{}
}

func (s *viewRecorderStub) RecordDocView(_ context.Context, workspace, doc, visitor string, guest bool, user string, _ time.Time) error {
	s.calls++
	s.workspace, s.doc, s.visitor, s.guest, s.user = workspace, doc, visitor, guest, user
	if s.done != nil {
		close(s.done)
	}
	return s.err
}

func TestWorkspaceDocumentHandlerRecordsOnlySuccessfulDocumentGets(t *testing.T) {
	principal := &auth.Principal{SessionID: "session-1", User: auth.User{ID: "user-1"}}
	authenticator := requestAuthenticator(func(context.Context, *http.Request) (*auth.Principal, error) { return principal, nil })
	authorizer := &documentAuthorizerStub{access: workspaces.DocumentAuthorization{Read: true}}
	reader := &workspaceDocumentReaderStub{content: []byte("binary-doc")}
	modes := &documentModeReaderStub{mode: 1}
	workspaceReader := &workspaceReadStub{}
	analytics := &viewRecorderStub{done: make(chan struct{})}
	handler := NewHandler(authenticator, authorizer, reader, modes, workspaceReader, analytics)

	request := httptest.NewRequest(http.MethodGet, "/api/workspaces/space/docs/space:doc", nil)
	request.SetPathValue("id", "space")
	request.SetPathValue("docId", "space:doc")
	request.Header.Set("CF-Connecting-IP", "203.0.113.8")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "binary-doc" || response.Header().Get("Content-Type") != "application/octet-stream" || response.Header().Get("publish-mode") != "edgeless" {
		t.Fatalf("GET response status/body/headers = %d/%q/%v", response.Code, response.Body.String(), response.Header())
	}
	select {
	case <-analytics.done:
	case <-time.After(time.Second):
		t.Fatal("successful GET did not enqueue analytics")
	}
	if analytics.calls != 1 || analytics.workspace != "space" || analytics.doc != "doc" || analytics.user != "user-1" || analytics.guest || analytics.visitor != docVisitorID(request, principal, "space", "doc") {
		t.Fatalf("recorded view = %+v", analytics)
	}
	if authorizer.actor != "user-1" || authorizer.space != "space" || authorizer.doc != "doc" || reader.space != "space" || reader.doc != "doc" || modes.space != "space" || modes.doc != "doc" {
		t.Fatalf("canonical identity was not used consistently: auth=%+v reader=%+v mode=%+v", authorizer, reader, modes)
	}
	if workspaceReader.calls != 0 || authorizer.calls != 1 || reader.calls != 1 {
		t.Fatalf("reader/auth calls workspace=%d doc-auth=%d content=%d", workspaceReader.calls, authorizer.calls, reader.calls)
	}

	reader.err = documents.ErrWorkspaceDocNotFound
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound || analytics.calls != 1 {
		t.Fatalf("missing content status/record count = %d/%d", response.Code, analytics.calls)
	}
}

func TestWorkspaceDocumentHandlerRejectsMalformedIdentityBeforeAccess(t *testing.T) {
	authenticator := requestAuthenticator(func(context.Context, *http.Request) (*auth.Principal, error) { return nil, auth.ErrNoSession })
	authorizer := &documentAuthorizerStub{access: workspaces.DocumentAuthorization{Read: true}}
	reader := &workspaceDocumentReaderStub{content: []byte("must not be read")}
	modes := &documentModeReaderStub{}
	workspaceReader := &workspaceReadStub{}
	handler := NewHandler(authenticator, authorizer, reader, modes, workspaceReader, nil)
	request := httptest.NewRequest(http.MethodGet, "/api/workspaces/space/docs/workspace:any:doc", nil)
	request.SetPathValue("id", "space")
	request.SetPathValue("docId", "workspace:any:doc")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError || authorizer.calls != 0 || reader.calls != 0 || modes.calls != 0 {
		t.Fatalf("malformed identity status/access calls = %d/%d/%d/%d", response.Code, authorizer.calls, reader.calls, modes.calls)
	}
}

func TestPublicRootDocumentHandlerAuthorizesPageAndFiltersRoot(t *testing.T) {
	authenticator := requestAuthenticator(func(context.Context, *http.Request) (*auth.Principal, error) { return nil, auth.ErrNoSession })
	authorizer := &documentAuthorizerStub{access: workspaces.DocumentAuthorization{Read: true}}
	root := ygo.NewDoc()
	meta := ygo.NewMap(root, "meta")
	txn := root.WriteTxn()
	pages := meta.SetArray(txn, "pages")
	publicPage := pages.InsertMap(txn, 0)
	publicPage.Set(txn, "id", "public")
	publicPage.Set(txn, "title", "Public page")
	privatePage := pages.InsertMap(txn, 1)
	privatePage.Set(txn, "id", "private")
	privatePage.Set(txn, "title", "Private page")
	txn.Commit()
	reader := &workspaceDocumentReaderStub{content: ygo.EncodeStateAsUpdate(root)}
	modes := &documentModeReaderStub{}
	publicTitle := "Published title"
	workspaceReader := &workspaceReadStub{publicDocs: []workspaces.DocumentInfo{{PublishedDocumentMetadata: workspaces.PublishedDocumentMetadata{WorkspaceID: "space", DocID: "public", Title: &publicTitle}}}}
	analytics := &viewRecorderStub{}
	handler := NewHandler(authenticator, authorizer, reader, modes, workspaceReader, analytics)
	request := httptest.NewRequest(http.MethodGet, "/api/workspaces/space/public-docs/public/root-doc", nil)
	request.SetPathValue("id", "space")
	request.SetPathValue("docId", "public")
	response := httptest.NewRecorder()
	handler.PublicRootDocHandler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("public root response = %d, %v", response.Code, response.Header())
	}
	if authorizer.actor != "anonymous" || authorizer.space != "space" || authorizer.doc != "public" || reader.space != "space" || reader.doc != "space" || workspaceReader.publicDocumentsCalls != 1 || analytics.calls != 0 {
		t.Fatalf("authorization/read/list/analytics = %+v / %+v / %d / %d", authorizer, reader, workspaceReader.publicDocumentsCalls, analytics.calls)
	}
	filtered := ygo.NewDoc()
	if err := ygo.ApplyUpdate(filtered, response.Body.Bytes()); err != nil {
		t.Fatalf("response is not a valid Yjs update: %v", err)
	}
	filteredPages := ygo.NewMap(filtered, "meta").Get("pages").(*ygo.Array)
	if filteredPages.Len() != 1 || filteredPages.Get(0).(*ygo.Map).Get("id") != "public" {
		t.Fatalf("filtered page list = %#v", filteredPages.Get(0))
	}

	authorizer.access.Read = false
	response = httptest.NewRecorder()
	handler.PublicRootDocHandler().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || reader.calls != 1 || workspaceReader.publicDocumentsCalls != 1 {
		t.Fatalf("denied public root status/read/list = %d/%d/%d", response.Code, reader.calls, workspaceReader.publicDocumentsCalls)
	}
}

func TestWorkspaceDocumentHandlerUsesWorkspaceReadForRootAndSkipsHeadViews(t *testing.T) {
	principal := &auth.Principal{SessionID: "session-1", User: auth.User{ID: "user-1"}}
	authenticator := requestAuthenticator(func(context.Context, *http.Request) (*auth.Principal, error) { return principal, nil })
	authorizer := &documentAuthorizerStub{access: workspaces.DocumentAuthorization{Read: false}}
	reader := &workspaceDocumentReaderStub{content: []byte("root")}
	modes := &documentModeReaderStub{mode: 1}
	workspaceReader := &workspaceReadStub{}
	analytics := &viewRecorderStub{}
	handler := NewHandler(authenticator, authorizer, reader, modes, workspaceReader, analytics)
	rootRequest := httptest.NewRequest(http.MethodGet, "/api/workspaces/space/docs/space", nil)
	rootRequest.SetPathValue("id", "space")
	rootRequest.SetPathValue("docId", "space")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, rootRequest)
	if response.Code != http.StatusOK || workspaceReader.calls != 1 || authorizer.calls != 0 || modes.calls != 0 || analytics.calls != 0 {
		t.Fatalf("root document status/calls = %d/%d/%d/%d/%d", response.Code, workspaceReader.calls, authorizer.calls, modes.calls, analytics.calls)
	}

	authorizer.access.Read = true
	headRequest := httptest.NewRequest(http.MethodHead, "/api/workspaces/space/public-docs/doc", nil)
	headRequest.SetPathValue("id", "space")
	headRequest.SetPathValue("docId", "doc")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, headRequest)
	if response.Code != http.StatusOK || response.Header().Get("publish-mode") != "edgeless" || reader.calls != 1 || analytics.calls != 0 {
		t.Fatalf("HEAD status/headers/content calls/view calls = %d/%v/%d/%d", response.Code, response.Header(), reader.calls, analytics.calls)
	}
}

func TestWorkspaceDocumentHandlerDenialUsesUserFriendlyHTTPShape(t *testing.T) {
	authenticator := requestAuthenticator(func(context.Context, *http.Request) (*auth.Principal, error) { return nil, auth.ErrNoSession })
	authorizer := &documentAuthorizerStub{access: workspaces.DocumentAuthorization{Read: false}}
	reader := &workspaceDocumentReaderStub{content: []byte("should not be read")}
	analytics := &viewRecorderStub{}
	handler := NewHandler(authenticator, authorizer, reader, &documentModeReaderStub{}, &workspaceReadStub{}, analytics)
	request := httptest.NewRequest(http.MethodGet, "/api/workspaces/space/docs/doc", nil)
	request.SetPathValue("id", "space")
	request.SetPathValue("docId", "doc")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusForbidden || body["type"] != "NO_PERMISSION" || body["name"] != "DOC_ACTION_DENIED" || body["code"] != "Forbidden" || reader.calls != 0 || analytics.calls != 0 {
		t.Fatalf("denial status/body/read/record = %d/%v/%d/%d", response.Code, body, reader.calls, analytics.calls)
	}

	authorizer.err = errors.New("database failure")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError || reader.calls != 0 || analytics.calls != 0 {
		t.Fatalf("authorization failure status/read/record = %d/%d/%d", response.Code, reader.calls, analytics.calls)
	}
}
