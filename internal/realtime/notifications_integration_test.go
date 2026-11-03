// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package realtime

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fydeos/fydeos-notes-server/internal/config"
)

type notificationCountRepositoryFake struct {
	userID string
	count  int
}

func (f *notificationCountRepositoryFake) CountUnread(_ context.Context, userID string) (int, error) {
	f.userID = userID
	return f.count, nil
}

func TestNotificationCountRealtimeRequestAndSubscription(t *testing.T) {
	backend, err := NewWithDependencies(config.Config{NodeEnv: "test", Host: "localhost"}, testAuthenticator{}, testDocumentSync{})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	counts := &notificationCountRepositoryFake{count: 3}
	backend.SetNotificationCountRepository(counts)
	server := httptest.NewServer(backend.Handler())
	defer server.Close()

	client := newPollingClient(t, server.URL, "valid-session")
	if !strings.Contains(client.connect(), "40{") {
		t.Fatal("expected authenticated Socket.IO namespace connection")
	}
	count := client.emitAck(t, 1, "realtime:request", map[string]any{
		"op": "notification.count.get", "input": map[string]any{}, "clientVersion": "0.26.0",
	})
	if !strings.Contains(count, `"data":{"count":3}`) || counts.userID != "user-1" {
		t.Fatalf("notification count response/user = %q/%q", count, counts.userID)
	}
	subscription := client.emitAck(t, 2, "realtime:subscribe", map[string]any{
		"topic": "notification.count.changed", "input": map[string]any{}, "clientVersion": "0.26.0",
	})
	if !strings.Contains(subscription, `"data":{"subscriptionId":`) || !strings.Contains(subscription, `notification.count.changed:{}`) {
		t.Fatalf("notification count subscription acknowledgement = %q", subscription)
	}
	otherClient := newPollingClient(t, server.URL, "other-session")
	if !strings.Contains(otherClient.connect(), "40{") {
		t.Fatal("expected second authenticated Socket.IO namespace connection")
	}
	otherSubscription := otherClient.emitAck(t, 1, "realtime:subscribe", map[string]any{
		"topic": "notification.count.changed", "input": map[string]any{}, "clientVersion": "0.26.0",
	})
	if !strings.Contains(otherSubscription, `"data":{"subscriptionId":`) {
		t.Fatalf("second user's notification subscription acknowledgement = %q", otherSubscription)
	}

	backend.PublishNotificationCountChanged("user-1", 2, "read")
	backend.PublishNotificationCountChanged("user-2", 5, "created")
	event := client.poll()
	for _, field := range []string{`"topic":"notification.count.changed"`, `"inputKey":"{}"`, `"count":2`, `"reason":"read"`} {
		if !strings.Contains(event, field) {
			t.Fatalf("notification count event missing %s: %q", field, event)
		}
	}
	otherEvent := otherClient.poll()
	for _, field := range []string{`"topic":"notification.count.changed"`, `"inputKey":"{}"`, `"count":5`, `"reason":"created"`} {
		if !strings.Contains(otherEvent, field) {
			t.Fatalf("other user's notification count event missing %s: %q", field, otherEvent)
		}
	}
	if strings.Contains(otherEvent, `"count":2`) || strings.Contains(event, `"count":5`) {
		t.Fatalf("notification counts crossed user rooms: user-1=%q user-2=%q", event, otherEvent)
	}
}
