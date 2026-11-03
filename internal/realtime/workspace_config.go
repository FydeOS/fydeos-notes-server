// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

var errWorkspaceConfigUnavailable = errors.New("workspace config realtime is unavailable")

func (s *Server) workspaceConfig(ctx context.Context, userID string, input map[string]any) (map[string]any, error) {
	if s == nil || s.workspace.Config == nil {
		return nil, errWorkspaceConfigUnavailable
	}
	workspaceID, valid := workspaceInviteLinkInput(input)
	if !valid {
		return nil, errInvalidWorkspaceConfigRequest
	}
	config, err := s.workspace.Config.GetWorkspaceConfig(ctx, userID, workspaceID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"config": map[string]any{
		"enableAi": config.EnableAI, "enableSharing": config.EnableSharing,
		"enableUrlPreview": config.EnableURLPreview, "enableDocEmbedding": config.EnableDocEmbedding,
	}}, nil
}

var errInvalidWorkspaceConfigRequest = errors.New("invalid workspace config request")

func workspaceConfigError(err error) error {
	switch {
	case errors.Is(err, errInvalidWorkspaceConfigRequest):
		return syncError("BAD_REQUEST", "Invalid workspace config request.")
	case errors.Is(err, errWorkspaceConfigUnavailable), errors.Is(err, workspaces.ErrAuthorizationUnavailable):
		return syncError("INTERNAL_SERVER_ERROR", "Workspace config realtime is unavailable.")
	case errors.Is(err, workspaces.ErrAccessDenied):
		return syncError("SPACE_ACCESS_DENIED", "Workspace settings read access is denied.")
	case errors.Is(err, workspaces.ErrNotFound):
		return syncError("SPACE_NOT_FOUND", "Workspace not found.")
	default:
		return syncError("INTERNAL_SERVER_ERROR", "The request could not be completed.")
	}
}

func (s *Server) PublishWorkspaceConfigUpdated(workspaceID string) {
	if s == nil || workspaceID == "" {
		return
	}
	s.publishWorkspacePermissionChange(workspaceID)
	inputKey, err := json.Marshal(map[string]string{"workspaceId": workspaceID})
	if err != nil {
		return
	}
	s.broadcastRoom(workspaceRealtimeRoom(workspaceID, "workspace.config.changed"), "realtime:event", map[string]any{
		"topic": "workspace.config.changed", "inputKey": string(inputKey), "sentAt": time.Now().UnixMilli(),
		"event": map[string]any{"changed": true, "reason": "workspace-updated"},
	}, "")
}

func (s *Server) WorkspaceMetadataBackfilled(workspaceID string) {
	if s == nil || workspaceID == "" {
		return
	}
	s.PublishWorkspaceConfigUpdated(workspaceID)
}
