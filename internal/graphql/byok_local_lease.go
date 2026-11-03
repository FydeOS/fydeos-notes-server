// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"errors"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/byok"
	"github.com/fydeos/fydeos-notes-server/internal/graphql/model"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

func (r *Resolver) createWorkspaceByokLocalLease(ctx context.Context, input model.CreateWorkspaceByokLocalLeaseInput) (*model.CreateWorkspaceByokLocalLeaseResultType, error) {
	if err := r.requireCopilotEnabled(ctx); err != nil {
		return nil, err
	}
	principal := auth.PrincipalFromContext(ctx)
	if principal == nil {
		return nil, ErrAuthenticationRequired
	}
	if r.WorkspacePermissionReader == nil {
		return nil, ErrWorkspaceRolePermissionsUnavailable
	}
	access, err := r.WorkspacePermissionReader.GetWorkspaceRolePermissions(ctx, principal.User.ID, input.WorkspaceID)
	if errors.Is(err, workspaces.ErrAccessDenied) || errors.Is(err, workspaces.ErrNotFound) {
		return nil, &WorkspacePermissionsDeniedError{WorkspaceID: input.WorkspaceID}
	}
	if err != nil {
		return nil, err
	}
	if !access.Permissions["Workspace_Copilot"] || !access.Permissions["Workspace_Settings_Update"] {
		return nil, &WorkspacePermissionsDeniedError{WorkspaceID: input.WorkspaceID}
	}
	if r.BYOKProfiles == nil {
		return nil, ErrBYOKProfilesUnavailable
	}
	entitlement, err := r.BYOKProfiles.Entitlement(ctx, input.WorkspaceID, principal.User.ID)
	if err != nil {
		return nil, err
	}
	if !entitlement.Local {
		return nil, byok.ErrEntitlementRequired
	}

	for _, provider := range input.Providers {
		if provider == nil {
			return nil, byok.ErrInvalidInput
		}
		if !provider.Description.IsSet() {
			return nil, ErrBYOKDescriptionRequired
		}
	}
	providers := make([]byok.LocalLeaseProviderInput, 0, len(input.Providers))
	for _, provider := range input.Providers {
		definition, err := byokDefinitionInput(provider.Definition)
		if err != nil {
			return nil, err
		}
		providers = append(providers, byok.LocalLeaseProviderInput{
			Provider: string(provider.Provider), Name: provider.Name, Description: provider.Description.Value(),
			Credential: provider.Credential, Definition: definition, Enabled: provider.Enabled,
		})
	}
	lease, err := r.BYOKProfiles.CreateLocalLease(ctx, byok.CreateLocalLeaseInput{
		WorkspaceID: input.WorkspaceID, UserID: principal.User.ID, Providers: providers,
	})
	if err != nil {
		return nil, err
	}
	return &model.CreateWorkspaceByokLocalLeaseResultType{
		LeaseID: lease.LeaseID, ExpiresAt: time.UnixMilli(lease.ExpiresAtMS).UTC().Format(time.RFC3339Nano),
	}, nil
}
