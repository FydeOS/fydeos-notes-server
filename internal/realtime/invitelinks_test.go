// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package realtime

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/invitelinks"
)

type inviteLinkReaderFake struct {
	link        *invitelinks.Link
	workspaceID string
}

func (f *inviteLinkReaderFake) Get(_ context.Context, workspaceID string) (*invitelinks.Link, error) {
	f.workspaceID = workspaceID
	return f.link, nil
}

type inviteLinkUserManagerFake struct{}

func (inviteLinkUserManagerFake) CanManageWorkspaceUsers(_ context.Context, userID, _ string) (bool, error) {
	return userID == "user-1", nil
}

func TestRealtimeWorkspaceInviteLinkRequestAndSubscription(t *testing.T) {
	reader := &inviteLinkReaderFake{link: &invitelinks.Link{
		URL: "https://notes.example.test/invite/link-1", ExpiresAt: time.Date(2026, 10, 18, 12, 30, 0, 0, time.UTC),
	}}
	backend, err := NewWithDependencies(
		config.Config{NodeEnv: "test", Host: "localhost"}, testAuthenticator{}, testDocumentSync{},
		WorkspaceDependencies{InviteLinks: reader, UserManager: inviteLinkUserManagerFake{}},
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
	request := client.emitAck(t, 1, "realtime:request", map[string]any{
		"op": "workspace.invite-link.get", "input": map[string]any{"workspaceId": "workspace-1"}, "clientVersion": "0.26.0",
	})
	for _, field := range []string{`"inviteLink":{"expireTime":"2026-10-18T12:30:00Z","link":"https://notes.example.test/invite/link-1"}`} {
		if !strings.Contains(request, field) {
			t.Fatalf("invite-link request response missing %s: %q", field, request)
		}
	}
	if reader.workspaceID != "workspace-1" {
		t.Fatalf("reader workspace = %q", reader.workspaceID)
	}
	reader.link = nil

	deniedClient := newPollingClient(t, server.URL, "other-session")
	if !strings.Contains(deniedClient.connect(), "40{") {
		t.Fatal("expected second authenticated Socket.IO namespace connection")
	}
	denied := deniedClient.emitAck(t, 1, "realtime:request", map[string]any{
		"op": "workspace.invite-link.get", "input": map[string]any{"workspaceId": "workspace-1"}, "clientVersion": "0.26.0",
	})
	if !strings.Contains(denied, `"name":"SPACE_ACCESS_DENIED"`) {
		t.Fatalf("unauthorized invite-link request = %q", denied)
	}
	badInput := client.emitAck(t, 2, "realtime:request", map[string]any{
		"op": "workspace.invite-link.get", "input": map[string]any{"workspaceId": "workspace-1", "extra": true}, "clientVersion": "0.26.0",
	})
	if !strings.Contains(badInput, `"name":"BAD_REQUEST"`) {
		t.Fatalf("invalid invite-link request = %q", badInput)
	}

	subscription := client.emitAck(t, 3, "realtime:subscribe", map[string]any{
		"topic": "workspace.invite-link.changed", "input": map[string]any{"workspaceId": "workspace-1"}, "clientVersion": "0.26.0",
	})
	if !strings.Contains(subscription, `"subscriptionId":"`) || !strings.Contains(subscription, `workspace.invite-link.changed:{\"workspaceId\":\"workspace-1\"}`) {
		t.Fatalf("invite-link subscription acknowledgement = %q", subscription)
	}
	backend.PublishWorkspaceInviteLinkChanged("workspace-1", "invite-link-created")
	event := client.poll()
	for _, field := range []string{`"topic":"workspace.invite-link.changed"`, `"inputKey":"{\"workspaceId\":\"workspace-1\"}"`, `"changed":true`, `"reason":"invite-link-created"`} {
		if !strings.Contains(event, field) {
			t.Fatalf("invite-link event missing %s: %q", field, event)
		}
	}

	deniedSubscription := deniedClient.emitAck(t, 2, "realtime:subscribe", map[string]any{
		"topic": "workspace.invite-link.changed", "input": map[string]any{"workspaceId": "workspace-1"}, "clientVersion": "0.26.0",
	})
	if !strings.Contains(deniedSubscription, `"name":"SPACE_ACCESS_DENIED"`) {
		t.Fatalf("unauthorized invite-link subscription = %q", deniedSubscription)
	}
}
