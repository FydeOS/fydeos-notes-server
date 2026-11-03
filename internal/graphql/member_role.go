// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import "github.com/fydeos/fydeos-notes-server/internal/graphql/model"

func mapPermissionToWorkspaceRole(permission model.Permission) (string, bool) {
	switch permission {
	case model.PermissionCollaborator:
		return "member", true
	case model.PermissionAdmin:
		return "admin", true
	case model.PermissionOwner:
		return "owner", true
	case model.PermissionExternal:
		return "external", true
	default:
		return "", false
	}
}
