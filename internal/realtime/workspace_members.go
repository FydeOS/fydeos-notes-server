// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

var errWorkspaceMembersUnavailable = errors.New("workspace members realtime is unavailable")
var errWorkspaceMembersDenied = errors.New("workspace member read access is denied")
var errInvalidWorkspaceMembersRequest = errors.New("invalid workspace members request")

const workspaceMembersRequestTakeMax = 100

func (s *Server) workspaceMembers(ctx context.Context, userID string, input map[string]any) (map[string]any, error) {
	if s == nil || s.workspace.Members == nil {
		return nil, errWorkspaceMembersUnavailable
	}
	for key := range input {
		switch key {
		case "workspaceId", "skip", "take", "query":
		default:
			return nil, errInvalidWorkspaceMembersRequest
		}
	}
	workspaceID := stringField(input, "workspaceId")
	if workspaceID == "" {
		return nil, errInvalidWorkspaceMembersRequest
	}
	skip, hasSkip := integerField(input, "skip")
	if _, exists := input["skip"]; exists && !hasSkip || hasSkip && skip < 0 {
		return nil, errInvalidWorkspaceMembersRequest
	}
	take, hasTake := integerField(input, "take")
	if _, exists := input["take"]; exists && !hasTake || hasTake && take < 0 {
		return nil, errInvalidWorkspaceMembersRequest
	}
	if !hasSkip {
		skip = 0
	}
	if !hasTake {
		take = 8
	}
	if take < 1 {
		take = 1
	}
	if take > workspaceMembersRequestTakeMax {
		take = workspaceMembersRequestTakeMax
	}
	search := ""
	if value, exists := input["query"]; exists {
		var ok bool
		search, ok = value.(string)
		if !ok {
			return nil, errInvalidWorkspaceMembersRequest
		}
	}
	members, err := s.workspace.Members.ListMembersForUser(ctx, userID, workspaceID, search, skip, take)
	if errors.Is(err, workspaces.ErrAccessDenied) {
		return nil, errWorkspaceMembersDenied
	}
	if err != nil {
		return nil, err
	}
	count, err := s.workspace.Members.MemberCount(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, len(members))
	for _, member := range members {
		items = append(items, map[string]any{
			"id": member.ID, "inviteId": member.InviteID, "name": member.Name,
			"email": member.Email, "avatarUrl": member.AvatarURL,
			"permission": member.Role, "role": member.Role,
			"emailVerified": nil, "status": member.Status,
		})
	}
	return map[string]any{"members": items, "memberCount": count}, nil
}

func workspaceMembersError(err error) error {
	switch {
	case errors.Is(err, errInvalidWorkspaceMembersRequest):
		return syncError("BAD_REQUEST", "Invalid workspace members request.")
	case errors.Is(err, errWorkspaceMembersUnavailable):
		return syncError("INTERNAL_SERVER_ERROR", "Workspace member realtime is unavailable.")
	case errors.Is(err, errWorkspaceMembersDenied), errors.Is(err, workspaces.ErrAccessDenied):
		return syncError("SPACE_ACCESS_DENIED", "Workspace member read access is denied.")
	case errors.Is(err, workspaces.ErrMemberQueryTooLong):
		return syncError("QUERY_TOO_LONG", "Query is too long, max length is 255.")
	case errors.Is(err, workspaces.ErrNotFound):
		return syncError("SPACE_NOT_FOUND", "Workspace not found.")
	default:
		return syncError("INTERNAL_SERVER_ERROR", "The request could not be completed.")
	}
}

func (s *Server) PublishWorkspaceMembersUpdated(workspaceID string) {
	if s == nil || workspaceID == "" {
		return
	}
	s.publishWorkspacePermissionChange(workspaceID)
	s.PublishWorkspaceQuotaStateChanged(workspaceID, "runtime-invalidation")
	inputKey, err := json.Marshal(map[string]string{"workspaceId": workspaceID})
	if err != nil {
		return
	}
	for _, topic := range []string{"workspace.access.changed", "workspace.members.changed"} {
		s.broadcastRoom(workspaceRealtimeRoom(workspaceID, topic), "realtime:event", map[string]any{
			"topic": topic, "inputKey": string(inputKey), "sentAt": time.Now().UnixMilli(),
			"event": map[string]any{"changed": true, "reason": "members-updated"},
		}, "")
	}
}

func (s *Server) publishWorkspaceMembersChanged(workspaceID, reason string) {
	s.publishWorkspaceMembersChangedContext(context.Background(), workspaceID, reason)
}

func (s *Server) publishWorkspaceMembersChangedContext(ctx context.Context, workspaceID, reason string) error {
	if s == nil || workspaceID == "" {
		return nil
	}
	inputKey, err := json.Marshal(map[string]string{"workspaceId": workspaceID})
	if err != nil {
		return err
	}
	return s.broadcastRoomContext(ctx, workspaceRealtimeRoom(workspaceID, "workspace.members.changed"), "realtime:event", map[string]any{
		"topic": "workspace.members.changed", "inputKey": string(inputKey), "sentAt": time.Now().UnixMilli(),
		"event": map[string]any{"changed": true, "reason": reason},
	}, "")
}

func (s *Server) publishWorkspacePermissionChange(workspaceID string) {
	if s == nil || workspaceID == "" {
		return
	}
	message := realtimeFanoutMessage{
		Kind: "workspace-permissions", WorkspaceID: workspaceID, Event: "sync.permissions.changed",
	}
	s.dispatchFanoutMessage(message)
	s.publishFanoutMessage(message)
}

func workspaceRealtimeRoom(workspaceID, topic string) string {
	scope := strings.TrimPrefix(topic, "workspace.")
	scope = strings.TrimSuffix(scope, ".changed")
	return "workspace:" + workspaceID + ":" + scope
}
