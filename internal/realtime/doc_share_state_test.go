// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package realtime

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

type shareStateReaderFake struct{ state *workspaces.DocShareState }

func (f shareStateReaderFake) ReadDocShareState(context.Context, string, string) (*workspaces.DocShareState, error) {
	return f.state, nil
}

type shareStateAuthorizerFake struct{ allow bool }

func (f shareStateAuthorizerFake) AuthorizeDocument(context.Context, string, string, string) (workspaces.DocumentAuthorization, error) {
	return workspaces.DocumentAuthorization{Read: f.allow}, nil
}

func TestRealtimeDocShareStateAndDefaultRoleEvent(t *testing.T) {
	backend, err := NewWithDependencies(config.Config{NodeEnv: "test", Host: "localhost"}, testAuthenticator{}, testDocumentSync{}, WorkspaceDependencies{ShareState: shareStateReaderFake{&workspaces.DocShareState{Mode: "Page", DefaultRole: "None"}}, GrantsAuth: shareStateAuthorizerFake{true}})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	server := httptest.NewServer(backend.Handler())
	defer server.Close()
	client := newPollingClient(t, server.URL, "valid-session")
	assertPacketContains(t, client.connect(), "40{")
	input := map[string]any{"workspaceId": "workspace-1", "docId": "doc-1"}
	result := client.emitAck(t, 1, "realtime:request", map[string]any{"op": "doc.share-state.get", "input": input, "clientVersion": "0.26.0"})
	for _, part := range []string{`"state":{`, `"public":false`, `"mode":"Page"`, `"defaultRole":"None"`} {
		assertPacketContains(t, result, part)
	}
	subscribeRealtimeTopic(t, client, 2, "doc.share-state.changed", input)
	backend.PublishDocumentRoleChanged("workspace-1", "doc-1", "default-role-changed")
	assertRealtimeEvent(t, client.poll(), `"topic":"doc.share-state.changed"`, `"reason":"default-role-changed"`)
	denied, err := NewWithDependencies(config.Config{NodeEnv: "test", Host: "localhost"}, testAuthenticator{}, testDocumentSync{}, WorkspaceDependencies{ShareState: shareStateReaderFake{}, GrantsAuth: shareStateAuthorizerFake{false}})
	if err != nil {
		t.Fatal(err)
	}
	defer denied.Close()
	deniedServer := httptest.NewServer(denied.Handler())
	defer deniedServer.Close()
	deniedClient := newPollingClient(t, deniedServer.URL, "valid-session")
	deniedClient.connect()
	for i, event := range []string{"realtime:request", "realtime:subscribe"} {
		result := deniedClient.emitAck(t, i+1, event, map[string]any{"op": "doc.share-state.get", "topic": "doc.share-state.changed", "input": input, "clientVersion": "0.26.0"})
		if !strings.Contains(result, `"name":"DOC_ACTION_DENIED"`) {
			t.Fatalf("denied %s = %s", event, result)
		}
	}
}
