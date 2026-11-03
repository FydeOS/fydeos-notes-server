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

var errQuotaStateUnavailable = errors.New("quota state realtime is unavailable")
var errInvalidQuotaStateRequest = errors.New("invalid quota state request")

func (s *Server) userQuotaState(ctx context.Context, userID string) (map[string]any, error) {
	if s == nil || s.workspace.Quota == nil {
		return nil, errQuotaStateUnavailable
	}
	state, err := s.workspace.Quota.ReadUserQuotaState(ctx, userID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"state": map[string]any{
		"plan": state.Plan, "seatLimit": state.SeatLimit, "blobLimit": state.BlobLimit,
		"storageQuota": state.StorageQuota, "usedStorageQuota": state.UsedStorageQuota,
		"historyPeriodSeconds": state.HistoryPeriodSeconds,
		"copilotActionLimit":   state.CopilotActionLimit, "unlimitedCopilot": state.UnlimitedCopilot,
	}}, nil
}

func (s *Server) workspaceQuotaState(ctx context.Context, userID string, input map[string]any) (map[string]any, error) {
	if s == nil || s.workspace.Quota == nil || s.workspace.QuotaAccess == nil {
		return nil, errQuotaStateUnavailable
	}
	workspaceID, valid := workspaceInviteLinkInput(input)
	if !valid {
		return nil, errInvalidQuotaStateRequest
	}
	if _, err := s.workspace.QuotaAccess.GetWorkspaceRolePermissions(ctx, userID, workspaceID); err != nil {
		return nil, err
	}
	state, err := s.workspace.Quota.ReadWorkspaceQuotaState(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"state": map[string]any{
		"plan": state.Plan, "ownerUserId": state.OwnerUserID, "usesOwnerQuota": state.UsesOwnerQuota,
		"seatLimit": state.SeatLimit, "memberCount": state.MemberCount,
		"overcapacityMemberCount": state.OvercapacityMemberCount,
		"blobLimit":               state.BlobLimit, "storageQuota": state.StorageQuota,
		"usedStorageQuota": state.UsedStorageQuota, "historyPeriodSeconds": state.HistoryPeriodSeconds,
		"readonly": state.Readonly, "readonlyReasons": state.ReadonlyReasons,
		"unlimitedCopilot": state.UnlimitedCopilot,
	}}, nil
}

func quotaStateError(err error) error {
	switch {
	case errors.Is(err, errInvalidQuotaStateRequest):
		return syncError("BAD_REQUEST", "Invalid quota state request.")
	case errors.Is(err, errQuotaStateUnavailable), errors.Is(err, workspaces.ErrAuthorizationUnavailable):
		return syncError("INTERNAL_SERVER_ERROR", "Quota state realtime is unavailable.")
	case errors.Is(err, workspaces.ErrAccessDenied):
		return syncError("SPACE_ACCESS_DENIED", "Workspace read access is denied.")
	case errors.Is(err, workspaces.ErrNotFound):
		return syncError("SPACE_NOT_FOUND", "Workspace not found.")
	default:
		return syncError("INTERNAL_SERVER_ERROR", "The request could not be completed.")
	}
}

func quotaStateRoom(userID, workspaceID string) string {
	if userID != "" {
		return "user:" + userID + ":quota-state"
	}
	return "workspace:" + workspaceID + ":quota-state"
}

func (s *Server) PublishUserQuotaStateChanged(userID, reason string) {
	if s == nil || s.io == nil || userID == "" {
		return
	}
	s.broadcastRoom(quotaStateRoom(userID, ""), "realtime:event", map[string]any{
		"topic": "user.quota-state.changed", "inputKey": "{}", "sentAt": time.Now().UnixMilli(),
		"event": map[string]any{"changed": true, "reason": reason},
	}, "")
}

func (s *Server) PublishWorkspaceQuotaStateChanged(workspaceID, reason string) {
	if s == nil || s.io == nil || workspaceID == "" {
		return
	}
	inputKey, err := json.Marshal(map[string]string{"workspaceId": workspaceID})
	if err != nil {
		return
	}
	s.broadcastRoom(quotaStateRoom("", workspaceID), "realtime:event", map[string]any{
		"topic": "workspace.quota-state.changed", "inputKey": string(inputKey), "sentAt": time.Now().UnixMilli(),
		"event": map[string]any{"changed": true, "reason": reason},
	}, "")
}

func workspaceQuotaStateAccess(ctx context.Context, access WorkspaceRolePermissionsProvider, userID, workspaceID string) error {
	if access == nil {
		return errQuotaStateUnavailable
	}
	_, err := access.GetWorkspaceRolePermissions(ctx, userID, workspaceID)
	return err
}
