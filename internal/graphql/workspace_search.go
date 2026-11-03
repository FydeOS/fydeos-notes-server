// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"errors"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/graphql/model"
	"github.com/fydeos/fydeos-notes-server/internal/search"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
	"github.com/vektah/gqlparser/v2/gqlerror"
)

type batchDocumentAuthorsReader interface {
	ReadDocumentAuthorsBatch(context.Context, string, []string) (map[string]workspaces.DocumentAuthors, error)
}

func (r *Resolver) workspaceSearchDocs(ctx context.Context, workspace *model.WorkspaceType, input model.SearchDocsInput) ([]*model.SearchDocObjectType, error) {
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
	if !access.Permissions["Workspace_Read"] {
		return nil, &WorkspacePermissionsDeniedError{WorkspaceID: workspace.ID}
	}
	if input.Limit != nil && (*input.Limit <= 0 || *input.Limit > 10000) {
		return nil, documentListBadRequest("searchDocs limit must be between 1 and 10000")
	}
	if r.SearchDocs == nil {
		return nil, searchIndexNotReadyError(workspace.ID)
	}
	limit := 20
	if input.Limit != nil {
		limit = *input.Limit
	}
	candidateLimit := 10_000
	if limit < 2500 {
		candidateLimit = limit * 4
	}
	results := make([]*model.SearchDocObjectType, 0, limit)
	if r.DocumentReader == nil {
		return nil, documentReadError(workspaces.ErrAuthorizationUnavailable, workspace.ID, "", "Doc.Read")
	}
	for offset := 0; ; offset += candidateLimit {
		candidates, err := r.SearchDocs.SearchDocsPage(ctx, workspace.ID, input.Keyword, candidateLimit, offset)
		if err != nil {
			if errors.Is(err, search.ErrInvalidSearchDocsInput) {
				return nil, documentListBadRequest("Invalid searchDocs input")
			}
			return nil, err
		}
		docIDs := make([]string, 0, len(candidates))
		for _, candidate := range candidates {
			docIDs = append(docIDs, candidate.DocID)
		}
		permissionsByDoc := make(map[string]map[string]bool, len(docIDs))
		if batchReader, ok := r.DocumentReader.(batchDocumentPermissionReader); ok {
			permissionsByDoc, err = batchReader.ReadDocumentPermissionsBatch(ctx, principal.User.ID, workspace.ID, docIDs)
			if errors.Is(err, workspaces.ErrNotFound) {
				return nil, documentWorkspaceReadError(err, workspace.ID)
			} else if errors.Is(err, workspaces.ErrDocRoleDenied) || errors.Is(err, workspaces.ErrDocumentNotFound) {
				permissionsByDoc = map[string]map[string]bool{}
			} else if err != nil {
				return nil, documentReadError(err, workspace.ID, "", "Doc.Read")
			}
		} else {
			for _, candidate := range candidates {
				permissions, permissionErr := r.DocumentReader.ReadDocumentPermissions(ctx, principal.User.ID, workspace.ID, candidate.DocID)
				if errors.Is(permissionErr, workspaces.ErrDocRoleDenied) || errors.Is(permissionErr, workspaces.ErrDocumentNotFound) {
					continue
				}
				if permissionErr != nil {
					if errors.Is(permissionErr, workspaces.ErrNotFound) {
						return nil, documentWorkspaceReadError(permissionErr, workspace.ID)
					}
					return nil, documentReadError(permissionErr, workspace.ID, candidate.DocID, "Doc.Read")
				}
				permissionsByDoc[candidate.DocID] = permissions
			}
		}
		visibleCandidates := make([]search.DocResult, 0, min(len(candidates), limit-len(results)))
		for _, candidate := range candidates {
			if permissionsByDoc[candidate.DocID]["Doc.Read"] {
				visibleCandidates = append(visibleCandidates, candidate)
				if len(visibleCandidates) == limit-len(results) {
					break
				}
			}
		}
		authorsByDoc := make(map[string]workspaces.DocumentAuthors, len(visibleCandidates))
		if batchReader, ok := r.DocumentReader.(batchDocumentAuthorsReader); ok {
			visibleDocIDs := make([]string, 0, len(visibleCandidates))
			for _, candidate := range visibleCandidates {
				visibleDocIDs = append(visibleDocIDs, candidate.DocID)
			}
			authorsByDoc, err = batchReader.ReadDocumentAuthorsBatch(ctx, workspace.ID, visibleDocIDs)
			if err != nil {
				return nil, documentReadError(err, workspace.ID, "", "Doc.Read")
			}
		} else {
			for _, candidate := range visibleCandidates {
				authors, authorErr := r.DocumentReader.ReadDocumentAuthors(ctx, workspace.ID, candidate.DocID)
				if errors.Is(authorErr, workspaces.ErrDocumentNotFound) {
					continue
				}
				if authorErr != nil {
					return nil, documentReadError(authorErr, workspace.ID, candidate.DocID, "Doc.Read")
				}
				authorsByDoc[candidate.DocID] = authors
			}
		}
		for _, candidate := range visibleCandidates {
			authors := authorsByDoc[candidate.DocID]
			result := &model.SearchDocObjectType{
				DocID: candidate.DocID, Title: candidate.Title, BlockID: candidate.BlockID,
				Highlight: candidate.Highlight, CreatedAt: *createdAt(&candidate.CreatedAt), UpdatedAt: *createdAt(&candidate.UpdatedAt),
			}
			if authors.CreatedBy != nil {
				result.CreatedByUser = &model.PublicUserType{ID: authors.CreatedBy.ID, Name: authors.CreatedBy.Name, AvatarURL: authors.CreatedBy.AvatarURL}
			}
			if authors.UpdatedBy != nil {
				result.UpdatedByUser = &model.PublicUserType{ID: authors.UpdatedBy.ID, Name: authors.UpdatedBy.Name, AvatarURL: authors.UpdatedBy.AvatarURL}
			}
			results = append(results, result)
			if len(results) == limit {
				return results, nil
			}
		}
		if len(candidates) < candidateLimit {
			break
		}
	}
	return results, nil
}

func searchIndexNotReadyError(workspace string) error {
	message := "Search index is not ready for Space " + workspace + "."
	return &gqlerror.Error{Message: message, Extensions: userFriendlyGraphQLExtensions(503, "service_unavailable", "search_index_not_ready", message, map[string]string{"spaceId": workspace})}
}
