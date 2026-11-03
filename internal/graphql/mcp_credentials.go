// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"errors"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/graphql/model"
	"github.com/fydeos/fydeos-notes-server/internal/mcpcredentials"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

func (r *Resolver) mcpCredentialReadWriteAvailable(ctx context.Context) (bool, error) {
	if err := r.requireCopilotEnabled(ctx); err != nil {
		return false, err
	}
	return r.MCPCredentials.ReadWriteAvailable(), nil
}

func (r *Resolver) mcpCredentials(ctx context.Context, workspaceID string) ([]*model.McpCredentialType, error) {
	if err := r.requireCopilotEnabled(ctx); err != nil {
		return nil, err
	}
	principal := auth.PrincipalFromContext(ctx)
	if principal == nil {
		return nil, ErrAuthenticationRequired
	}
	if err := r.authorizeMCPCredentialWorkspace(ctx, principal.User.ID, workspaceID); err != nil {
		return nil, err
	}
	credentials, err := r.MCPCredentials.List(ctx, principal.User.ID, workspaceID)
	if err != nil {
		return nil, err
	}
	result := make([]*model.McpCredentialType, 0, len(credentials))
	for _, credential := range credentials {
		result = append(result, mcpCredentialModel(credential))
	}
	return result, nil
}

func (r *Resolver) createMcpCredential(ctx context.Context, input model.CreateMcpCredentialInput) (*model.RevealedMcpCredentialType, error) {
	if err := r.requireCopilotEnabled(ctx); err != nil {
		return nil, err
	}
	principal := auth.PrincipalFromContext(ctx)
	if principal == nil {
		return nil, ErrAuthenticationRequired
	}
	accessMode := string(input.AccessMode)
	if accessMode == "READ_WRITE" && !r.MCPCredentials.ReadWriteAvailable() {
		return nil, mcpcredentials.ErrWriteToolsUnavailable
	}
	if err := r.authorizeMCPCredentialWorkspace(ctx, principal.User.ID, input.WorkspaceID); err != nil {
		return nil, err
	}
	issued, err := r.MCPCredentials.Create(ctx, mcpcredentials.CreateInput{
		UserID: principal.User.ID, WorkspaceID: input.WorkspaceID,
		Name: input.Name, AccessMode: accessMode, ExpirationDays: input.ExpirationDays,
	})
	if err != nil {
		return nil, err
	}
	return revealedMCPCredentialModel(issued), nil
}

func (r *Resolver) rotateMcpCredential(ctx context.Context, id, workspaceID string, expirationDays int) (*model.RevealedMcpCredentialType, error) {
	if err := r.requireCopilotEnabled(ctx); err != nil {
		return nil, err
	}
	principal := auth.PrincipalFromContext(ctx)
	if principal == nil {
		return nil, ErrAuthenticationRequired
	}
	if err := r.authorizeMCPCredentialWorkspace(ctx, principal.User.ID, workspaceID); err != nil {
		return nil, err
	}
	issued, err := r.MCPCredentials.Rotate(ctx, id, principal.User.ID, workspaceID, expirationDays)
	if err != nil {
		return nil, err
	}
	return revealedMCPCredentialModel(issued), nil
}

func (r *Resolver) revokeMcpCredential(ctx context.Context, id, workspaceID string) (bool, error) {
	if err := r.requireCopilotEnabled(ctx); err != nil {
		return false, err
	}
	principal := auth.PrincipalFromContext(ctx)
	if principal == nil {
		return false, ErrAuthenticationRequired
	}
	if err := r.authorizeMCPCredentialWorkspace(ctx, principal.User.ID, workspaceID); err != nil {
		return false, err
	}
	return r.MCPCredentials.Revoke(ctx, id, principal.User.ID, workspaceID)
}

func (r *Resolver) requireCopilotEnabled(ctx context.Context) error {
	if r.MCPCredentials == nil {
		return mcpcredentials.ErrUnavailable
	}
	enabled, err := r.MCPCredentials.CopilotEnabled(ctx)
	if err != nil {
		return err
	}
	if !enabled {
		return mcpcredentials.ErrCopilotDisabled
	}
	return nil
}

func (r *Resolver) authorizeMCPCredentialWorkspace(ctx context.Context, userID, workspaceID string) error {
	if r.WorkspacePermissionReader == nil {
		return ErrWorkspaceRolePermissionsUnavailable
	}
	access, err := r.WorkspacePermissionReader.GetWorkspaceRolePermissions(ctx, userID, workspaceID)
	if errors.Is(err, workspaces.ErrAccessDenied) || errors.Is(err, workspaces.ErrNotFound) {
		return &WorkspacePermissionsDeniedError{WorkspaceID: workspaceID}
	}
	if err != nil {
		return err
	}
	if !access.Permissions["Workspace_Read"] {
		return &WorkspacePermissionsDeniedError{WorkspaceID: workspaceID}
	}
	return nil
}

func mcpCredentialModel(credential mcpcredentials.Credential) *model.McpCredentialType {
	return &model.McpCredentialType{
		ID: credential.ID, Name: credential.Name, WorkspaceID: credential.WorkspaceID,
		AccessMode: model.McpAccessMode(credential.AccessMode), Fingerprint: credential.Fingerprint,
		CreatedAt:  credential.CreatedAt.UTC().Format(time.RFC3339Nano),
		ExpiresAt:  credential.ExpiresAt.UTC().Format(time.RFC3339Nano),
		LastUsedAt: mcpCredentialTime(credential.LastUsedAt), RevokedAt: mcpCredentialTime(credential.RevokedAt),
		GraceEndsAt: mcpCredentialTime(credential.GraceEndsAt), Status: model.McpCredentialStatus(credential.Status),
	}
}

func revealedMCPCredentialModel(issued mcpcredentials.Issued) *model.RevealedMcpCredentialType {
	return &model.RevealedMcpCredentialType{Credential: mcpCredentialModel(issued.Credential), Token: issued.Token}
}

func mcpCredentialTime(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := value.UTC().Format(time.RFC3339Nano)
	return &formatted
}
