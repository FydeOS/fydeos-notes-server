// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
	"github.com/vektah/gqlparser/v2/gqlerror"
)

type DocumentRoleMutator interface {
	GrantDocRoles(context.Context, string, string, string, []string, string) error
	TransitionDocRole(context.Context, string, string, string, string, string) error
	SetDocDefaultRole(context.Context, string, string, string, string) error
}

type DocumentRolePublisher interface {
	PublishDocumentRoleChanged(workspaceID, docID, reason string)
}

type DocumentRoleDependencies struct {
	Service   DocumentRoleMutator
	Publisher DocumentRolePublisher
}

func (r *mutationResolver) mutateDocumentRole(ctx context.Context, operation, workspaceID, docID, role string, targets []string) (bool, error) {
	principal := auth.PrincipalFromContext(ctx)
	if principal == nil {
		return false, ErrAuthenticationRequired
	}
	invalid := func(name, message string, pairs bool) (bool, error) {
		var data any
		if pairs {
			data = map[string]string{"spaceId": workspaceID, "docId": docID}
		}
		return false, &gqlerror.Error{Message: message, Extensions: userFriendlyGraphQLExtensions(http.StatusBadRequest, "invalid_input", name, message, data)}
	}
	if operation == "default" && role == "Owner" {
		return invalid("doc_default_role_can_not_be_owner", "Doc default role can not be owner.", false)
	}
	invalidName := "expect_to_update_doc_user_role"
	if operation == "grant" {
		invalidName = "expect_to_grant_doc_user_roles"
	}
	if operation == "revoke" {
		invalidName = "expect_to_revoke_doc_user_roles"
	}
	if workspaceID == docID {
		return invalid(invalidName, "Expect doc not to be workspace", true)
	}
	if operation != "revoke" && (role == "External" || role == "None" && operation != "default" || role == "Owner" && operation == "grant") {
		return invalid(invalidName, "Invalid grant role", true)
	}
	if r.DocumentRoles.Service == nil {
		return false, ErrDocRoleServiceUnavailable
	}
	newRole := strings.ToLower(role)
	var err error
	reason := "grants-changed"
	switch operation {
	case "grant":
		err = r.DocumentRoles.Service.GrantDocRoles(ctx, principal.User.ID, workspaceID, docID, targets, newRole)
	case "revoke", "transition":
		err = r.DocumentRoles.Service.TransitionDocRole(ctx, principal.User.ID, workspaceID, docID, targets[0], newRole)
		if role == "Owner" {
			reason = "owner-changed"
		}
	case "default":
		err = r.DocumentRoles.Service.SetDocDefaultRole(ctx, principal.User.ID, workspaceID, docID, newRole)
		reason = "default-role-changed"
	}
	if errors.Is(err, workspaces.ErrDocRoleDenied) {
		action := "Doc.Users.Manage"
		if operation == "default" {
			action = "Doc.Properties.Update"
		}
		message := fmt.Sprintf("You do not have permission to perform %s action on doc %s.", action, docID)
		return false, &gqlerror.Error{Message: message, Extensions: userFriendlyGraphQLExtensions(http.StatusForbidden, "no_permission", "doc_action_denied", message, map[string]string{"spaceId": workspaceID, "docId": docID, "action": action})}
	}
	if errors.Is(err, workspaces.ErrDocRoleInputLimit) {
		return invalid(invalidName, "Document role requests allow at most 1000 targets and 1024 bytes per identifier.", true)
	}
	if err != nil {
		// Native domain failures were mapped to this generic error by the old
		// exception filter. Do not expose SQL details or target-specific facts.
		message := "An internal error occurred."
		return false, &gqlerror.Error{Message: message, Extensions: userFriendlyGraphQLExtensions(http.StatusInternalServerError, "internal_server_error", "internal_server_error", message, nil)}
	}
	if r.DocumentRoles.Publisher != nil {
		r.DocumentRoles.Publisher.PublishDocumentRoleChanged(workspaceID, docID, reason)
	}
	return true, nil
}

var ErrDocRoleServiceUnavailable = errors.New("document role service unavailable")
