// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package mcphttp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/mcpcredentials"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

type credentialServiceStub struct {
	enabled      bool
	enableErr    error
	credential   *mcpcredentials.AuthenticatedCredential
	authErr      error
	gotToken     string
	gotWorkspace string
}

func (s *credentialServiceStub) CopilotEnabled(context.Context) (bool, error) {
	return s.enabled, s.enableErr
}

func (s *credentialServiceStub) Authenticate(_ context.Context, token, workspaceID string) (*mcpcredentials.AuthenticatedCredential, error) {
	s.gotToken = token
	s.gotWorkspace = workspaceID
	return s.credential, s.authErr
}

type workspaceAuthorizerStub struct {
	workspaceAccess workspaces.WorkspaceAccess
	workspaceErr    error
	documents       map[string]workspaces.DocumentAuthorization
	documentErrors  map[string]error
	workspaceCalls  int
	documentCalls   []string
}

func (s *workspaceAuthorizerStub) GetWorkspaceAccess(context.Context, string, string) (workspaces.WorkspaceAccess, error) {
	s.workspaceCalls++
	return s.workspaceAccess, s.workspaceErr
}

func (s *workspaceAuthorizerStub) AuthorizeDocument(_ context.Context, _, _, docID string) (workspaces.DocumentAuthorization, error) {
	s.documentCalls = append(s.documentCalls, docID)
	return s.documents[docID], s.documentErrors[docID]
}

type workspaceDocumentReaderStub struct {
	updates map[string][]byte
	err     error
	calls   []string
}

func (s *workspaceDocumentReaderStub) DiffWorkspace(_ context.Context, _, docID string, _ []byte) ([]byte, []byte, int64, error) {
	s.calls = append(s.calls, docID)
	return s.updates[docID], nil, 1, s.err
}

func newMCPTestHandler() (*Handler, *credentialServiceStub, *workspaceAuthorizerStub, *workspaceDocumentReaderStub) {
	credentials := &credentialServiceStub{
		enabled:    true,
		credential: &mcpcredentials.AuthenticatedCredential{ID: "credential", UserID: "user", WorkspaceID: "workspace", AccessMode: "READ_ONLY"},
	}
	authorizer := &workspaceAuthorizerStub{
		workspaceAccess: workspaces.WorkspaceAccess{Permissions: map[string]bool{"Workspace_Read": true}},
		documents:       map[string]workspaces.DocumentAuthorization{"doc": {Read: true}},
		documentErrors:  map[string]error{},
	}
	reader := &workspaceDocumentReaderStub{updates: map[string][]byte{}}
	return NewHandler(credentials, authorizer, reader).(*Handler), credentials, authorizer, reader
}

func mcpRequest(t *testing.T, handler http.Handler, method, body, authorization string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, "/api/workspaces/workspace/mcp", strings.NewReader(body))
	request.SetPathValue("id", "workspace")
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func decodeRPCResponse(t *testing.T, response *httptest.ResponseRecorder) jsonRPCResponse {
	t.Helper()
	var result jsonRPCResponse
	decoder := json.NewDecoder(strings.NewReader(response.Body.String()))
	decoder.UseNumber()
	if err := decoder.Decode(&result); err != nil {
		t.Fatalf("decode JSON-RPC response: %v; body=%q", err, response.Body.String())
	}
	return result
}

func TestHandlerAuthenticatesAndChecksWorkspaceAccessBeforeParsing(t *testing.T) {
	handler, credentials, authorizer, reader := newMCPTestHandler()
	authorizer.workspaceErr = workspaces.ErrAccessDenied
	response := mcpRequest(t, handler, http.MethodPost, "not-json", "bEaReR   token-value")
	got := decodeRPCResponse(t, response)
	if response.Code != http.StatusForbidden || got.Error == nil || got.Error.Message != "Access denied" {
		t.Fatalf("response = %d %s, want 403 Access denied", response.Code, response.Body.String())
	}
	if credentials.gotToken != "token-value" || credentials.gotWorkspace != "workspace" {
		t.Fatalf("Authenticate() token=%q workspace=%q", credentials.gotToken, credentials.gotWorkspace)
	}
	if authorizer.workspaceCalls != 1 || len(reader.calls) != 0 {
		t.Fatalf("authorization calls=%d document reads=%v", authorizer.workspaceCalls, reader.calls)
	}
}

func TestHandlerMapsCredentialErrorsAndFeatureGate(t *testing.T) {
	t.Run("invalid token", func(t *testing.T) {
		handler, credentials, _, _ := newMCPTestHandler()
		credentials.authErr = mcpcredentials.ErrUnauthorized
		response := mcpRequest(t, handler, http.MethodPost, `{}`, "Bearer invalid")
		got := decodeRPCResponse(t, response)
		if response.Code != http.StatusUnauthorized || got.Error == nil || got.Error.Message != "Authentication failed" {
			t.Fatalf("response = %d %s", response.Code, response.Body.String())
		}
	})

	t.Run("Copilot disabled", func(t *testing.T) {
		handler, credentials, _, _ := newMCPTestHandler()
		credentials.enabled = false
		response := mcpRequest(t, handler, http.MethodPost, `{}`, "Bearer token")
		got := decodeRPCResponse(t, response)
		if response.Code != http.StatusForbidden || got.Error == nil || got.Error.Message != "Access denied" {
			t.Fatalf("response = %d %s", response.Code, response.Body.String())
		}
		if credentials.gotToken != "" {
			t.Fatalf("Authenticate() called while Copilot disabled: token=%q", credentials.gotToken)
		}
	})
}

func TestHandlerStatelessMethodsAndProtocolNegotiation(t *testing.T) {
	handler, _, _, _ := newMCPTestHandler()
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		response := mcpRequest(t, handler, method, "", "")
		got := decodeRPCResponse(t, response)
		if response.Code != http.StatusMethodNotAllowed || got.Error == nil || got.Error.Message != "Method not allowed." {
			t.Errorf("%s response = %d %s", method, response.Code, response.Body.String())
		}
	}

	response := mcpRequest(t, handler, http.MethodPost,
		`{"jsonrpc":"2.0","id":7,"method":"initialize","params":{"protocolVersion":"2024-10-07"}}`,
		"Bearer token")
	got := decodeRPCResponse(t, response)
	if response.Code != http.StatusOK || got.ID != json.Number("7") {
		t.Fatalf("initialize response = %d %+v", response.Code, got)
	}
	serverInfo := got.Result["serverInfo"].(map[string]any)
	if got.Result["protocolVersion"] != "2024-10-07" || serverInfo["name"] != "FydeOS Notes MCP Server for Workspace workspace" || serverInfo["version"] != "1.0.1" {
		t.Fatalf("initialize result = %#v", got.Result)
	}

	response = mcpRequest(t, handler, http.MethodPost,
		`{"jsonrpc":"2.0","id":"init","method":"initialize","params":{"protocolVersion":"unsupported"}}`,
		"Bearer token")
	got = decodeRPCResponse(t, response)
	if got.Result["protocolVersion"] != defaultProtocol {
		t.Fatalf("unsupported version negotiated as %#v", got.Result["protocolVersion"])
	}
}

func TestHandlerNotificationsAndBatchLimit(t *testing.T) {
	handler, _, _, _ := newMCPTestHandler()
	response := mcpRequest(t, handler, http.MethodPost,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`, "Bearer token")
	if response.Code != http.StatusAccepted || response.Body.Len() != 0 {
		t.Fatalf("notification response = %d %q", response.Code, response.Body.String())
	}

	tooMany := make([]string, maxBatchSize+1)
	for i := range tooMany {
		tooMany[i] = `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	}
	response = mcpRequest(t, handler, http.MethodPost, "["+strings.Join(tooMany, ",")+"]", "Bearer token")
	got := decodeRPCResponse(t, response)
	if response.Code != http.StatusBadRequest || got.Error == nil || got.Error.Message != "Batch size exceeds limit (20)." {
		t.Fatalf("oversized batch response = %d %s", response.Code, response.Body.String())
	}
}

func TestHandlerRejectsConcurrentRequestsBeforeReadingBody(t *testing.T) {
	handler, _, _, _ := newMCPTestHandler()
	for range maxConcurrentRequests {
		handler.requestSlots <- struct{}{}
	}
	response := mcpRequest(t, handler, http.MethodPost, "invalid json", "Bearer token")
	got := decodeRPCResponse(t, response)
	if response.Code != http.StatusTooManyRequests || got.Error == nil || got.Error.Message != "Too many requests." {
		t.Fatalf("concurrent request response = %d %s", response.Code, response.Body.String())
	}
}

func TestReadRequestBodyEnforcesLegacyJSONLimit(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/api/workspaces/workspace/mcp", nil)
	request.Body = io.NopCloser(bytes.NewReader(bytes.Repeat([]byte{'x'}, maxRequestBodyBytes+1)))
	_, err := readRequestBody(httptest.NewRecorder(), request)
	var sizeErr *http.MaxBytesError
	if !errors.As(err, &sizeErr) {
		t.Fatalf("oversized request error = %v, want http.MaxBytesError", err)
	}
}

func TestReadRequestBodyClosesSlowReaderAtDeadline(t *testing.T) {
	body := &blockingRequestBody{closed: make(chan struct{})}
	request := httptest.NewRequest(http.MethodPost, "/api/workspaces/workspace/mcp", nil)
	request.Body = body
	_, err := readRequestBodyWithTimeout(httptest.NewRecorder(), request, 10*time.Millisecond)
	if err == nil {
		t.Fatal("slow request body completed without a timeout")
	}
	select {
	case <-body.closed:
	default:
		t.Fatal("body was not closed after the read deadline")
	}
}

func TestHandlerReadDeadlineInterruptsRealSlowRequestBody(t *testing.T) {
	handler, _, _, _ := newMCPTestHandler()
	handler.bodyReadTimeout = 50 * time.Millisecond
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.SetPathValue("id", "workspace")
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	connection, err := net.DialTimeout("tcp", strings.TrimPrefix(server.URL, "http://"), time.Second)
	if err != nil {
		t.Fatalf("connect slow MCP client: %v", err)
	}
	defer connection.Close()
	if err := connection.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set test client read deadline: %v", err)
	}
	_, err = io.WriteString(connection, "POST /api/workspaces/workspace/mcp HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer token\r\nContent-Type: application/json\r\nContent-Length: 20\r\n\r\n{")
	if err != nil {
		t.Fatalf("write partial MCP request: %v", err)
	}
	reader := bufio.NewReader(connection)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatalf("read response after MCP body deadline: %v", err)
	}
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("slow body response status = %d, want %d", response.StatusCode, http.StatusBadRequest)
	}
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatalf("read slow body error response: %v", err)
	}
	if err := connection.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set keep-alive cleanup deadline: %v", err)
	}
	if _, err := reader.ReadByte(); err == nil {
		t.Fatal("server kept the connection alive after an incomplete request body")
	} else {
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			t.Fatal("server did not close the connection after an incomplete request body")
		}
	}
}

func TestHandlerEarlyRejectionsCloseSlowRequestBodies(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		authorize string
		prepare   func(*Handler, *credentialServiceStub)
	}{
		{
			name:      "unauthorized",
			status:    http.StatusUnauthorized,
			authorize: "Bearer invalid",
			prepare: func(_ *Handler, credentials *credentialServiceStub) {
				credentials.authErr = mcpcredentials.ErrUnauthorized
			},
		},
		{
			name:      "at capacity",
			status:    http.StatusTooManyRequests,
			authorize: "Bearer token",
			prepare: func(handler *Handler, _ *credentialServiceStub) {
				for range cap(handler.requestSlots) {
					handler.requestSlots <- struct{}{}
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, credentials, _, _ := newMCPTestHandler()
			test.prepare(handler, credentials)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				r.SetPathValue("id", "workspace")
				handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			connection, err := net.DialTimeout("tcp", strings.TrimPrefix(server.URL, "http://"), time.Second)
			if err != nil {
				t.Fatalf("connect slow MCP client: %v", err)
			}
			defer connection.Close()
			if err := connection.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
				t.Fatalf("set test client read deadline: %v", err)
			}
			request := "POST /api/workspaces/workspace/mcp HTTP/1.1\r\nHost: localhost\r\nAuthorization: " + test.authorize + "\r\nContent-Type: application/json\r\nContent-Length: 20\r\n\r\n{"
			if _, err := io.WriteString(connection, request); err != nil {
				t.Fatalf("write partial MCP request: %v", err)
			}
			reader := bufio.NewReader(connection)
			response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodPost})
			if err != nil {
				t.Fatalf("read early rejection response: %v", err)
			}
			if response.StatusCode != test.status {
				t.Fatalf("early rejection status = %d, want %d", response.StatusCode, test.status)
			}
			if _, err := io.Copy(io.Discard, response.Body); err != nil {
				t.Fatalf("read early rejection body: %v", err)
			}
			if _, err := reader.ReadByte(); err == nil {
				t.Fatal("server kept the connection alive after rejecting an incomplete request body")
			} else {
				var timeout net.Error
				if errors.As(err, &timeout) && timeout.Timeout() {
					t.Fatal("server did not close the connection after rejecting an incomplete request body")
				}
			}
		})
	}
}

type blockingRequestBody struct {
	closed    chan struct{}
	closeOnce sync.Once
}

func (b *blockingRequestBody) Read([]byte) (int, error) {
	<-b.closed
	return 0, errors.New("request body closed")
}

func (b *blockingRequestBody) Close() error {
	b.closeOnce.Do(func() { close(b.closed) })
	return nil
}

func TestHandlerBatchPreservesResponsesAndRejectsUnknownTool(t *testing.T) {
	handler, _, _, _ := newMCPTestHandler()
	response := mcpRequest(t, handler, http.MethodPost,
		`[{"jsonrpc":"2.0","id":"a","method":"ping"},{"jsonrpc":"2.0","method":"notifications/initialized"},{"jsonrpc":"2.0","id":"b","method":"tools/call","params":{"name":"missing"}}]`,
		"Bearer token")
	var got []jsonRPCResponse
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || len(got) != 2 || got[0].ID != "a" || got[0].Result == nil || got[1].ID != "b" || got[1].Error == nil || got[1].Error.Code != -32602 || got[1].Error.Message != "Tool not found: missing" {
		t.Fatalf("batch response = %d %s (%+v)", response.Code, response.Body.String(), got)
	}
}

func TestHandlerReadDocumentEnforcesDocumentPermissionAndProjectsMarkdown(t *testing.T) {
	handler, _, authorizer, reader := newMCPTestHandler()
	fixture, err := os.ReadFile("../documents/testdata/sample-document.snapshot.bin")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("../documents/testdata/sample-document.md")
	if err != nil {
		t.Fatal(err)
	}
	reader.updates["doc"] = fixture

	response := mcpRequest(t, handler, http.MethodPost,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read_document","arguments":{"docId":"doc"}}}`,
		"Bearer token")
	got := decodeRPCResponse(t, response)
	content := got.Result["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	if response.Code != http.StatusOK || text != string(want) {
		t.Fatalf("read_document response status=%d, markdown differs", response.Code)
	}
	if len(authorizer.documentCalls) != 1 || authorizer.documentCalls[0] != "doc" || len(reader.calls) != 1 {
		t.Fatalf("document authorization=%v reads=%v", authorizer.documentCalls, reader.calls)
	}

	authorizer.documents["doc"] = workspaces.DocumentAuthorization{Read: false}
	response = mcpRequest(t, handler, http.MethodPost,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"read_document","arguments":{"docId":"doc"}}}`,
		"Bearer token")
	got = decodeRPCResponse(t, response)
	resultContent := got.Result["content"].([]any)
	if resultContent[0].(map[string]any)["text"] != "Doc with id doc not found." || len(reader.calls) != 1 {
		t.Fatalf("denied read response=%#v reader calls=%v", got.Result, reader.calls)
	}
}

func TestHandlerDocumentSearchUsesScopedFallbackAndRequiresExplicitIDs(t *testing.T) {
	handler, _, _, reader := newMCPTestHandler()
	fixture, err := os.ReadFile("../documents/testdata/sample-document.snapshot.bin")
	if err != nil {
		t.Fatal(err)
	}
	reader.updates["doc"] = fixture
	response := mcpRequest(t, handler, http.MethodPost,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"doc_search","arguments":{"query":"notes","doc_ids":["doc"],"limit":1}}}`,
		"Bearer token")
	got := decodeRPCResponse(t, response)
	content := got.Result["content"].([]any)
	var result map[string]any
	if err := json.Unmarshal([]byte(content[0].(map[string]any)["text"].(string)), &result); err != nil {
		t.Fatal(err)
	}
	if result["retrieval_mode"] != "scoped" || result["degraded_reason"] != "SEARCH_UNAVAILABLE" {
		t.Fatalf("search fallback metadata = %#v", result)
	}
	hits := result["hits"].([]any)
	if len(hits) != 1 || hits[0].(map[string]any)["doc_id"] != "doc" {
		t.Fatalf("search hits = %#v", hits)
	}

	response = mcpRequest(t, handler, http.MethodPost,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"doc_search","arguments":{"query":"notes"}}}`,
		"Bearer token")
	got = decodeRPCResponse(t, response)
	content = got.Result["content"].([]any)
	if content[0].(map[string]any)["text"] != "SEARCH_UNAVAILABLE" {
		t.Fatalf("unscoped search response = %#v", got.Result)
	}
}

func TestHandlerMapsUnexpectedServiceFailures(t *testing.T) {
	handler, credentials, _, _ := newMCPTestHandler()
	credentials.enableErr = errors.New("database unavailable")
	response := mcpRequest(t, handler, http.MethodPost, `{}`, "Bearer token")
	got := decodeRPCResponse(t, response)
	if response.Code != http.StatusInternalServerError || got.Error == nil || got.Error.Code != -32603 {
		t.Fatalf("service failure response = %d %s", response.Code, response.Body.String())
	}
}
