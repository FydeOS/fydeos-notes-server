// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"fmt"

	"github.com/fydeos/fydeos-notes-server/internal/graphql/model"
)

func workspacePermissionsModel(values map[string]bool) (*model.WorkspacePermissions, error) {
	permissions := &model.WorkspacePermissions{}
	fields := map[string]*bool{
		"Workspace_Administrators_Manage": &permissions.WorkspaceAdministratorsManage,
		"Workspace_Blobs_Manage":          &permissions.WorkspaceBlobsManage,
		"Workspace_Blobs_Upload":          &permissions.WorkspaceBlobsUpload,
		"Workspace_Copilot":               &permissions.WorkspaceCopilot,
		"Workspace_CreateDoc":             &permissions.WorkspaceCreateDoc,
		"Workspace_Delete":                &permissions.WorkspaceDelete,
		"Workspace_Organize_Read":         &permissions.WorkspaceOrganizeRead,
		"Workspace_Payment_Manage":        &permissions.WorkspacePaymentManage,
		"Workspace_Preview":               &permissions.WorkspacePreview,
		"Workspace_Properties_Create":     &permissions.WorkspacePropertiesCreate,
		"Workspace_Properties_Delete":     &permissions.WorkspacePropertiesDelete,
		"Workspace_Properties_Read":       &permissions.WorkspacePropertiesRead,
		"Workspace_Properties_Update":     &permissions.WorkspacePropertiesUpdate,
		"Workspace_Read":                  &permissions.WorkspaceRead,
		"Workspace_Settings_Read":         &permissions.WorkspaceSettingsRead,
		"Workspace_Settings_Update":       &permissions.WorkspaceSettingsUpdate,
		"Workspace_Sync":                  &permissions.WorkspaceSync,
		"Workspace_TransferOwner":         &permissions.WorkspaceTransferOwner,
		"Workspace_Users_Manage":          &permissions.WorkspaceUsersManage,
		"Workspace_Users_Read":            &permissions.WorkspaceUsersRead,
	}
	if len(values) != len(fields) {
		return nil, fmt.Errorf("%w: got %d actions, want %d", ErrWorkspacePermissionProjectionIncomplete, len(values), len(fields))
	}
	for name, destination := range fields {
		value, ok := values[name]
		if !ok {
			return nil, fmt.Errorf("%w: missing %s", ErrWorkspacePermissionProjectionIncomplete, name)
		}
		*destination = value
	}
	return permissions, nil
}
