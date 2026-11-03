// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package realtime

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/documents"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
	"github.com/zishang520/engine.io/v2/types"
)

func TestEngineIOv4PollingHandshake(t *testing.T) {
	ioServer, err := New(config.Config{NodeEnv: "test", Host: "localhost"})
	if err != nil {
		t.Fatal(err)
	}
	defer ioServer.Close()

	mux := http.NewServeMux()
	mux.Handle("/socket.io", ioServer.Handler())
	mux.Handle("/socket.io/", ioServer.Handler())
	server := httptest.NewServer(mux)
	defer server.Close()

	request, err := http.NewRequest(http.MethodGet, server.URL+"/socket.io/?EIO=4&transport=polling", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Origin", "http://localhost:8080")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("handshake status = %d, body = %q", response.StatusCode, body)
	}
	if got := response.Header.Get("Access-Control-Allow-Origin"); got != "http://localhost:8080" {
		t.Fatalf("CORS origin = %q", got)
	}
	if len(body) < 2 || body[0] != '0' {
		t.Fatalf("expected Engine.IO open packet, got %q", body)
	}
	var handshake struct {
		SID         string   `json:"sid"`
		Upgrades    []string `json:"upgrades"`
		MaxPayload  int64    `json:"maxPayload"`
		PingTimeout int64    `json:"pingTimeout"`
	}
	if err := json.Unmarshal(body[1:], &handshake); err != nil {
		t.Fatalf("decode Engine.IO handshake: %v; body=%q", err, body)
	}
	if handshake.SID == "" || handshake.PingTimeout <= 0 {
		t.Fatalf("invalid Engine.IO handshake: %+v", handshake)
	}
	if handshake.MaxPayload != config.DefaultWebsocketMaxHttpBufferSize {
		t.Fatalf("maxPayload = %d, want %d", handshake.MaxPayload, config.DefaultWebsocketMaxHttpBufferSize)
	}
	if !contains(handshake.Upgrades, "websocket") {
		t.Fatalf("upgrades = %#v, want websocket", handshake.Upgrades)
	}

	postURL := server.URL + "/socket.io/?EIO=4&transport=polling&sid=" + handshake.SID
	postRequest, err := http.NewRequest(http.MethodPost, postURL, strings.NewReader("40"))
	if err != nil {
		t.Fatal(err)
	}
	postRequest.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	postResponse, err := server.Client().Do(postRequest)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, postResponse.Body)
	postResponse.Body.Close()
	if postResponse.StatusCode != http.StatusOK {
		t.Fatalf("namespace connect POST status = %d", postResponse.StatusCode)
	}

	pollRequest, err := http.NewRequest(http.MethodGet, postURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	pollResponse, err := client.Do(pollRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer pollResponse.Body.Close()
	connectPacket, err := io.ReadAll(pollResponse.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(connectPacket), `40{"sid"`) {
		t.Fatalf("namespace connect response = %q", connectPacket)
	}
}

func TestAnonymousRealtimePayloadsAreBoundedBeforeSocketParsing(t *testing.T) {
	backend, err := NewWithDependencies(config.Config{NodeEnv: "test", Host: "localhost"}, testAuthenticator{}, testDocumentSync{})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	server := httptest.NewServer(backend.Handler())
	defer server.Close()

	client := newPollingClient(t, server.URL, "")
	client.connect()
	request := func(body string, contentLength int64) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, server.URL+"/socket.io/?EIO=4&transport=polling&sid="+url.QueryEscape(client.sid), strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.ContentLength = contentLength
		req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
		response, err := client.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}

	oversized := strings.Repeat("x", int(anonymousRealtimeMaxPayloadSize+1))
	for _, contentLength := range []int64{int64(len(oversized)), -1} {
		response := request(oversized, contentLength)
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("anonymous payload status for content length %d = %d, want 413", contentLength, response.StatusCode)
		}
	}
}

func TestAuthenticatedRealtimePollingKeepsConfiguredLargePayloadLimit(t *testing.T) {
	backend, err := NewWithDependencies(config.Config{NodeEnv: "test", Host: "localhost", WebsocketMaxHttpBufferSize: anonymousRealtimeMaxPayloadSize * 2}, testAuthenticator{}, testDocumentSync{})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	server := httptest.NewServer(backend.Handler())
	defer server.Close()

	client := newPollingClient(t, server.URL, "valid-session")
	client.connect()
	body := `42["unknown",{"padding":"` + strings.Repeat("x", int(anonymousRealtimeMaxPayloadSize)) + `"}]`
	client.post(body)
}

func TestAnonymousRealtimeWebsocketUpgradeIsRejectedBeforeFrameParsing(t *testing.T) {
	backend, err := NewWithDependencies(config.Config{NodeEnv: "test", Host: "localhost"}, testAuthenticator{}, testDocumentSync{})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	server := httptest.NewServer(backend.Handler())
	defer server.Close()

	request, err := http.NewRequest(http.MethodGet, server.URL+"/socket.io/?EIO=4&transport=websocket", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Sec-WebSocket-Version", "13")
	request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous websocket handshake = %d, want 401", response.StatusCode)
	}
}

func TestWebsocketUpgradeAuthenticatesUnknownPollingSession(t *testing.T) {
	backend, err := NewWithDependencies(config.Config{NodeEnv: "test", Host: "localhost"}, testAuthenticator{}, testDocumentSync{})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()

	request := httptest.NewRequest(http.MethodGet, "/socket.io/?EIO=4&transport=websocket&sid=pending-sid", nil)
	request.AddCookie(&http.Cookie{Name: "session", Value: "valid-session"})
	response := httptest.NewRecorder()
	ctx := types.NewHttpContext(response, request)
	defer ctx.Flush()

	called := false
	backend.guardIncomingPayloads(ctx, func(err error) {
		if err != nil {
			t.Errorf("middleware error = %v", err)
		}
		called = true
	})
	if !called {
		t.Fatalf("authenticated upgrade with unregistered SID was rejected with status %d", response.Code)
	}
}

func TestAnonymousPollingSessionCannotUpgradeWithUnknownNamespaceState(t *testing.T) {
	backend, err := NewWithDependencies(config.Config{NodeEnv: "test", Host: "localhost"}, testAuthenticator{}, testDocumentSync{})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()

	request := httptest.NewRequest(http.MethodGet, "/socket.io/?EIO=4&transport=websocket&sid=pending-sid", nil)
	response := httptest.NewRecorder()
	ctx := types.NewHttpContext(response, request)
	defer ctx.Flush()

	called := false
	backend.guardIncomingPayloads(ctx, func(error) { called = true })
	if called || response.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous upgrade middleware called=%t status=%d, want false/401", called, response.Code)
	}
}

func TestAuthenticatedCookieCannotUpgradeExplicitlyAnonymousSession(t *testing.T) {
	backend, err := NewWithDependencies(config.Config{NodeEnv: "test", Host: "localhost"}, testAuthenticator{}, testDocumentSync{})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	backend.authenticatedClients.Store("anonymous-sid", false)

	request := httptest.NewRequest(http.MethodGet, "/socket.io/?EIO=4&transport=websocket&sid=anonymous-sid", nil)
	request.AddCookie(&http.Cookie{Name: "session", Value: "valid-session"})
	response := httptest.NewRecorder()
	ctx := types.NewHttpContext(response, request)
	defer ctx.Flush()

	called := false
	backend.guardIncomingPayloads(ctx, func(error) { called = true })
	if called || response.Code != http.StatusUnauthorized {
		t.Fatalf("explicitly anonymous upgrade middleware called=%t status=%d, want false/401", called, response.Code)
	}
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func TestAuthenticatedUserspaceSyncSocketIOContract(t *testing.T) {
	backend, err := NewWithDependencies(config.Config{NodeEnv: "test", Namespace: "dev", Host: "localhost"}, testAuthenticator{}, testDocumentSync{})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	server := httptest.NewServer(backend.Handler())
	defer server.Close()

	client := newPollingClient(t, server.URL, "valid-session")
	if !strings.Contains(client.connect(), "40{") {
		t.Fatal("expected authenticated Socket.IO namespace connection")
	}

	join := client.emitAck(t, 1, "space:join-batch", map[string]any{
		"spaces":        []any{map[string]any{"spaceType": "userspace", "spaceId": "user-1", "docId": "page:doc-1"}},
		"clientVersion": "0.27.5",
	})
	if !strings.Contains(join, `"data":{"clientId"`) || !strings.Contains(join, `"success":true`) {
		t.Fatalf("join acknowledgement = %q", join)
	}

	pushed := client.emitAck(t, 2, "space:push-doc-update", map[string]any{
		"spaceType": "userspace", "spaceId": "user-1", "docId": "doc-1", "update": "AQ==",
	})
	if !strings.Contains(pushed, `"data":{"accepted":true,"timestamp":1234}`) {
		t.Fatalf("push acknowledgement = %q", pushed)
	}

	loaded := client.emitAck(t, 3, "space:load-doc", map[string]any{
		"spaceType": "userspace", "spaceId": "user-1", "docId": "doc-1",
	})
	if !strings.Contains(loaded, `"missing":"AQI="`) || !strings.Contains(loaded, `"state":"AwQ="`) {
		t.Fatalf("load acknowledgement = %q", loaded)
	}

	badLeave := client.emitAck(t, 4, "space:leave-batch", map[string]any{
		"spaceType": "userspace", "spaceId": "user-1", "docIds": []any{"doc-1", ""},
	})
	if !strings.Contains(badLeave, `"name":"BAD_REQUEST"`) {
		t.Fatalf("invalid leave-batch acknowledgement = %q", badLeave)
	}
	stillLoaded := client.emitAck(t, 5, "space:load-doc", map[string]any{
		"spaceType": "userspace", "spaceId": "user-1", "docId": "doc-1",
	})
	if !strings.Contains(stillLoaded, `"missing":"AQI="`) {
		t.Fatalf("load after rejected leave-batch = %q", stillLoaded)
	}

	deleted := client.emitAck(t, 6, "space:delete-doc", map[string]any{
		"spaceType": "userspace", "spaceId": "user-1", "docId": "page:doc-1",
	})
	if !strings.Contains(deleted, `"data":{"success":true}`) {
		t.Fatalf("delete acknowledgement = %q", deleted)
	}

	left := client.emitAck(t, 7, "space:leave", map[string]any{
		"spaceType": "userspace", "spaceId": "user-1",
	})
	if !strings.Contains(left, `"data":{"clientId"`) || !strings.Contains(left, `"success":true`) {
		t.Fatalf("leave acknowledgement = %q", left)
	}
	noLongerSubscribed := client.emitAck(t, 8, "space:load-doc", map[string]any{
		"spaceType": "userspace", "spaceId": "user-1", "docId": "doc-1",
	})
	if !strings.Contains(noLongerSubscribed, `"name":"NOT_IN_SPACE"`) {
		t.Fatalf("load after leave acknowledgement = %q", noLongerSubscribed)
	}
}

func TestRealtimeOperationsDoNotUseCompletedHandshakeContext(t *testing.T) {
	backend, err := NewWithDependencies(config.Config{NodeEnv: "test", Namespace: "dev", Host: "localhost"}, contextAwareAuthenticator{}, testDocumentSync{})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	server := httptest.NewServer(backend.Handler())
	defer server.Close()

	client := newPollingClient(t, server.URL, "valid-session")
	if connected := client.connect(); !strings.Contains(connected, "40{") {
		t.Fatalf("namespace connection = %q, want authenticated connection", connected)
	}

	profile := client.emitAck(t, 1, "realtime:request", map[string]any{
		"op": "user.profile.get", "input": map[string]any{}, "clientVersion": "0.27.5",
	})
	if !strings.Contains(profile, `"data":{"user":`) || !strings.Contains(profile, `"id":"user-1"`) {
		t.Fatalf("profile response = %q, want authenticated profile", profile)
	}
}

func TestWorkspaceSyncSocketIOEnforcesReadUpdateAndGeneration(t *testing.T) {
	documents := &testWorkspaceDocumentSync{}
	documents.generation.Store(7)
	documents.workspaceMetadataChanged.Store(true)
	backend, err := NewWithDependencies(
		config.Config{NodeEnv: "test", Host: "localhost"},
		testAuthenticator{},
		testDocumentSync{},
		WorkspaceDependencies{
			Authorizer: testWorkspaceAuthorizer{access: workspaces.WorkspaceSyncAccess{
				CanSync:    true,
				Generation: 7,
				Documents: map[string]workspaces.DocumentSyncAccess{
					"doc-1":       {Read: true, Update: true},
					"workspace-1": {Read: true, Update: true},
				},
			}},
			Documents: documents,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	server := httptest.NewServer(backend.Handler())
	defer server.Close()

	client := newPollingClient(t, server.URL, "valid-session")
	if !strings.Contains(client.connect(), "40{") {
		t.Fatal("expected authenticated Socket.IO namespace connection")
	}

	join := client.emitAck(t, 1, "space:join-batch", map[string]any{
		"spaces":        []any{map[string]any{"spaceType": "workspace", "spaceId": "workspace-1", "docId": "doc-1"}},
		"clientVersion": "0.27.5",
	})
	if !strings.Contains(join, `"data":{"clientId"`) || !strings.Contains(join, `"success":true`) {
		t.Fatalf("workspace join acknowledgement = %q", join)
	}

	loaded := client.emitAck(t, 2, "space:load-doc", map[string]any{
		"spaceType": "workspace", "spaceId": "workspace-1", "docId": "doc-1",
	})
	if !strings.Contains(loaded, `"missing":"AQI="`) || !strings.Contains(loaded, `"state":"AwQ="`) {
		t.Fatalf("workspace load acknowledgement = %q", loaded)
	}
	configSubscription := client.emitAck(t, 10, "realtime:subscribe", map[string]any{
		"topic": "workspace.config.changed", "input": map[string]any{"workspaceId": "workspace-1"}, "clientVersion": "0.26.0",
	})
	if !strings.Contains(configSubscription, `"subscriptionId":`) {
		t.Fatalf("workspace config subscription = %q", configSubscription)
	}

	pushed := client.emitAck(t, 3, "space:push-doc-update", map[string]any{
		"spaceType": "workspace", "spaceId": "workspace-1", "docId": "doc-1", "update": "AQ==",
	})
	if !strings.Contains(pushed, `"data":{"accepted":true,"timestamp":1234}`) {
		t.Fatalf("workspace push acknowledgement = %q", pushed)
	}
	if got := documents.lastExpectedGeneration.Load(); got != 7 {
		t.Fatalf("push expected generation = %d, want 7", got)
	}
	if strings.Contains(pushed, `"topic":"workspace.config.changed"`) {
		t.Fatalf("ordinary document update published workspace config change: %q", pushed)
	}
	rootJoin := client.emitAck(t, 11, "space:join-batch", map[string]any{
		"spaces":        []any{map[string]any{"spaceType": "workspace", "spaceId": "workspace-1", "docId": "workspace-1"}},
		"clientVersion": "0.27.5",
	})
	if !strings.Contains(rootJoin, `"success":true`) {
		t.Fatalf("workspace root join = %q", rootJoin)
	}
	rootPush := client.emitAckAndPollUntilContains(t, 12, "space:push-doc-update", map[string]any{
		"spaceType": "workspace", "spaceId": "workspace-1", "docId": "workspace-1", "update": "AQ==",
	}, `"data":{"accepted":true,"timestamp":1234}`, `"topic":"workspace.config.changed"`, `"reason":"workspace-updated"`)
	if !strings.Contains(rootPush, `"data":{"accepted":true,"timestamp":1234}`) || !strings.Contains(rootPush, `"topic":"workspace.config.changed"`) || !strings.Contains(rootPush, `"reason":"workspace-updated"`) {
		t.Fatalf("root profile update response/config event = %q", rootPush)
	}

	documents.generation.Store(8)
	stale := client.emitAck(t, 4, "space:load-doc", map[string]any{
		"spaceType": "workspace", "spaceId": "workspace-1", "docId": "doc-1",
	})
	if !strings.Contains(stale, `"name":"NOT_IN_SPACE"`) {
		t.Fatalf("load after permission generation change = %q", stale)
	}
	if got := documents.pushCalls.Load(); got != 2 {
		t.Fatalf("workspace pushes = %d, want 2", got)
	}
}

func TestWorkspaceDocumentLifecycleRequiresManagerAndNeedsNoJoin(t *testing.T) {
	for _, test := range []struct {
		name       string
		manage     bool
		wantDenied bool
	}{
		{name: "manager can trash without a sync subscription", manage: true},
		{name: "editor cannot trash", manage: false, wantDenied: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			documents := &testWorkspaceDocumentSync{}
			documents.generation.Store(7)
			backend, err := NewWithDependencies(
				config.Config{NodeEnv: "test", Host: "localhost"},
				testAuthenticator{},
				testDocumentSync{},
				WorkspaceDependencies{
					Authorizer: testWorkspaceAuthorizer{access: workspaces.WorkspaceSyncAccess{
						CanSync: true, Generation: 7,
						Documents: map[string]workspaces.DocumentSyncAccess{"doc-1": {Read: true, Update: true, Manage: test.manage}},
					}},
					Documents: documents,
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			defer backend.Close()
			server := httptest.NewServer(backend.Handler())
			defer server.Close()
			client := newPollingClient(t, server.URL, "valid-session")
			if !strings.Contains(client.connect(), "40{") {
				t.Fatal("expected authenticated Socket.IO connection")
			}

			response := client.emitAck(t, 1, "space:doc-lifecycle", map[string]any{
				"spaceType": "workspace", "spaceId": "workspace-1", "docId": "doc-1", "lifecycle": "trash",
			})
			if test.wantDenied {
				if !strings.Contains(response, `"name":"DOC_ACTION_DENIED"`) || documents.lifecycleCalls.Load() != 0 {
					t.Fatalf("editor lifecycle response = %q, calls=%d", response, documents.lifecycleCalls.Load())
				}
				return
			}
			if !strings.Contains(response, `"rootUpdate":"AQI="`) || !strings.Contains(response, `"timestamp":1234`) {
				t.Fatalf("lifecycle acknowledgement = %q", response)
			}
			if got := documents.lastExpectedGeneration.Load(); got != 7 {
				t.Fatalf("lifecycle expected generation = %d, want 7", got)
			}

			deleted := client.emitAck(t, 2, "space:delete-doc", map[string]any{
				"spaceType": "workspace", "spaceId": "workspace-1", "docId": "doc-1",
			})
			if !strings.Contains(deleted, `"data":{"success":true}`) || documents.lifecycleCalls.Load() != 2 {
				t.Fatalf("workspace delete acknowledgement = %q, lifecycle calls=%d", deleted, documents.lifecycleCalls.Load())
			}
		})
	}
}

func TestWorkspaceTimestampAuthorizationRevokesStaleSubscription(t *testing.T) {
	documents := &testWorkspaceDocumentSync{}
	documents.generation.Store(7)
	authorizer := &changingWorkspaceAuthorizer{generation: 7}
	backend, err := NewWithDependencies(
		config.Config{NodeEnv: "test", Host: "localhost"},
		testAuthenticator{},
		testDocumentSync{},
		WorkspaceDependencies{Authorizer: authorizer, Documents: documents},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	server := httptest.NewServer(backend.Handler())
	defer server.Close()
	client := newPollingClient(t, server.URL, "valid-session")
	if !strings.Contains(client.connect(), "40{") {
		t.Fatal("expected authenticated Socket.IO namespace connection")
	}
	joined := client.emitAck(t, 1, "space:join-batch", map[string]any{
		"spaces":        []any{map[string]any{"spaceType": "workspace", "spaceId": "workspace-1"}},
		"clientVersion": "0.27.5",
	})
	if !strings.Contains(joined, `"success":true`) {
		t.Fatalf("workspace join = %q", joined)
	}

	stale := client.emitAck(t, 2, "space:load-doc-timestamps", map[string]any{
		"spaceType": "workspace", "spaceId": "workspace-1",
	})
	if !strings.Contains(stale, `"name":"NOT_IN_SPACE"`) {
		t.Fatalf("timestamp load after ACL generation change = %q", stale)
	}
	noLongerSubscribed := client.emitAck(t, 3, "space:load-doc-timestamps", map[string]any{
		"spaceType": "workspace", "spaceId": "workspace-1",
	})
	if !strings.Contains(noLongerSubscribed, `"name":"NOT_IN_SPACE"`) {
		t.Fatalf("timestamp load after stale subscription eviction = %q", noLongerSubscribed)
	}
}

func TestWorkspaceJoinRequiresSyncAndReadPermissions(t *testing.T) {
	for _, test := range []struct {
		name   string
		access workspaces.WorkspaceSyncAccess
		want   string
	}{
		{
			name: "workspace sync denied",
			access: workspaces.WorkspaceSyncAccess{
				CanSync: false, Generation: 1,
				Documents: map[string]workspaces.DocumentSyncAccess{"doc-1": {Read: true}},
			},
			want: "SPACE_ACCESS_DENIED",
		},
		{
			name: "document read denied",
			access: workspaces.WorkspaceSyncAccess{
				CanSync: true, Generation: 1,
				Documents: map[string]workspaces.DocumentSyncAccess{"doc-1": {Read: false}},
			},
			want: "DOC_ACTION_DENIED",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			documents := &testWorkspaceDocumentSync{}
			documents.generation.Store(1)
			backend, err := NewWithDependencies(
				config.Config{NodeEnv: "test", Host: "localhost"},
				testAuthenticator{},
				testDocumentSync{},
				WorkspaceDependencies{
					Authorizer: testWorkspaceAuthorizer{access: test.access},
					Documents:  documents,
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			defer backend.Close()
			server := httptest.NewServer(backend.Handler())
			defer server.Close()

			client := newPollingClient(t, server.URL, "valid-session")
			if !strings.Contains(client.connect(), "40{") {
				t.Fatal("expected authenticated Socket.IO namespace connection")
			}
			response := client.emitAck(t, 1, "space:join-batch", map[string]any{
				"spaces":        []any{map[string]any{"spaceType": "workspace", "spaceId": "workspace-1", "docId": "doc-1"}},
				"clientVersion": "0.27.5",
			})
			if !strings.Contains(response, `"name":"`+test.want+`"`) {
				t.Fatalf("workspace join error = %q, want %s", response, test.want)
			}
		})
	}
}

func TestSocketIOAllowsUnauthenticatedProfileBootstrap(t *testing.T) {
	backend, err := NewWithDependencies(config.Config{NodeEnv: "test", Host: "localhost"}, testAuthenticator{}, testDocumentSync{})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	server := httptest.NewServer(backend.Handler())
	defer server.Close()

	client := newPollingClient(t, server.URL, "")
	response := client.connect()
	if !strings.Contains(response, `40{`) {
		t.Fatalf("unauthenticated namespace response = %q", response)
	}
	profile := client.emitAck(t, 1, "realtime:request", map[string]any{
		"op": "user.profile.get", "input": map[string]any{}, "clientVersion": "0.26.0",
	})
	if !strings.Contains(profile, `"data":{"user":null}`) {
		t.Fatalf("anonymous profile bootstrap = %q", profile)
	}
	join := client.emitAck(t, 2, "space:join-batch", map[string]any{
		"spaces": []any{map[string]any{"spaceType": "userspace", "spaceId": "user-1"}}, "clientVersion": "0.27.5",
	})
	if !strings.Contains(join, `"name":"UNAUTHORIZED"`) {
		t.Fatalf("unauthenticated sync event = %q", join)
	}
}

func TestRealtimeProfileRequestSubscribeAndUnsubscribe(t *testing.T) {
	backend, err := NewWithDependencies(config.Config{NodeEnv: "test", Host: "localhost"}, testAuthenticator{}, testDocumentSync{})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	server := httptest.NewServer(backend.Handler())
	defer server.Close()

	client := newPollingClient(t, server.URL, "valid-session")
	if !strings.Contains(client.connect(), "40{") {
		t.Fatal("expected authenticated Socket.IO namespace connection")
	}
	otherClient := newPollingClient(t, server.URL, "other-session")
	if !strings.Contains(otherClient.connect(), "40{") {
		t.Fatal("expected second authenticated Socket.IO namespace connection")
	}
	profile := client.emitAck(t, 1, "realtime:request", map[string]any{
		"op": "user.profile.get", "input": map[string]any{}, "clientVersion": "0.26.0",
	})
	for _, field := range []string{`"id":"user-1"`, `"name":"Current User"`, `"email":"current@example.test"`, `"emailVerified":true`, `"hasPassword":true`, `"avatarUrl":"https://example.test/avatar.png"`, `"features":["Admin"]`} {
		if !strings.Contains(profile, field) {
			t.Fatalf("profile response missing %s: %q", field, profile)
		}
	}

	subscription := client.emitAck(t, 2, "realtime:subscribe", map[string]any{
		"topic": "user.profile.changed", "input": map[string]any{}, "clientVersion": "0.26.0",
	})
	prefix := `"subscriptionId":"`
	start := strings.Index(subscription, prefix)
	if start < 0 {
		t.Fatalf("profile subscription acknowledgement = %q", subscription)
	}
	start += len(prefix)
	end := strings.Index(subscription[start:], `"`)
	if end < 0 {
		t.Fatalf("profile subscription id is malformed: %q", subscription)
	}
	subscriptionID := subscription[start : start+end]
	otherSubscription := otherClient.emitAck(t, 1, "realtime:subscribe", map[string]any{
		"topic": "user.profile.changed", "input": map[string]any{}, "clientVersion": "0.26.0",
	})
	if !strings.Contains(otherSubscription, `"data":{"subscriptionId":`) {
		t.Fatalf("second profile subscription acknowledgement = %q", otherSubscription)
	}

	backend.PublishUserProfileChanged("user-1")
	event := client.poll()
	for _, field := range []string{`"topic":"user.profile.changed"`, `"inputKey":"{}"`, `"changed":true`, `"reason":"user-updated"`} {
		if !strings.Contains(event, field) {
			t.Fatalf("profile event missing %s: %q", field, event)
		}
	}
	otherClient.client.Timeout = 150 * time.Millisecond
	if packet, err := otherClient.pollWithError(); err == nil {
		t.Fatalf("another user's profile event was received: %q", packet)
	}

	unsubscribe := client.emitAck(t, 3, "realtime:unsubscribe", map[string]any{
		"subscriptionId": subscriptionID, "topic": "user.profile.changed", "input": map[string]any{}, "clientVersion": "0.26.0",
	})
	if !strings.Contains(unsubscribe, `"data":{"ok":true}`) {
		t.Fatalf("profile unsubscribe acknowledgement = %q", unsubscribe)
	}
	backend.PublishUserProfileChanged("user-1")
	client.client.Timeout = 150 * time.Millisecond
	if packet, err := client.pollWithError(); err == nil {
		t.Fatalf("received profile event after unsubscribe: %q", packet)
	}
}

func TestSupportedRealtimeClientVersion(t *testing.T) {
	now := time.Date(2026, time.October, 17, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		version string
		canary  bool
		want    bool
	}{
		{version: "0.26.0", want: true},
		{version: "v0.26.0-rc.1", want: true},
		{version: "0.25.9", want: false},
		{version: "not-semver", want: false},
		{version: "2026.10.16-canary.015", canary: true, want: true},
		{version: "2026.10.16-canary.015", want: false},
		{version: "2026.07.01-canary.015", canary: true, want: false},
	} {
		if got := supportedRealtimeVersion(test.version, test.canary, now); got != test.want {
			t.Errorf("supportedRealtimeVersion(%q, %t) = %t, want %t", test.version, test.canary, got, test.want)
		}
	}
}

func TestSocketIOSyncRevalidatesRevokedSession(t *testing.T) {
	authenticator := &revocableAuthenticator{}
	backend, err := NewWithDependencies(config.Config{NodeEnv: "test", Host: "localhost"}, authenticator, testDocumentSync{})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	server := httptest.NewServer(backend.Handler())
	defer server.Close()

	client := newPollingClient(t, server.URL, "valid-session")
	if !strings.Contains(client.connect(), "40{") {
		t.Fatal("expected authenticated Socket.IO namespace connection")
	}
	join := client.emitAck(t, 1, "space:join-batch", map[string]any{
		"spaces":        []any{map[string]any{"spaceType": "userspace", "spaceId": "user-1", "docId": "doc-1"}},
		"clientVersion": "0.27.5",
	})
	if !strings.Contains(join, `"success":true`) {
		t.Fatalf("join acknowledgement = %q", join)
	}

	authenticator.revoked.Store(true)
	response := client.emitAck(t, 2, "space:push-doc-update", map[string]any{
		"spaceType": "userspace", "spaceId": "user-1", "docId": "doc-1", "update": "AQ==",
	})
	if !strings.Contains(response, `"name":"UNAUTHORIZED"`) {
		t.Fatalf("revoked session acknowledgement = %q", response)
	}
}

func TestSocketIORejectsOldSyncVersionWithLegacyAcknowledgement(t *testing.T) {
	backend, err := NewWithDependencies(config.Config{NodeEnv: "test", Host: "localhost"}, testAuthenticator{}, testDocumentSync{})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	server := httptest.NewServer(backend.Handler())
	defer server.Close()

	client := newPollingClient(t, server.URL, "valid-session")
	if !strings.Contains(client.connect(), "40{") {
		t.Fatal("expected authenticated Socket.IO namespace connection")
	}
	response := client.emitAck(t, 1, "space:join-batch", map[string]any{
		"spaces":        []any{map[string]any{"spaceType": "userspace", "spaceId": "user-1"}},
		"clientVersion": "0.27.4",
	})
	if !strings.Contains(response, `"data":{"clientId"`) || !strings.Contains(response, `"success":false`) {
		t.Fatalf("old client version acknowledgement = %q", response)
	}
}

func TestUserspacePushInvalidatesSpaceSubscribers(t *testing.T) {
	backend, err := NewWithDependencies(config.Config{NodeEnv: "test", Host: "localhost"}, testAuthenticator{}, testDocumentSync{})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	server := httptest.NewServer(backend.Handler())
	defer server.Close()

	documentClient := newPollingClient(t, server.URL, "valid-session")
	if !strings.Contains(documentClient.connect(), "40{") {
		t.Fatal("expected authenticated document namespace connection")
	}
	spaceClient := newPollingClient(t, server.URL, "valid-session")
	if !strings.Contains(spaceClient.connect(), "40{") {
		t.Fatal("expected authenticated space namespace connection")
	}

	joinDoc := documentClient.emitAck(t, 1, "space:join-batch", map[string]any{
		"spaces":        []any{map[string]any{"spaceType": "userspace", "spaceId": "user-1", "docId": "doc-1"}},
		"clientVersion": "0.27.5",
	})
	if !strings.Contains(joinDoc, `"success":true`) {
		t.Fatalf("document join acknowledgement = %q", joinDoc)
	}
	joinSpace := spaceClient.emitAck(t, 1, "space:join-batch", map[string]any{
		"spaces":        []any{map[string]any{"spaceType": "userspace", "spaceId": "user-1"}},
		"clientVersion": "0.27.5",
	})
	if !strings.Contains(joinSpace, `"success":true`) {
		t.Fatalf("space join acknowledgement = %q", joinSpace)
	}

	push := documentClient.emitAck(t, 2, "space:push-doc-update", map[string]any{
		"spaceType": "userspace", "spaceId": "user-1", "docId": "doc-1", "update": "AQ==",
	})
	if !strings.Contains(push, `"accepted":true`) {
		t.Fatalf("push acknowledgement = %q", push)
	}
	invalidation := spaceClient.poll()
	if !strings.Contains(invalidation, `space:broadcast-doc-invalidation`) || !strings.Contains(invalidation, `"spaceId":"user-1"`) {
		t.Fatalf("space invalidation event = %q", invalidation)
	}
}

func TestBatchSyncVersionCompatibility(t *testing.T) {
	now := time.Date(2026, time.October, 16, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		version string
		canary  bool
		want    bool
	}{
		{version: "0.27.5-0", want: true},
		{version: "0.27.4", want: false},
		{version: "2026.10.16-canary.015", canary: true, want: true},
		{version: "2026.10.16-canary.015", want: false},
		{version: "2026.08.01-canary.015", canary: true, want: false},
		{version: "2026.10.20-canary.015", canary: true, want: false},
	} {
		if got := supportedSyncVersion(test.version, test.canary, now); got != test.want {
			t.Errorf("supportedSyncVersion(%q, %t) = %t, want %t", test.version, test.canary, got, test.want)
		}
	}
}

func TestJoinBatchEnforcesUserspaceOwnershipAndUniqueness(t *testing.T) {
	valid := map[string]any{
		"spaces":        []any{map[string]any{"spaceType": "userspace", "spaceId": "user-1", "docId": "space:page:doc-1"}},
		"clientVersion": "0.27.5",
	}
	entries, err := parseJoinBatch(valid, "user-1", false)
	if err != nil || len(entries) != 1 || entries[0].docID != "doc-1" {
		t.Fatalf("parseJoinBatch() = %#v, %v", entries, err)
	}

	wrongOwner := map[string]any{"spaces": []any{map[string]any{"spaceType": "userspace", "spaceId": "user-2"}}, "clientVersion": "0.27.5"}
	if _, err := parseJoinBatch(wrongOwner, "user-1", false); err == nil || err.(syncProtocolError).name != "SPACE_ACCESS_DENIED" {
		t.Fatalf("wrong-owner join error = %v", err)
	}
	workspace := map[string]any{"spaces": []any{map[string]any{"spaceType": "workspace", "spaceId": "user-1"}}, "clientVersion": "0.27.5"}
	workspaceEntries, err := parseJoinBatch(workspace, "user-1", false)
	if err != nil || len(workspaceEntries) != 1 || workspaceEntries[0].spaceType != "workspace" {
		t.Fatalf("workspace join = %#v, %v", workspaceEntries, err)
	}
	duplicate := map[string]any{"spaces": []any{
		map[string]any{"spaceType": "userspace", "spaceId": "user-1", "docId": "doc-1"},
		map[string]any{"spaceType": "userspace", "spaceId": "user-1", "docId": "doc-1"},
	}, "clientVersion": "0.27.5"}
	if _, err := parseJoinBatch(duplicate, "user-1", false); err == nil {
		t.Fatal("duplicate document join was accepted")
	}
}

type testAuthenticator struct{}

func (testAuthenticator) FromRequest(_ context.Context, request *http.Request) (*auth.Principal, error) {
	cookie, err := request.Cookie("session")
	if err != nil || (cookie.Value != "valid-session" && cookie.Value != "other-session") {
		return nil, auth.ErrNoSession
	}
	if cookie.Value == "other-session" {
		return &auth.Principal{SessionID: cookie.Value, UserSessionID: "user-session-2", User: auth.User{ID: "user-2"}}, nil
	}
	avatarURL := "https://example.test/avatar.png"
	return &auth.Principal{SessionID: cookie.Value, UserSessionID: "user-session-1", User: auth.User{
		ID: "user-1", Name: "Current User", Email: "current@example.test", AvatarURL: &avatarURL,
		EmailVerified: true, HasPassword: true, Features: []string{"administrator"},
	}}, nil
}

type contextAwareAuthenticator struct{}

func (contextAwareAuthenticator) FromRequest(ctx context.Context, request *http.Request) (*auth.Principal, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return testAuthenticator{}.FromRequest(ctx, request)
}

type revocableAuthenticator struct {
	revoked atomic.Bool
}

func (a *revocableAuthenticator) FromRequest(_ context.Context, request *http.Request) (*auth.Principal, error) {
	cookie, err := request.Cookie("session")
	if err != nil || cookie.Value != "valid-session" || a.revoked.Load() {
		return nil, auth.ErrNoSession
	}
	return &auth.Principal{SessionID: cookie.Value, UserSessionID: "user-session-1", User: auth.User{ID: "user-1"}}, nil
}

type testWorkspaceAuthorizer struct {
	access workspaces.WorkspaceSyncAccess
}

func (a testWorkspaceAuthorizer) AuthorizeWorkspaceSync(context.Context, string, string, []string) (workspaces.WorkspaceSyncAccess, error) {
	return a.access, nil
}

type changingWorkspaceAuthorizer struct {
	calls      atomic.Int32
	generation int64
}

func (a *changingWorkspaceAuthorizer) AuthorizeWorkspaceSync(_ context.Context, _, _ string, docIDs []string) (workspaces.WorkspaceSyncAccess, error) {
	generation := a.generation
	if a.calls.Add(1) > 1 {
		generation++
	}
	documents := make(map[string]workspaces.DocumentSyncAccess, len(docIDs))
	for _, docID := range docIDs {
		documents[docID] = workspaces.DocumentSyncAccess{Read: true}
	}
	return workspaces.WorkspaceSyncAccess{CanSync: true, Generation: generation, Documents: documents}, nil
}

type testWorkspaceDocumentSync struct {
	generation               atomic.Int64
	lastExpectedGeneration   atomic.Int64
	pushCalls                atomic.Int32
	lifecycleCalls           atomic.Int32
	workspaceMetadataChanged atomic.Bool
}

func (d *testWorkspaceDocumentSync) CurrentPermissionGeneration(context.Context, string) (int64, error) {
	return d.generation.Load(), nil
}

func (d *testWorkspaceDocumentSync) PushWorkspace(_ context.Context, _, workspaceID, docID string, _ []byte, expectedGeneration int64) (documents.WorkspacePushResult, error) {
	d.lastExpectedGeneration.Store(expectedGeneration)
	d.pushCalls.Add(1)
	if d.generation.Load() != expectedGeneration {
		return documents.WorkspacePushResult{}, workspaces.ErrPermissionGenerationChanged
	}
	return documents.WorkspacePushResult{Timestamp: 1234, WorkspaceMetadataChanged: workspaceID == docID && d.workspaceMetadataChanged.Load()}, nil
}

func (d *testWorkspaceDocumentSync) ApplyWorkspaceDocLifecycle(_ context.Context, _, _, _ string, _ documents.WorkspaceDocLifecycle, expectedGeneration int64) (documents.WorkspaceDocLifecycleResult, error) {
	d.lastExpectedGeneration.Store(expectedGeneration)
	if d.generation.Load() != expectedGeneration {
		return documents.WorkspaceDocLifecycleResult{}, documents.ErrWorkspacePermissionGenerationChanged
	}
	d.lifecycleCalls.Add(1)
	return documents.WorkspaceDocLifecycleResult{RootUpdate: []byte{1, 2}, Timestamp: 1234}, nil
}

func (*testWorkspaceDocumentSync) DiffWorkspace(context.Context, string, string, []byte) ([]byte, []byte, int64, error) {
	return []byte{1, 2}, []byte{3, 4}, 1234, nil
}

func (*testWorkspaceDocumentSync) WorkspaceTimestamps(context.Context, string, *int64) (map[string]int64, error) {
	return map[string]int64{"doc-1": 1234}, nil
}

type testDocumentSync struct{}

func (testDocumentSync) Push(context.Context, string, string, []byte) (int64, error) {
	return 1234, nil
}

func (testDocumentSync) Diff(context.Context, string, string, []byte) ([]byte, []byte, int64, error) {
	return []byte{1, 2}, []byte{3, 4}, 1234, nil
}

func (testDocumentSync) Timestamps(context.Context, string, *int64) (map[string]int64, error) {
	return map[string]int64{"doc-1": 1234}, nil
}

func (testDocumentSync) Delete(context.Context, string, string) error { return nil }

type pollingTestClient struct {
	t       *testing.T
	baseURL string
	client  *http.Client
	sid     string
}

func newPollingClient(t *testing.T, baseURL, cookie string) pollingTestClient {
	t.Helper()
	serverURL, err := url.Parse(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	client := pollingTestClient{t: t, baseURL: baseURL, client: &http.Client{Timeout: 5 * time.Second}}
	request, err := http.NewRequest(http.MethodGet, serverURL.String()+"/socket.io/?EIO=4&transport=polling", nil)
	if err != nil {
		t.Fatal(err)
	}
	if cookie != "" {
		request.AddCookie(&http.Cookie{Name: "session", Value: cookie})
	}
	response, err := client.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || len(body) < 2 || body[0] != '0' {
		t.Fatalf("Engine.IO handshake response = %d %q", response.StatusCode, body)
	}
	var open struct {
		SID string `json:"sid"`
	}
	if err := json.Unmarshal(body[1:], &open); err != nil {
		t.Fatal(err)
	}
	client.sid = open.SID
	return client
}

func (c pollingTestClient) connect() string {
	c.post("40")
	return c.poll()
}

func (c pollingTestClient) emitAck(t *testing.T, id int, event string, payload map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal([]any{event, payload})
	if err != nil {
		t.Fatal(err)
	}
	c.post("42" + strconv.Itoa(id) + string(encoded))
	return c.poll()
}

func (c pollingTestClient) emitAckAndPollUntilContains(t *testing.T, id int, event string, payload map[string]any, required ...string) string {
	t.Helper()
	encoded, err := json.Marshal([]any{event, payload})
	if err != nil {
		t.Fatal(err)
	}
	c.post("42" + strconv.Itoa(id) + string(encoded))
	var responses []string
	for attempt := 0; attempt < 4; attempt++ {
		response, err := c.pollWithError()
		if err != nil {
			break
		}
		responses = append(responses, response)
		combined := strings.Join(responses, "\n")
		allFound := true
		for _, item := range required {
			if !strings.Contains(combined, item) {
				allFound = false
				break
			}
		}
		if allFound {
			return combined
		}
	}
	t.Fatalf("Socket.IO acknowledgement/events did not contain %q: %q", required, strings.Join(responses, "\n"))
	return ""
}

func (c pollingTestClient) post(packet string) {
	c.t.Helper()
	request, err := http.NewRequest(http.MethodPost, c.baseURL+"/socket.io/?EIO=4&transport=polling&sid="+url.QueryEscape(c.sid), strings.NewReader(packet))
	if err != nil {
		c.t.Fatal(err)
	}
	request.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	response, err := c.client.Do(request)
	if err != nil {
		c.t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		c.t.Fatalf("Socket.IO POST status = %d", response.StatusCode)
	}
}

func (c pollingTestClient) poll() string {
	c.t.Helper()
	body, err := c.pollWithError()
	if err != nil {
		c.t.Fatal(err)
	}
	return body
}

func (c pollingTestClient) pollWithError() (string, error) {
	c.t.Helper()
	request, err := http.NewRequest(http.MethodGet, c.baseURL+"/socket.io/?EIO=4&transport=polling&sid="+url.QueryEscape(c.sid), nil)
	if err != nil {
		return "", err
	}
	response, err := c.client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return "", err
	}
	return string(bytes.TrimSpace(body)), nil
}
