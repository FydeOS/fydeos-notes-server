// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package realtime

import (
	"context"

	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

type DocShareStateReader interface {
	ReadDocShareState(context.Context, string, string) (*workspaces.DocShareState, error)
}

func (s *Server) docShareState(ctx context.Context, userID string, input any) (map[string]any, error) {
	workspaceID, docID, valid := docGrantsSubscriptionInput(input)
	if !valid {
		return nil, syncError("BAD_REQUEST", "Invalid document share state request.")
	}
	if s.workspace.ShareState == nil || s.workspace.GrantsAuth == nil {
		return nil, syncError("INTERNAL_SERVER_ERROR", "Document share state is unavailable.")
	}
	access, err := s.workspace.GrantsAuth.AuthorizeDocument(ctx, userID, workspaceID, docID)
	if err != nil {
		return nil, docGrantsError(err)
	}
	if !access.Read {
		return nil, syncError("DOC_ACTION_DENIED", "Document read access is denied.")
	}
	state, err := s.workspace.ShareState.ReadDocShareState(ctx, workspaceID, docID)
	if err != nil {
		return nil, syncError("INTERNAL_SERVER_ERROR", "The request could not be completed.")
	}
	return map[string]any{"state": state}, nil
}

func docShareStateRoom(workspaceID, docID string) string {
	return "workspace:" + workspaceID + ":doc:" + docID + ":share-state"
}
