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

type quotaStateReaderFake struct {
	user      workspaces.UserQuotaState
	workspace workspaces.WorkspaceQuotaState
	userIDs   []string
	spaceIDs  []string
}

func (r *quotaStateReaderFake) ReadUserQuotaState(_ context.Context, userID string) (workspaces.UserQuotaState, error) {
	r.userIDs = append(r.userIDs, userID)
	return r.user, nil
}

func (r *quotaStateReaderFake) ReadWorkspaceQuotaState(_ context.Context, workspaceID string) (workspaces.WorkspaceQuotaState, error) {
	r.spaceIDs = append(r.spaceIDs, workspaceID)
	return r.workspace, nil
}

func TestRealtimeQuotaStateRequestsSubscriptionsAndAuthorization(t *testing.T) {
	limit := int32(10)
	quota := &quotaStateReaderFake{
		user: workspaces.UserQuotaState{
			Plan: "pro", SeatLimit: 10, BlobLimit: 100, StorageQuota: 200,
			UsedStorageQuota: 30, HistoryPeriodSeconds: 86400,
			CopilotActionLimit: &limit, UnlimitedCopilot: false,
		},
		workspace: workspaces.WorkspaceQuotaState{
			Plan: "team", OwnerUserID: "user-1", UsesOwnerQuota: false,
			SeatLimit: 2, MemberCount: 3, OvercapacityMemberCount: 1,
			BlobLimit: 500, StorageQuota: 300, UsedStorageQuota: 301,
			HistoryPeriodSeconds: 86400, Readonly: true,
			ReadonlyReasons: []string{"member_overflow", "storage_overflow"}, UnlimitedCopilot: false,
		},
	}
	backend, err := NewWithDependencies(config.Config{NodeEnv: "test", Host: "localhost"}, testAuthenticator{}, testDocumentSync{}, WorkspaceDependencies{
		QuotaAccess: workspaceAccessProviderFake{}, Quota: quota,
	})
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
	userState := client.emitAck(t, 1, "realtime:request", map[string]any{
		"op": "user.quota-state.get", "input": map[string]any{}, "clientVersion": "0.26.0",
	})
	for _, field := range []string{`"plan":"pro"`, `"copilotActionLimit":10`, `"unlimitedCopilot":false`, `"usedStorageQuota":30`} {
		if !strings.Contains(userState, field) {
			t.Fatalf("user quota response missing %s: %q", field, userState)
		}
	}
	workspaceState := client.emitAck(t, 2, "realtime:request", map[string]any{
		"op": "workspace.quota-state.get", "input": map[string]any{"workspaceId": "workspace-1"}, "clientVersion": "0.26.0",
	})
	for _, field := range []string{`"plan":"team"`, `"ownerUserId":"user-1"`, `"usesOwnerQuota":false`, `"readonly":true`, `"storage_overflow"`} {
		if !strings.Contains(workspaceState, field) {
			t.Fatalf("workspace quota response missing %s: %q", field, workspaceState)
		}
	}
	if len(quota.userIDs) != 1 || quota.userIDs[0] != "user-1" || len(quota.spaceIDs) != 1 || quota.spaceIDs[0] != "workspace-1" {
		t.Fatalf("quota reader calls user=%v workspace=%v", quota.userIDs, quota.spaceIDs)
	}

	userSubscription := client.emitAck(t, 3, "realtime:subscribe", map[string]any{
		"topic": "user.quota-state.changed", "input": map[string]any{}, "clientVersion": "0.26.0",
	})
	if !strings.Contains(userSubscription, `"subscriptionId":`) {
		t.Fatalf("user quota subscription = %q", userSubscription)
	}
	backend.PublishUserQuotaStateChanged("user-1", "runtime-invalidation")
	assertRealtimeEvent(t, client.poll(), `"topic":"user.quota-state.changed"`, `"inputKey":"{}"`, `"reason":"runtime-invalidation"`)

	workspaceSubscription := client.emitAck(t, 4, "realtime:subscribe", map[string]any{
		"topic": "workspace.quota-state.changed", "input": map[string]any{"workspaceId": "workspace-1"}, "clientVersion": "0.26.0",
	})
	if !strings.Contains(workspaceSubscription, `"subscriptionId":`) || !strings.Contains(workspaceSubscription, `workspace.quota-state.changed:{\"workspaceId\":\"workspace-1\"}`) {
		t.Fatalf("workspace quota subscription = %q", workspaceSubscription)
	}
	backend.PublishWorkspaceQuotaStateChanged("workspace-1", "runtime-invalidation")
	assertRealtimeEvent(t, client.poll(), `"topic":"workspace.quota-state.changed"`, `"inputKey":"{\"workspaceId\":\"workspace-1\"}"`, `"reason":"runtime-invalidation"`)

	denied := newPollingClient(t, server.URL, "other-session")
	if !strings.Contains(denied.connect(), "40{") {
		t.Fatal("expected second authenticated Socket.IO namespace connection")
	}
	deniedSubscription := denied.emitAck(t, 1, "realtime:subscribe", map[string]any{
		"topic": "workspace.quota-state.changed", "input": map[string]any{"workspaceId": "workspace-1"}, "clientVersion": "0.26.0",
	})
	if !strings.Contains(deniedSubscription, `"name":"SPACE_ACCESS_DENIED"`) {
		t.Fatalf("unauthorized workspace quota subscription = %q", deniedSubscription)
	}
}
