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

var (
	errWorkspaceEmbeddingProgressUnavailable    = errors.New("workspace embedding progress is unavailable")
	errInvalidWorkspaceEmbeddingProgressRequest = errors.New("invalid workspace embedding progress request")
)

func (s *Server) workspaceEmbeddingProgress(ctx context.Context, userID string, input map[string]any) (map[string]any, error) {
	workspaceID, valid := workspaceInviteLinkInput(input)
	if !valid {
		return nil, errInvalidWorkspaceEmbeddingProgressRequest
	}
	if err := s.authorizeWorkspaceCopilot(ctx, userID, workspaceID); err != nil {
		return nil, err
	}
	if s.workspace.Embedding == nil {
		return map[string]any{"total": int64(0), "embedded": int64(0)}, nil
	}
	total, embedded, err := s.workspace.Embedding.ReadEmbeddingProgress(ctx, workspaceID)
	if err != nil {
		return nil, errWorkspaceEmbeddingProgressUnavailable
	}
	return map[string]any{"total": total, "embedded": embedded}, nil
}

func (s *Server) authorizeWorkspaceCopilot(ctx context.Context, userID, workspaceID string) error {
	if s == nil || s.workspace.QuotaAccess == nil {
		return errWorkspaceEmbeddingProgressUnavailable
	}
	access, err := s.workspace.QuotaAccess.GetWorkspaceRolePermissions(ctx, userID, workspaceID)
	if err != nil {
		return err
	}
	if !access.Permissions["Workspace_Copilot"] {
		return workspaces.ErrAccessDenied
	}
	return nil
}

func workspaceEmbeddingProgressError(err error) error {
	switch {
	case errors.Is(err, errInvalidWorkspaceEmbeddingProgressRequest):
		return syncError("BAD_REQUEST", "Invalid workspace embedding progress request.")
	case errors.Is(err, errWorkspaceEmbeddingProgressUnavailable), errors.Is(err, workspaces.ErrAuthorizationUnavailable):
		return syncError("INTERNAL_SERVER_ERROR", "Workspace embedding progress is unavailable.")
	case errors.Is(err, workspaces.ErrAccessDenied):
		return syncError("SPACE_ACCESS_DENIED", "Workspace Copilot access is denied.")
	case errors.Is(err, workspaces.ErrNotFound):
		return syncError("SPACE_NOT_FOUND", "Workspace not found.")
	default:
		return syncError("INTERNAL_SERVER_ERROR", "The request could not be completed.")
	}
}

func (s *Server) PublishWorkspaceEmbeddingProgressChanged(workspaceID string) {
	if s == nil || workspaceID == "" {
		return
	}
	inputKey, err := json.Marshal(map[string]string{"workspaceId": workspaceID})
	if err != nil {
		return
	}
	s.broadcastRoom(workspaceRealtimeRoom(workspaceID, "workspace.embedding.progress.changed"), "realtime:event", map[string]any{
		"topic": "workspace.embedding.progress.changed", "inputKey": string(inputKey),
		"sentAt": time.Now().UnixMilli(), "event": map[string]any{"changed": true, "reason": "embedding-progress-updated"},
	}, "")
}
