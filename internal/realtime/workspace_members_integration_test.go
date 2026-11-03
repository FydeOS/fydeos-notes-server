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

type workspaceMembersReaderFake struct {
	members []workspaces.Member
	count   int
}

type workspaceAccessProviderFake struct{}
type workspaceEmbeddingProgressProviderFake struct{}

func (workspaceEmbeddingProgressProviderFake) ReadEmbeddingProgress(context.Context, string) (int64, int64, error) {
	return 12, 9, nil
}

func (workspaceAccessProviderFake) GetWorkspaceAccess(_ context.Context, userID, _ string) (workspaces.WorkspaceAccess, error) {
	if userID != "user-1" {
		return workspaces.WorkspaceAccess{}, workspaces.ErrAccessDenied
	}
	return workspaces.WorkspaceAccess{
		Role: "Owner", Team: true,
		Permissions: map[string]bool{"Workspace_Read": true, "Workspace_Delete": true, "Workspace_Copilot": true},
	}, nil
}

func (workspaceAccessProviderFake) GetWorkspaceRolePermissions(ctx context.Context, userID, workspaceID string) (workspaces.WorkspaceAccess, error) {
	return workspaceAccessProviderFake{}.GetWorkspaceAccess(ctx, userID, workspaceID)
}

type workspaceConfigProviderFake struct{}

func (workspaceConfigProviderFake) GetWorkspaceConfig(_ context.Context, userID, _ string) (workspaces.WorkspaceConfig, error) {
	if userID != "user-1" {
		return workspaces.WorkspaceConfig{}, workspaces.ErrAccessDenied
	}
	return workspaces.WorkspaceConfig{EnableAI: true, EnableSharing: false, EnableURLPreview: true}, nil
}

func (f workspaceMembersReaderFake) MemberCount(context.Context, string) (int, error) {
	return f.count, nil
}

func (f workspaceMembersReaderFake) ListMembersForUser(_ context.Context, userID, _ string, _ string, _, _ int) ([]workspaces.Member, error) {
	if userID != "user-1" {
		return nil, workspaces.ErrAccessDenied
	}
	return f.members, nil
}

func TestRealtimeWorkspaceMemberRequestAndChangeTopics(t *testing.T) {
	authorizer := testWorkspaceAuthorizer{access: workspaces.WorkspaceSyncAccess{CanSync: true}}
	backend, err := NewWithDependencies(
		config.Config{NodeEnv: "test", Host: "localhost"}, testAuthenticator{}, testDocumentSync{},
		WorkspaceDependencies{
			Authorizer: authorizer, Documents: &testWorkspaceDocumentSync{},
			Access: workspaceAccessProviderFake{}, Config: workspaceConfigProviderFake{}, Embedding: workspaceEmbeddingProgressProviderFake{},
			QuotaAccess: workspaceAccessProviderFake{}, Quota: &quotaStateReaderFake{},
			Members: workspaceMembersReaderFake{members: []workspaces.Member{{
				ID: "user-1", InviteID: "user-1", Name: "Current User", Email: "current@example.test",
				Role: "Owner", Status: "Accepted",
			}}, count: 1},
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
	accessRequest := client.emitAck(t, 1, "realtime:request", map[string]any{
		"op": "workspace.access.get", "input": map[string]any{"workspaceId": "workspace-1"}, "clientVersion": "0.26.0",
	})
	for _, field := range []string{`"role":"Owner"`, `"Workspace_Read":true`, `"Workspace_Delete":true`, `"team":true`} {
		if !strings.Contains(accessRequest, field) {
			t.Fatalf("workspace access response missing %s: %q", field, accessRequest)
		}
	}
	configRequest := client.emitAck(t, 2, "realtime:request", map[string]any{
		"op": "workspace.config.get", "input": map[string]any{"workspaceId": "workspace-1"}, "clientVersion": "0.26.0",
	})
	for _, field := range []string{`"enableAi":true`, `"enableSharing":false`, `"enableUrlPreview":true`, `"enableDocEmbedding":false`} {
		if !strings.Contains(configRequest, field) {
			t.Fatalf("workspace config response missing %s: %q", field, configRequest)
		}
	}
	request := client.emitAck(t, 3, "realtime:request", map[string]any{
		"op": "workspace.members.get", "input": map[string]any{"workspaceId": "workspace-1", "take": 1000}, "clientVersion": "0.26.0",
	})
	for _, field := range []string{`"memberCount":1`, `"permission":"Owner"`, `"status":"Accepted"`} {
		if !strings.Contains(request, field) {
			t.Fatalf("workspace members response missing %s: %q", field, request)
		}
	}
	embeddingProgress := client.emitAck(t, 4, "realtime:request", map[string]any{
		"op": "workspace.embedding.progress.get", "input": map[string]any{"workspaceId": "workspace-1"}, "clientVersion": "0.26.0",
	})
	if !strings.Contains(embeddingProgress, `"total":12`) || !strings.Contains(embeddingProgress, `"embedded":9`) {
		t.Fatalf("workspace embedding progress response = %q", embeddingProgress)
	}

	for id, topic := range []string{"workspace.access.changed", "workspace.config.changed", "workspace.members.changed", "workspace.quota-state.changed", "workspace.embedding.progress.changed"} {
		ack := client.emitAck(t, id+4, "realtime:subscribe", map[string]any{
			"topic": topic, "input": map[string]any{"workspaceId": "workspace-1"}, "clientVersion": "0.26.0",
		})
		if !strings.Contains(ack, `"subscriptionId":`) || !strings.Contains(ack, topic+`:{\"workspaceId\":\"workspace-1\"}`) {
			t.Fatalf("%s subscription acknowledgement = %q", topic, ack)
		}
	}
	backend.PublishWorkspaceMembersUpdated("workspace-1")
	backend.WorkspaceMetadataBackfilled("workspace-1")
	backend.PublishWorkspaceEmbeddingProgressChanged("workspace-1")
	event := client.poll()
	for _, topic := range []string{"workspace.access.changed", "workspace.config.changed", "workspace.members.changed", "workspace.quota-state.changed", "workspace.embedding.progress.changed"} {
		reason := "members-updated"
		if topic == "workspace.config.changed" {
			reason = "workspace-updated"
		} else if topic == "workspace.quota-state.changed" {
			reason = "runtime-invalidation"
		} else if topic == "workspace.embedding.progress.changed" {
			reason = "embedding-progress-updated"
		}
		for _, field := range []string{`"topic":"` + topic + `"`, `"changed":true`, `"reason":"` + reason + `"`} {
			if !strings.Contains(event, field) {
				t.Fatalf("%s event missing %s: %q", topic, field, event)
			}
		}
	}

	deniedClient := newPollingClient(t, server.URL, "other-session")
	if !strings.Contains(deniedClient.connect(), "40{") {
		t.Fatal("expected second authenticated Socket.IO namespace connection")
	}
	deniedAccess := deniedClient.emitAck(t, 1, "realtime:request", map[string]any{
		"op": "workspace.access.get", "input": map[string]any{"workspaceId": "workspace-1"}, "clientVersion": "0.26.0",
	})
	if !strings.Contains(deniedAccess, `"name":"SPACE_ACCESS_DENIED"`) {
		t.Fatalf("unauthorized workspace access request = %q", deniedAccess)
	}
	deniedConfig := deniedClient.emitAck(t, 2, "realtime:request", map[string]any{
		"op": "workspace.config.get", "input": map[string]any{"workspaceId": "workspace-1"}, "clientVersion": "0.26.0",
	})
	if !strings.Contains(deniedConfig, `"name":"SPACE_ACCESS_DENIED"`) {
		t.Fatalf("unauthorized workspace config request = %q", deniedConfig)
	}
	deniedEmbeddingProgress := deniedClient.emitAck(t, 3, "realtime:request", map[string]any{
		"op": "workspace.embedding.progress.get", "input": map[string]any{"workspaceId": "workspace-1"}, "clientVersion": "0.26.0",
	})
	if !strings.Contains(deniedEmbeddingProgress, `"name":"SPACE_ACCESS_DENIED"`) {
		t.Fatalf("unauthorized workspace embedding progress request = %q", deniedEmbeddingProgress)
	}
	denied := deniedClient.emitAck(t, 3, "realtime:subscribe", map[string]any{
		"topic": "workspace.members.changed", "input": map[string]any{"workspaceId": "workspace-1"}, "clientVersion": "0.26.0",
	})
	if !strings.Contains(denied, `"name":"SPACE_ACCESS_DENIED"`) {
		t.Fatalf("unauthorized workspace members subscription = %q", denied)
	}
}
