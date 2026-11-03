// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/invitelinks"
)

var errWorkspaceInviteLinkDenied = errors.New("workspace invite link access denied")
var errWorkspaceInviteLinkUnavailable = errors.New("workspace invite link service is unavailable")

func (s *Server) workspaceInviteLink(ctx context.Context, userID, workspaceID string) (*invitelinks.Link, error) {
	if err := s.authorizeWorkspaceInviteLink(ctx, userID, workspaceID); err != nil {
		return nil, err
	}
	if s.inviteLinks == nil {
		return nil, errWorkspaceInviteLinkUnavailable
	}
	return s.inviteLinks.Get(ctx, workspaceID)
}

func (s *Server) authorizeWorkspaceInviteLink(ctx context.Context, userID, workspaceID string) error {
	if s == nil || s.inviteLinkAuthorizer == nil {
		return errWorkspaceInviteLinkUnavailable
	}
	allowed, err := s.inviteLinkAuthorizer.CanManageWorkspaceUsers(ctx, userID, workspaceID)
	if err != nil {
		return err
	}
	if !allowed {
		return errWorkspaceInviteLinkDenied
	}
	return nil
}

func (s *Server) PublishWorkspaceInviteLinkChanged(workspaceID, reason string) {
	if s == nil || workspaceID == "" {
		return
	}
	inputKey, err := json.Marshal(map[string]string{"workspaceId": workspaceID})
	if err != nil {
		return
	}
	s.broadcastRoom(workspaceInviteLinkRoom(workspaceID), "realtime:event", map[string]any{
		"topic": "workspace.invite-link.changed", "inputKey": string(inputKey),
		"sentAt": time.Now().UnixMilli(), "event": map[string]any{"changed": true, "reason": reason},
	}, "")
}

func workspaceInviteLinkRoom(workspaceID string) string {
	return "workspace:" + workspaceID + ":invite-link"
}
