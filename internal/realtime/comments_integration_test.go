// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package realtime

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

func TestRealtimeCommentSubscriptionAuthorizesAndReceivesChanges(t *testing.T) {
	authorizer := testWorkspaceAuthorizer{access: workspaces.WorkspaceSyncAccess{
		CanSync: true, Documents: map[string]workspaces.DocumentSyncAccess{"doc-1": {Read: true}},
	}}
	backend, err := NewWithDependencies(
		config.Config{NodeEnv: "test", Host: "localhost"}, testAuthenticator{}, testDocumentSync{},
		WorkspaceDependencies{Authorizer: authorizer, Documents: &testWorkspaceDocumentSync{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	backend.SetCommentDependencies(&commentChangesRepositoryFake{}, authorizer)
	server := httptest.NewServer(backend.Handler())
	defer server.Close()

	client := newPollingClient(t, server.URL, "valid-session")
	if !strings.Contains(client.connect(), "40{") {
		t.Fatal("expected authenticated Socket.IO namespace connection")
	}
	ack := client.emitAck(t, 1, "realtime:subscribe", map[string]any{
		"topic": "comment.changed", "input": map[string]any{"workspaceId": "workspace-1", "docId": "doc-1"}, "clientVersion": "0.26.0",
	})
	if !strings.Contains(ack, `"subscriptionId":`) || !strings.Contains(ack, `comment.changed:{\"docId\":\"doc-1\",\"workspaceId\":\"workspace-1\"}`) {
		t.Fatalf("comment subscription acknowledgement = %q", ack)
	}
	denied := client.emitAck(t, 2, "realtime:subscribe", map[string]any{
		"topic": "comment.changed", "input": map[string]any{"workspaceId": "workspace-1", "docId": "doc-2"}, "clientVersion": "0.26.0",
	})
	if !strings.Contains(denied, `"name":"DOC_ACTION_DENIED"`) {
		t.Fatalf("unreadable comment subscription = %q", denied)
	}
	badCursor := client.emitAck(t, 3, "realtime:request", map[string]any{
		"op": "comment.changes.get", "input": map[string]any{"workspaceId": "workspace-1", "docId": "doc-1", "after": "%%%"}, "clientVersion": "0.26.0",
	})
	if !strings.Contains(badCursor, `"name":"BAD_REQUEST"`) {
		t.Fatalf("malformed comment cursor = %q", badCursor)
	}

	backend.PublishCommentChanged("workspace-1", "doc-1")
	event := client.poll()
	for _, field := range []string{`"topic":"comment.changed"`, `"changed":true`, `\"docId\":\"doc-1\"`, `\"workspaceId\":\"workspace-1\"`} {
		if !strings.Contains(event, field) {
			t.Fatalf("comment event missing %s: %q", field, event)
		}
	}
}
