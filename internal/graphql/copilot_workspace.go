// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/99designs/gqlgen/graphql"
	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/copilot"
	"github.com/fydeos/fydeos-notes-server/internal/graphql/model"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

var ErrCopilotWorkspaceUnavailable = errors.New("Copilot workspace settings are unavailable.")

type copilotWorkspaceStore interface {
	ListIgnoredWorkspaceDocs(context.Context, string, int, int) ([]workspaces.IgnoredWorkspaceDoc, int, error)
	ListAllIgnoredWorkspaceDocs(context.Context, string) ([]workspaces.IgnoredWorkspaceDoc, error)
	UpdateIgnoredWorkspaceDocs(context.Context, string, []string, []string) (int, error)
}

func (r *Resolver) workspaceEmbedding(ctx context.Context, workspaceID string) (*model.CopilotWorkspaceConfig, error) {
	if _, err := r.authorizeWorkspaceEmbedding(ctx, workspaceID, "Workspace_Read"); err != nil {
		return nil, err
	}
	return &model.CopilotWorkspaceConfig{WorkspaceID: workspaceID}, nil
}

func (r *Resolver) workspaceIgnoredDocs(ctx context.Context, obj *model.CopilotWorkspaceConfig, pagination model.PaginationInput) (*model.PaginatedIgnoredDocsType, error) {
	_, err := r.authorizeWorkspaceEmbedding(ctx, obj.WorkspaceID, "Workspace_Read")
	if err != nil {
		return nil, err
	}
	store, err := r.copilotWorkspaceStore()
	if err != nil {
		return nil, err
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
	if pagination.After != nil && *pagination.After != "" && offset > 0 {
		return nil, documentListBadRequest("pagination.after and pagination.offset cannot be used together")
	}
	docs, total, err := store.ListIgnoredWorkspaceDocs(ctx, obj.WorkspaceID, first, offset)
	if err != nil {
		return nil, err
	}
	edges := make([]*model.CopilotWorkspaceIgnoredDocTypeEdge, 0, len(docs))
	for _, doc := range docs {
		projected := ignoredWorkspaceDocModel(doc)
		cursor := base64.StdEncoding.EncodeToString([]byte(doc.CreatedAt.UTC().Format(time.RFC3339Nano)))
		edges = append(edges, &model.CopilotWorkspaceIgnoredDocTypeEdge{Cursor: cursor, Node: projected})
	}
	var startCursor, endCursor *string
	if len(edges) > 0 {
		start, end := edges[0].Cursor, edges[len(edges)-1].Cursor
		startCursor, endCursor = &start, &end
	}
	return &model.PaginatedIgnoredDocsType{
		TotalCount: total, Edges: edges,
		PageInfo: &model.PageInfo{
			StartCursor: startCursor, EndCursor: endCursor,
			HasNextPage: len(edges) >= first, HasPreviousPage: offset > 0 || pagination.After != nil && *pagination.After != "",
		},
	}, nil
}

func (r *Resolver) allWorkspaceIgnoredDocs(ctx context.Context, obj *model.CopilotWorkspaceConfig) ([]*model.CopilotWorkspaceIgnoredDoc, error) {
	if _, err := r.authorizeWorkspaceEmbedding(ctx, obj.WorkspaceID, "Workspace_Read"); err != nil {
		return nil, err
	}
	store, err := r.copilotWorkspaceStore()
	if err != nil {
		return nil, err
	}
	docs, err := store.ListAllIgnoredWorkspaceDocs(ctx, obj.WorkspaceID)
	if err != nil {
		return nil, err
	}
	result := make([]*model.CopilotWorkspaceIgnoredDoc, 0, len(docs))
	for _, doc := range docs {
		result = append(result, ignoredWorkspaceDocModel(doc))
	}
	return result, nil
}

func (r *Resolver) workspaceArtifacts(ctx context.Context, obj *model.CopilotWorkspaceConfig, pagination model.PaginationInput) (*model.PaginatedCopilotWorkspaceArtifactType, error) {
	if _, err := r.authorizeWorkspaceEmbedding(ctx, obj.WorkspaceID, "Workspace_Read"); err != nil {
		return nil, err
	}
	if r.CopilotWorkspaceArtifacts == nil {
		return nil, ErrCopilotWorkspaceUnavailable
	}
	first, offset := 10, 0
	if pagination.First != nil {
		first = *pagination.First
	}
	if pagination.Offset != nil && *pagination.Offset > 0 {
		offset = *pagination.Offset
	}
	if pagination.After != nil && *pagination.After != "" && offset > 0 {
		return nil, documentListBadRequest("pagination.after and pagination.offset cannot be used together")
	}
	items, total, err := r.CopilotWorkspaceArtifacts.List(ctx, obj.WorkspaceID, first, offset)
	if err != nil {
		return nil, err
	}
	edges := make([]*model.CopilotWorkspaceArtifactTypeEdge, 0, len(items))
	for _, item := range items {
		cursor := base64.StdEncoding.EncodeToString([]byte(item.CreatedAt.UTC().Format(time.RFC3339Nano)))
		edges = append(edges, &model.CopilotWorkspaceArtifactTypeEdge{Cursor: cursor, Node: workspaceArtifactModel(item)})
	}
	var startCursor, endCursor *string
	if len(edges) > 0 {
		start, end := edges[0].Cursor, edges[len(edges)-1].Cursor
		startCursor, endCursor = &start, &end
	}
	return &model.PaginatedCopilotWorkspaceArtifactType{TotalCount: total, Edges: edges, PageInfo: &model.PageInfo{
		StartCursor: startCursor, EndCursor: endCursor,
		HasNextPage: offset+len(edges) < total, HasPreviousPage: offset > 0 || pagination.After != nil && *pagination.After != "",
	}}, nil
}

func (r *Resolver) addWorkspaceArtifact(ctx context.Context, upload graphql.Upload, workspaceID string) (*model.CopilotWorkspaceArtifact, error) {
	if _, err := r.authorizeWorkspaceEmbedding(ctx, workspaceID, "Workspace_Settings_Update"); err != nil {
		return nil, err
	}
	if r.CopilotWorkspaceArtifacts == nil {
		return nil, ErrCopilotWorkspaceUnavailable
	}
	if !r.CopilotWorkspaceArtifacts.EmbeddingAvailable(ctx) {
		return nil, ErrCopilotEmbeddingUnavailable
	}
	if upload.File == nil {
		return nil, errors.New("workspace artifact upload is required")
	}
	if _, err := upload.File.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	item, err := r.CopilotWorkspaceArtifacts.Add(ctx, workspaceID, upload.Filename, upload.ContentType, upload.File)
	if err != nil {
		return nil, fmt.Errorf("add workspace artifact: %w", err)
	}
	return workspaceArtifactModel(item), nil
}

var ErrCopilotEmbeddingUnavailable = errors.New("Copilot embedding is unavailable")

func (r *Resolver) removeWorkspaceArtifact(ctx context.Context, artifactID, workspaceID string) (bool, error) {
	if _, err := r.authorizeWorkspaceEmbedding(ctx, workspaceID, "Workspace_Settings_Update"); err != nil {
		return false, err
	}
	if r.CopilotWorkspaceArtifacts == nil {
		return false, ErrCopilotWorkspaceUnavailable
	}
	return r.CopilotWorkspaceArtifacts.Remove(ctx, workspaceID, artifactID)
}

func workspaceArtifactModel(item copilot.WorkspaceArtifact) *model.CopilotWorkspaceArtifact {
	return &model.CopilotWorkspaceArtifact{
		WorkspaceID: item.WorkspaceID, ArtifactID: item.ArtifactID, ContentHash: item.ContentHash,
		FileName: item.FileName, EmbeddingStatus: item.EmbeddingStatus, MediaType: item.MediaType,
		Size: item.Size, CreatedAt: item.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
}

func (r *Resolver) updateWorkspaceEmbeddingIgnoredDocs(ctx context.Context, workspaceID string, add, remove []string) (int, error) {
	if _, err := r.authorizeWorkspaceEmbedding(ctx, workspaceID, "Workspace_Settings_Update"); err != nil {
		return 0, err
	}
	store, err := r.copilotWorkspaceStore()
	if err != nil {
		return 0, err
	}
	return store.UpdateIgnoredWorkspaceDocs(ctx, workspaceID, add, remove)
}

func (r *Resolver) authorizeWorkspaceEmbedding(ctx context.Context, workspaceID, permission string) (*auth.Principal, error) {
	principal := auth.PrincipalFromContext(ctx)
	if principal == nil {
		return nil, ErrAuthenticationRequired
	}
	if r.WorkspacePermissionReader == nil {
		return nil, ErrWorkspaceRolePermissionsUnavailable
	}
	access, err := r.WorkspacePermissionReader.GetWorkspaceRolePermissions(ctx, principal.User.ID, workspaceID)
	if errors.Is(err, workspaces.ErrAccessDenied) || errors.Is(err, workspaces.ErrNotFound) || err == nil && !access.Permissions[permission] {
		return nil, &WorkspacePermissionsDeniedError{WorkspaceID: workspaceID}
	}
	if err != nil {
		return nil, err
	}
	return principal, nil
}

func (r *Resolver) copilotWorkspaceStore() (copilotWorkspaceStore, error) {
	store, ok := r.WorkspaceRepo.(copilotWorkspaceStore)
	if !ok || store == nil {
		return nil, ErrCopilotWorkspaceUnavailable
	}
	return store, nil
}

func ignoredWorkspaceDocModel(doc workspaces.IgnoredWorkspaceDoc) *model.CopilotWorkspaceIgnoredDoc {
	return &model.CopilotWorkspaceIgnoredDoc{
		DocID: doc.DocID, CreatedAt: doc.CreatedAt.UTC().Format(time.RFC3339Nano),
		DocCreatedAt: optionalDateTime(doc.DocCreatedAt), DocUpdatedAt: optionalDateTime(doc.DocUpdatedAt),
		Title: nonEmptyString(doc.Title), CreatedBy: nonEmptyString(doc.CreatedBy),
		CreatedByAvatar: nonEmptyString(doc.CreatedByAvatar), UpdatedBy: nonEmptyString(doc.UpdatedBy),
	}
}

func optionalDateTime(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := value.UTC().Format(time.RFC3339Nano)
	return &formatted
}

func nonEmptyString(value *string) *string {
	if value == nil || *value == "" {
		return nil
	}
	return value
}
