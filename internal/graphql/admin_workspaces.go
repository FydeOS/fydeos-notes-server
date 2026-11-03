// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"net/http"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/graphql/model"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
	"github.com/vektah/gqlparser/v2/gqlerror"
)

type AdminWorkspaceReader interface {
	ListAdminWorkspaces(context.Context, workspaces.AdminWorkspaceFilter) ([]workspaces.AdminWorkspace, error)
	CountAdminWorkspaces(context.Context, workspaces.AdminWorkspaceFilter) (int, error)
	GetAdminWorkspace(context.Context, string) (*workspaces.AdminWorkspace, error)
	ListAdminWorkspaceMembers(context.Context, string, int, int, *string) ([]workspaces.AdminWorkspaceMember, error)
	ListAdminWorkspaceSharedLinks(context.Context, string) ([]workspaces.AdminWorkspaceSharedLink, error)
}

type AdminWorkspaceUpdater interface {
	UpdateAdminWorkspace(context.Context, string, workspaces.AdminWorkspaceUpdate) error
}

func (r *Resolver) adminWorkspaces(ctx context.Context, filter model.ListWorkspaceInput) ([]*model.AdminWorkspace, error) {
	if err := requireCloudAdministrator(ctx, r.Config); err != nil {
		return nil, err
	}
	reader, err := r.adminWorkspaceReader()
	if err != nil {
		return nil, adminWorkspaceInternalError()
	}
	rows, err := reader.ListAdminWorkspaces(ctx, adminWorkspaceFilter(filter))
	if err != nil {
		return nil, adminWorkspaceInternalError()
	}
	result := make([]*model.AdminWorkspace, 0, len(rows))
	for i := range rows {
		result = append(result, adminWorkspaceModel(&rows[i]))
	}
	return result, nil
}

func (r *Resolver) adminWorkspacesCount(ctx context.Context, filter model.ListWorkspaceInput) (int, error) {
	if err := requireCloudAdministrator(ctx, r.Config); err != nil {
		return 0, err
	}
	reader, err := r.adminWorkspaceReader()
	if err != nil {
		return 0, adminWorkspaceInternalError()
	}
	total, err := reader.CountAdminWorkspaces(ctx, adminWorkspaceFilter(filter))
	if err != nil {
		return 0, adminWorkspaceInternalError()
	}
	return total, nil
}

func (r *Resolver) adminWorkspace(ctx context.Context, id string) (*model.AdminWorkspace, error) {
	if err := requireCloudAdministrator(ctx, r.Config); err != nil {
		return nil, err
	}
	reader, err := r.adminWorkspaceReader()
	if err != nil {
		return nil, adminWorkspaceInternalError()
	}
	workspace, err := reader.GetAdminWorkspace(ctx, id)
	if err != nil {
		return nil, adminWorkspaceInternalError()
	}
	if workspace == nil {
		return nil, nil
	}
	return adminWorkspaceModel(workspace), nil
}

func (r *Resolver) adminUpdateWorkspace(ctx context.Context, input model.AdminUpdateWorkspaceInput) (*model.AdminWorkspace, error) {
	if err := requireCloudAdministrator(ctx, r.Config); err != nil {
		return nil, err
	}
	reader, err := r.adminWorkspaceReader()
	if err != nil {
		return nil, adminWorkspaceInternalError()
	}
	if (input.EnableAi.IsSet() && input.EnableAi.Value() == nil) ||
		(input.EnableDocEmbedding.IsSet() && input.EnableDocEmbedding.Value() == nil) {
		return nil, adminWorkspaceInternalError()
	}
	update := workspaces.AdminWorkspaceUpdate{
		Public: input.Public.Value(), EnableAI: input.EnableAi.Value(), EnableDocEmbedding: input.EnableDocEmbedding.Value(),
		EnableSharing: input.EnableSharing.Value(), EnableSharingSet: input.EnableSharing.IsSet(),
		EnableURLPreview: input.EnableURLPreview.Value(), EnableURLPreviewSet: input.EnableURLPreview.IsSet(),
	}
	// The legacy policy mapper treats explicit null public as private.
	if input.Public.IsSet() && update.Public == nil {
		update.Public = new(bool)
	}
	update.Name, update.NameSet = input.Name.ValueOK()
	update.AvatarKey, update.AvatarKeySet = input.AvatarKey.ValueOK()
	updateChanged := input.Public.IsSet() || input.EnableAi.IsSet() || input.EnableSharing.IsSet() ||
		input.EnableURLPreview.IsSet() || input.EnableDocEmbedding.IsSet() || update.NameSet || update.AvatarKeySet

	if updateChanged {
		updater, ok := r.WorkspaceRepo.(AdminWorkspaceUpdater)
		if !ok || updater == nil {
			return nil, adminWorkspaceInternalError()
		}
		if err := updater.UpdateAdminWorkspace(ctx, input.ID, update); err != nil {
			return nil, adminWorkspaceInternalError()
		}
		if r.WorkspaceConfigPublisher != nil {
			r.WorkspaceConfigPublisher.PublishWorkspaceConfigUpdated(input.ID)
		}
	}
	workspace, err := reader.GetAdminWorkspace(ctx, input.ID)
	if err != nil {
		return nil, adminWorkspaceInternalError()
	}
	if workspace == nil {
		return nil, nil
	}
	return adminWorkspaceModel(workspace), nil
}

func (r *Resolver) adminWorkspaceMembers(ctx context.Context, parent *model.AdminWorkspace, skip, take *int, query *string) ([]*model.AdminWorkspaceMember, error) {
	reader, err := r.adminWorkspaceReader()
	if err != nil {
		return nil, adminWorkspaceInternalError()
	}
	offset, pageSize := 0, 20
	if skip != nil {
		offset = *skip
	}
	if take != nil {
		pageSize = *take
	}
	rows, err := reader.ListAdminWorkspaceMembers(ctx, parent.ID, offset, pageSize, query)
	if err != nil {
		return nil, adminWorkspaceInternalError()
	}
	result := make([]*model.AdminWorkspaceMember, 0, len(rows))
	for _, row := range rows {
		result = append(result, &model.AdminWorkspaceMember{
			ID: row.ID, Name: row.Name, Email: row.Email, AvatarURL: row.AvatarURL,
			Role: model.AdminWorkspaceMemberRole(row.Role), Status: model.WorkspaceMemberStatus(row.Status),
		})
	}
	return result, nil
}

func (r *Resolver) adminWorkspaceSharedLinks(ctx context.Context, parent *model.AdminWorkspace) ([]*model.AdminWorkspaceSharedLink, error) {
	reader, err := r.adminWorkspaceReader()
	if err != nil {
		return nil, adminWorkspaceInternalError()
	}
	rows, err := reader.ListAdminWorkspaceSharedLinks(ctx, parent.ID)
	if err != nil {
		return nil, adminWorkspaceInternalError()
	}
	result := make([]*model.AdminWorkspaceSharedLink, 0, len(rows))
	for _, row := range rows {
		result = append(result, &model.AdminWorkspaceSharedLink{
			DocID: row.DocID, Title: row.Title, PublishedAt: createdAt(row.PublishedAt),
		})
	}
	return result, nil
}

func (r *Resolver) adminWorkspaceReader() (AdminWorkspaceReader, error) {
	reader, ok := r.WorkspaceRepo.(AdminWorkspaceReader)
	if !ok || reader == nil {
		return nil, ErrWorkspaceRepositoryUnavailable
	}
	return reader, nil
}

func adminWorkspaceFilter(input model.ListWorkspaceInput) workspaces.AdminWorkspaceFilter {
	filter := workspaces.AdminWorkspaceFilter{
		First: input.First, Skip: input.Skip,
		Keyword: input.Keyword, Public: input.Public, EnableAI: input.EnableAi,
		EnableSharing: input.EnableSharing, EnableURLPreview: input.EnableURLPreview,
		EnableDocEmbedding: input.EnableDocEmbedding,
	}
	if input.OrderBy != nil {
		filter.OrderBy = string(*input.OrderBy)
	}
	return filter
}

func adminWorkspaceModel(workspace *workspaces.AdminWorkspace) *model.AdminWorkspace {
	result := &model.AdminWorkspace{
		ID: workspace.ID, Public: workspace.Public, CreatedAt: workspace.CreatedAt.UTC().Format(time.RFC3339Nano),
		Name: workspace.Name, AvatarKey: workspace.AvatarKey, EnableAi: workspace.EnableAI,
		EnableSharing: workspace.EnableSharing, EnableURLPreview: workspace.EnableURLPreview,
		EnableDocEmbedding: workspace.EnableDocEmbedding, MemberCount: int(workspace.MemberCount),
		PublicPageCount: int(workspace.PublicPageCount), SnapshotCount: int(workspace.SnapshotCount),
		SnapshotSize: workspace.SnapshotSize, BlobCount: int(workspace.BlobCount), BlobSize: workspace.BlobSize,
	}
	if workspace.Owner != nil {
		result.Owner = &model.WorkspaceUserType{
			ID: workspace.Owner.ID, Name: workspace.Owner.Name, Email: workspace.Owner.Email,
			AvatarURL: workspace.Owner.AvatarURL,
		}
	}
	return result
}

func requireCloudAdministrator(ctx context.Context, cfg config.Config) error {
	if err := requireAdministrator(ctx); err != nil {
		return err
	}
	if cfg.Deployment == "affine" {
		return nil
	}
	message := "Not Found"
	return &gqlerror.Error{Message: message, Extensions: userFriendlyGraphQLExtensions(http.StatusNotFound, "resource_not_found", "NOT_FOUND", message, nil)}
}

func adminWorkspaceInternalError() error {
	message := "An internal error occurred."
	return &gqlerror.Error{Message: message, Extensions: userFriendlyGraphQLExtensions(http.StatusInternalServerError, "internal_server_error", "internal_server_error", message, nil)}
}
