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
	"github.com/fydeos/fydeos-notes-server/internal/users"
)

type userSettingsReaderStub struct {
	userIDs []string
}

func (r *userSettingsReaderStub) GetSettings(_ context.Context, userID string) (users.UserSettings, error) {
	r.userIDs = append(r.userIDs, userID)
	return users.UserSettings{ReceiveInvitationEmail: true, ReceiveMentionEmail: false, ReceiveCommentEmail: true}, nil
}

func TestRealtimeUserSettingsRequestSubscribeAndUserIsolation(t *testing.T) {
	backend, err := NewWithDependencies(config.Config{NodeEnv: "test", Host: "localhost"}, testAuthenticator{}, testDocumentSync{})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	settings := &userSettingsReaderStub{}
	backend.SetUserSettingsReader(settings)
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

	response := client.emitAck(t, 1, "realtime:request", map[string]any{
		"op": "user.settings.get", "input": map[string]any{}, "clientVersion": "0.26.0",
	})
	for _, field := range []string{`"settings":{"receiveInvitationEmail":true`, `"receiveMentionEmail":false`, `"receiveCommentEmail":true`} {
		if !strings.Contains(response, field) {
			t.Fatalf("settings response missing %s: %q", field, response)
		}
	}
	if len(settings.userIDs) != 1 || settings.userIDs[0] != "user-1" {
		t.Fatalf("settings lookup user IDs = %#v", settings.userIDs)
	}

	subscription := client.emitAck(t, 2, "realtime:subscribe", map[string]any{
		"topic": "user.settings.changed", "input": map[string]any{}, "clientVersion": "0.26.0",
	})
	if !strings.Contains(subscription, `"data":{"subscriptionId":`) {
		t.Fatalf("settings subscription acknowledgement = %q", subscription)
	}
	otherSubscription := otherClient.emitAck(t, 1, "realtime:subscribe", map[string]any{
		"topic": "user.settings.changed", "input": map[string]any{}, "clientVersion": "0.26.0",
	})
	if !strings.Contains(otherSubscription, `"data":{"subscriptionId":`) {
		t.Fatalf("other settings subscription acknowledgement = %q", otherSubscription)
	}

	backend.PublishUserSettingsChanged("user-1")
	event := client.poll()
	for _, field := range []string{`"topic":"user.settings.changed"`, `"inputKey":"{}"`, `"changed":true`, `"reason":"settings-updated"`} {
		if !strings.Contains(event, field) {
			t.Fatalf("settings event missing %s: %q", field, event)
		}
	}
	otherClient.client.Timeout = 150 * time.Millisecond
	if packet, err := otherClient.pollWithError(); err == nil {
		t.Fatalf("another user's settings event was received: %q", packet)
	}
}
