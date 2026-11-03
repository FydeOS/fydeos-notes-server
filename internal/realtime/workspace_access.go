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

var errWorkspaceAccessUnavailable = errors.New("workspace access realtime is unavailable")

func (s *Server) workspaceAccess(ctx context.Context, userID string, input map[string]any) (map[string]any, error) {
	if s == nil || s.workspace.Access == nil {
		return nil, errWorkspaceAccessUnavailable
	}
	workspaceID, valid := workspaceInviteLinkInput(input)
	if !valid {
		return nil, errInvalidWorkspaceAccessRequest
	}
	access, err := s.workspace.Access.GetWorkspaceAccess(ctx, userID, workspaceID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"access": map[string]any{
		"role": access.Role, "permissions": access.Permissions, "team": access.Team,
	}}, nil
}

func (s *Server) PublishWorkspaceEntitlementChanged(workspaceID string) {
	if s == nil || workspaceID == "" {
		return
	}
	s.publishWorkspacePermissionChange(workspaceID)
	s.publishWorkspaceAccessChanged(workspaceID, "quota-state-changed")
	s.PublishWorkspaceQuotaStateChanged(workspaceID, "runtime-invalidation")
}

func (s *Server) publishWorkspaceAccessChanged(workspaceID, reason string) {
	inputKey, err := json.Marshal(map[string]string{"workspaceId": workspaceID})
	if err != nil {
		return
	}
	s.broadcastRoom(workspaceRealtimeRoom(workspaceID, "workspace.access.changed"), "realtime:event", map[string]any{
		"topic": "workspace.access.changed", "inputKey": string(inputKey), "sentAt": time.Now().UnixMilli(),
		"event": map[string]any{"changed": true, "reason": reason},
	}, "")
}

var errInvalidWorkspaceAccessRequest = errors.New("invalid workspace access request")

func workspaceAccessError(err error) error {
	switch {
	case errors.Is(err, errInvalidWorkspaceAccessRequest):
		return syncError("BAD_REQUEST", "Invalid workspace access request.")
	case errors.Is(err, errWorkspaceAccessUnavailable), errors.Is(err, workspaces.ErrAuthorizationUnavailable):
		return syncError("INTERNAL_SERVER_ERROR", "Workspace access realtime is unavailable.")
	case errors.Is(err, workspaces.ErrAccessDenied):
		return syncError("SPACE_ACCESS_DENIED", "Workspace access is denied.")
	case errors.Is(err, workspaces.ErrNotFound):
		return syncError("SPACE_NOT_FOUND", "Workspace not found.")
	default:
		return syncError("INTERNAL_SERVER_ERROR", "The request could not be completed.")
	}
}
