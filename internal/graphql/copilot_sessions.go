// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/copilot"
	"github.com/fydeos/fydeos-notes-server/internal/graphql/model"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

var ErrCopilotSessionsUnavailable = errors.New("copilot session service is unavailable")

func (r *Resolver) authorizeCopilotScope(ctx context.Context, workspaceID string, docID *string, docAction string) (*auth.Principal, bool, error) {
	if err := r.requireCopilotEnabled(ctx); err != nil {
		return nil, false, err
	}
	principal := auth.PrincipalFromContext(ctx)
	if principal == nil {
		return nil, false, ErrAuthenticationRequired
	}
	if r.CopilotSessions == nil {
		return nil, false, ErrCopilotSessionsUnavailable
	}
	workspaceExists, err := r.CopilotSessions.WorkspaceExists(ctx, workspaceID)
	if err != nil {
		return nil, false, err
	}
	if !workspaceExists {
		if docID != nil && *docID != "" {
			return nil, false, copilot.ErrPersonalScope
		}
		return principal, true, nil
	}
	if docID != nil && *docID != "" {
		if r.DocumentReader == nil {
			return nil, false, workspaces.ErrAuthorizationUnavailable
		}
		permissions, err := r.DocumentReader.ReadDocumentPermissions(ctx, principal.User.ID, workspaceID, *docID)
		if err != nil {
			return nil, false, documentReadError(err, workspaceID, *docID, docAction)
		}
		if !permissions[docAction] {
			return nil, false, documentReadError(workspaces.ErrDocRoleDenied, workspaceID, *docID, docAction)
		}
		return principal, false, nil
	}
	if r.WorkspacePermissionReader == nil {
		return nil, false, ErrWorkspaceRolePermissionsUnavailable
	}
	access, err := r.WorkspacePermissionReader.GetWorkspaceRolePermissions(ctx, principal.User.ID, workspaceID)
	if errors.Is(err, workspaces.ErrAccessDenied) || errors.Is(err, workspaces.ErrNotFound) {
		return nil, false, &WorkspacePermissionsDeniedError{WorkspaceID: workspaceID}
	}
	if err != nil {
		return nil, false, err
	}
	if !access.Permissions["Workspace_Copilot"] {
		return nil, false, &WorkspacePermissionsDeniedError{WorkspaceID: workspaceID}
	}
	return principal, false, nil
}

func (r *Resolver) copilotScopeForSession(ctx context.Context, userID, workspaceID string, docID *string, docAction string) (bool, error) {
	_, personal, err := r.authorizeCopilotScope(ctx, workspaceID, docID, docAction)
	return personal, err
}

func copilotHistoryModel(session copilot.Session) (*model.CopilotHistories, error) {
	createdAt := session.CreatedAt.UTC().Format(time.RFC3339Nano)
	updatedAt := session.UpdatedAt.UTC().Format(time.RFC3339Nano)
	result := &model.CopilotHistories{SessionID: session.ID, WorkspaceID: session.WorkspaceID, DocID: session.DocID,
		ParentSessionID: session.ParentSessionID, PromptName: session.PromptName, Action: session.Action,
		Pinned: session.Pinned, Title: session.Title, CreatedAt: createdAt, UpdatedAt: updatedAt,
		Messages: make([]*model.ChatMessage, 0, len(session.Messages))}
	for _, message := range session.Messages {
		attachments := []string{}
		if len(message.Attachments) > 0 {
			_ = json.Unmarshal(message.Attachments, &attachments)
		}
		if message.Content == "" && len(attachments) == 0 {
			continue
		}
		id := message.ID
		chatMessage := &model.ChatMessage{ID: &id, Role: message.Role, Content: message.Content, Attachments: attachments, CreatedAt: message.CreatedAt.UTC().Format(time.RFC3339Nano)}
		if len(message.ScopeSnapshot) > 0 && string(message.ScopeSnapshot) != "null" {
			var value any
			if err := json.Unmarshal(message.ScopeSnapshot, &value); err != nil {
				return nil, fmt.Errorf("decode copilot message scope: %w", err)
			}
			chatMessage.ScopeSnapshot = &model.JSON{Value: value}
		}
		var streamObjects []map[string]json.RawMessage
		if len(message.StreamObjects) > 0 && string(message.StreamObjects) != "null" {
			if err := json.Unmarshal(message.StreamObjects, &streamObjects); err != nil {
				return nil, fmt.Errorf("decode copilot stream objects: %w", err)
			}
		}
		for _, raw := range streamObjects {
			var item model.StreamObject
			if err := json.Unmarshal(raw["type"], &item.Type); err != nil {
				continue
			}
			_ = json.Unmarshal(raw["textDelta"], &item.TextDelta)
			_ = json.Unmarshal(raw["toolCallId"], &item.ToolCallID)
			_ = json.Unmarshal(raw["toolName"], &item.ToolName)
			if bytes := raw["args"]; len(bytes) > 0 && string(bytes) != "null" {
				var value any
				if err := json.Unmarshal(bytes, &value); err != nil {
					return nil, err
				}
				item.Args = &model.JSON{Value: value}
			}
			if bytes := raw["result"]; len(bytes) > 0 && string(bytes) != "null" {
				var value any
				if err := json.Unmarshal(bytes, &value); err != nil {
					return nil, err
				}
				item.Result = &model.JSON{Value: value}
			}
			chatMessage.StreamObjects = append(chatMessage.StreamObjects, &item)
		}
		result.Messages = append(result.Messages, chatMessage)
	}
	return result, nil
}
