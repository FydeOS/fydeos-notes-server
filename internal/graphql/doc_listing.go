// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/graphql/model"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
	"github.com/vektah/gqlparser/v2/gqlerror"
)

type WorkspaceDocumentListReader interface {
	PaginateWorkspaceDocuments(context.Context, string, string, workspaces.DocumentListInput) (workspaces.DocumentListPage, error)
}

func (r *Resolver) workspaceDocs(ctx context.Context, workspace *model.WorkspaceType, pagination model.PaginationInput) (*model.PaginatedDocType, error) {
	principal := auth.PrincipalFromContext(ctx)
	if principal == nil {
		return nil, ErrAuthenticationRequired
	}
	if r.WorkspacePermissionReader == nil {
		return nil, documentWorkspaceReadError(workspaces.ErrAuthorizationUnavailable, workspace.ID)
	}
	access, err := r.WorkspacePermissionReader.GetWorkspaceRolePermissions(ctx, principal.User.ID, workspace.ID)
	if errors.Is(err, workspaces.ErrAccessDenied) || errors.Is(err, workspaces.ErrNotFound) {
		return nil, &WorkspacePermissionsDeniedError{WorkspaceID: workspace.ID}
	}
	if err != nil {
		return nil, err
	}
	if !access.Permissions["Workspace_Users_Manage"] {
		return nil, &WorkspacePermissionsDeniedError{WorkspaceID: workspace.ID}
	}
	return r.paginateWorkspaceDocuments(ctx, principal.User.ID, workspace.ID, pagination, false)
}

func (r *Resolver) workspaceRecentlyUpdatedDocs(ctx context.Context, workspace *model.WorkspaceType, pagination model.PaginationInput) (*model.PaginatedDocType, error) {
	principal := auth.PrincipalFromContext(ctx)
	if principal == nil {
		return nil, ErrAuthenticationRequired
	}
	return r.paginateWorkspaceDocuments(ctx, principal.User.ID, workspace.ID, pagination, true)
}

func (r *Resolver) paginateWorkspaceDocuments(ctx context.Context, userID, workspaceID string, pagination model.PaginationInput, recentlyUpdated bool) (*model.PaginatedDocType, error) {
	reader, ok := r.DocumentReader.(WorkspaceDocumentListReader)
	if !ok || reader == nil {
		return nil, documentWorkspaceReadError(workspaces.ErrAuthorizationUnavailable, workspaceID)
	}
	first, offset := 10, 0
	if pagination.First != nil {
		first = *pagination.First
	}
	if first < 1 {
		first = 1
	} else if first > 100 {
		first = 100
	}
	if pagination.Offset != nil && *pagination.Offset > 0 {
		offset = *pagination.Offset
	}
	input := workspaces.DocumentListInput{First: first, Offset: offset, RecentlyUpdated: recentlyUpdated}
	if pagination.After != nil && *pagination.After != "" {
		if offset > 0 {
			return nil, documentListBadRequest("pagination.after and pagination.offset cannot be used together")
		}
		decoded, err := base64.StdEncoding.DecodeString(*pagination.After)
		if err != nil {
			return nil, documentListBadRequest("Invalid pagination cursor")
		}
		cursor, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(decoded)))
		if err != nil {
			return nil, documentListBadRequest("Invalid pagination cursor")
		}
		input.After = &cursor
	}
	page, err := reader.PaginateWorkspaceDocuments(ctx, userID, workspaceID, input)
	if err != nil {
		if errors.Is(err, workspaces.ErrAccessDenied) || errors.Is(err, workspaces.ErrNotFound) {
			return nil, documentWorkspaceReadError(err, workspaceID)
		}
		if strings.Contains(err.Error(), "invalid workspace document pagination") {
			return nil, documentListBadRequest("Invalid pagination input")
		}
		return nil, err
	}
	edges := make([]*model.DocTypeEdge, 0, len(page.Documents))
	for _, doc := range page.Documents {
		cursorTime := doc.CreatedAt
		if recentlyUpdated {
			cursorTime = doc.UpdatedAt
		}
		if cursorTime == nil {
			return nil, documentListBadRequest("Document pagination timestamp is unavailable")
		}
		cursor := base64.StdEncoding.EncodeToString([]byte(cursorTime.UTC().Format("2006-01-02T15:04:05.000Z")))
		edges = append(edges, &model.DocTypeEdge{Cursor: cursor, Node: documentModel(doc)})
	}
	var startCursor, endCursor *string
	if len(edges) > 0 {
		start, end := edges[0].Cursor, edges[len(edges)-1].Cursor
		startCursor, endCursor = &start, &end
	}
	return &model.PaginatedDocType{
		TotalCount: page.TotalCount,
		Edges:      edges,
		PageInfo: &model.PageInfo{
			StartCursor:     startCursor,
			EndCursor:       endCursor,
			HasNextPage:     len(edges) >= first,
			HasPreviousPage: (pagination.After != nil && *pagination.After != "") || offset > 0,
		},
	}, nil
}

func documentListBadRequest(message string) error {
	return &gqlerror.Error{Message: message, Extensions: userFriendlyGraphQLExtensions(http.StatusBadRequest, "BAD_REQUEST", "BAD_REQUEST", message, nil)}
}
