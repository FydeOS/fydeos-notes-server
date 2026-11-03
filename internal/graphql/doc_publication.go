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
	"github.com/fydeos/fydeos-notes-server/internal/graphql/model"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
	"github.com/vektah/gqlparser/v2/gqlerror"
)

type DocumentPublicationMutator interface {
	SetDocPublished(context.Context, string, string, string, int, bool) (workspaces.PublishedDocumentMetadata, error)
}

type DocumentPublicStatePublisher interface {
	PublishDocumentPublicStateChanged(workspaceID, docID string)
}

type DocumentPublicationDependencies struct {
	Service   DocumentPublicationMutator
	Publisher DocumentPublicStatePublisher
}

func (r *mutationResolver) setDocumentPublished(ctx context.Context, workspaceID, docID string, mode *model.PublicDocMode, publish bool) (*model.DocType, error) {
	principal := auth.PrincipalFromContext(ctx)
	if principal == nil {
		return nil, ErrAuthenticationRequired
	}
	failure := func(status int, category, name, message string, data any) (*model.DocType, error) {
		return nil, &gqlerror.Error{Message: message, Extensions: userFriendlyGraphQLExtensions(status, category, name, message, data)}
	}
	inputError := "expect_to_revoke_public_doc"
	if publish {
		inputError = "expect_to_publish_doc"
	}
	if workspaceID == docID {
		message := "Expect doc not to be workspace"
		if publish {
			message = "Expected to publish a doc, not a Space."
		}
		return failure(http.StatusBadRequest, "invalid_input", inputError, message, nil)
	}
	internalFailure := func() (*model.DocType, error) {
		return failure(http.StatusInternalServerError, "internal_server_error", "internal_server_error", "An internal error occurred.", nil)
	}
	// GraphQL supplies Page when omitted. Explicit null reaches the old native
	// i16 decoder as an error and must not silently change into a publish request.
	if r.DocumentPublications.Service == nil || publish && mode == nil {
		return internalFailure()
	}
	modeNumber := 0
	if mode != nil && *mode == model.PublicDocModeEdgeless {
		modeNumber = 1
	}
	doc, err := r.DocumentPublications.Service.SetDocPublished(ctx, principal.User.ID, workspaceID, docID, modeNumber, publish)
	switch {
	case errors.Is(err, workspaces.ErrDocRoleDenied), errors.Is(err, workspaces.ErrNotFound):
		action := "Doc.Unpublish"
		if publish {
			action = "Doc.Publish"
		}
		message := fmt.Sprintf("You do not have permission to perform %s action on doc %s.", action, docID)
		return failure(http.StatusForbidden, "no_permission", "doc_action_denied", message, map[string]string{"spaceId": workspaceID, "docId": docID, "action": action})
	case errors.Is(err, workspaces.ErrDocumentNotFound):
		return failure(http.StatusNotFound, "resource_not_found", "doc_not_found", fmt.Sprintf("Doc %s under Space %s not found.", docID, workspaceID), map[string]string{"spaceId": workspaceID, "docId": docID})
	case errors.Is(err, workspaces.ErrDocumentNotPublic):
		return failure(http.StatusBadRequest, "bad_request", "doc_is_not_public", "Doc is not public.", nil)
	case errors.Is(err, workspaces.ErrDocRoleInputLimit):
		return failure(http.StatusBadRequest, "invalid_input", inputError, "Document identifiers allow at most 1024 bytes.", nil)
	case err != nil:
		return internalFailure()
	}
	if r.DocumentPublications.Publisher != nil {
		r.DocumentPublications.Publisher.PublishDocumentPublicStateChanged(workspaceID, docID)
	}
	resultMode := model.PublicDocModePage
	if doc.Mode == 1 {
		resultMode = model.PublicDocModeEdgeless
	}
	role := model.DocRoleManager
	for _, candidate := range model.AllDocRole {
		if strings.EqualFold(string(candidate), doc.DefaultRole) {
			role = candidate
			break
		}
	}
	return &model.DocType{ID: doc.DocID, WorkspaceID: doc.WorkspaceID, Mode: resultMode, Public: doc.Public, DefaultRole: role, Title: doc.Title, Summary: doc.Summary}, nil
}
