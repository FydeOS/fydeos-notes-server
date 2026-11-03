// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/documentidentity"
	"github.com/fydeos/fydeos-notes-server/internal/documents"
	"github.com/fydeos/fydeos-notes-server/internal/graphql/model"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
	"github.com/vektah/gqlparser/v2/gqlerror"
)

type WorkspaceDocumentHistoryRepository interface {
	ListWorkspaceDocHistories(context.Context, string, string, time.Time, int) ([]documents.WorkspaceDocHistory, error)
	RecoverWorkspaceDoc(context.Context, string, string, string, time.Time, int64) error
}

type DocumentHistoryAccessReader interface {
	ReadDocumentHistoryAccess(context.Context, string, string, string) (workspaces.DocumentHistoryAccess, error)
}

type DocumentHistoryDependencies struct {
	Repository WorkspaceDocumentHistoryRepository
	Access     DocumentHistoryAccessReader
}

func canonicalHistoryDocument(workspaceID, guid string) (documentidentity.Identity, error) {
	identity, err := documentidentity.Canonicalize(guid, workspaceID)
	if err != nil {
		message := "Invalid document identity."
		return documentidentity.Identity{}, &gqlerror.Error{Message: message, Extensions: userFriendlyGraphQLExtensions(http.StatusBadRequest, "invalid_input", "invalid_document_identity", message, nil)}
	}
	return identity, nil
}

func (r *Resolver) documentHistoryAccess(ctx context.Context, workspaceID, guid string) (string, string, workspaces.DocumentHistoryAccess, error) {
	principal := auth.PrincipalFromContext(ctx)
	if principal == nil {
		return "", "", workspaces.DocumentHistoryAccess{}, ErrAuthenticationRequired
	}
	identity, err := canonicalHistoryDocument(workspaceID, guid)
	if err != nil {
		return "", "", workspaces.DocumentHistoryAccess{}, err
	}
	if r.DocumentHistory.Access == nil {
		return "", "", workspaces.DocumentHistoryAccess{}, documentReadError(workspaces.ErrAuthorizationUnavailable, workspaceID, identity.DocumentID, "Doc.History.Read")
	}
	access, err := r.DocumentHistory.Access.ReadDocumentHistoryAccess(ctx, principal.User.ID, identity.WorkspaceID, identity.DocumentID)
	if err != nil {
		if errors.Is(err, workspaces.ErrNotFound) {
			return "", "", workspaces.DocumentHistoryAccess{}, documentWorkspaceReadError(err, workspaceID)
		}
		if errors.Is(err, workspaces.ErrPermissionGenerationChanged) {
			return "", "", workspaces.DocumentHistoryAccess{}, err
		}
		return "", "", workspaces.DocumentHistoryAccess{}, documentReadError(workspaces.ErrAuthorizationUnavailable, workspaceID, identity.DocumentID, "Doc.History.Read")
	}
	return principal.User.ID, identity.DocumentID, access, nil
}

func parseHistoryDateTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err == nil {
		return parsed.UTC().Truncate(time.Millisecond), nil
	}
	message := "Invalid history timestamp."
	return time.Time{}, &gqlerror.Error{Message: message, Extensions: userFriendlyGraphQLExtensions(http.StatusBadRequest, "invalid_input", "invalid_history_timestamp", message, map[string]string{"timestamp": value})}
}

func formatHistoryDateTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

func (r *Resolver) documentHistories(ctx context.Context, workspaceID string, before *string, guid string, take *int) ([]*model.DocHistoryType, error) {
	var docID string
	var access workspaces.DocumentHistoryAccess
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		_, docID, access, err = r.documentHistoryAccess(ctx, workspaceID, guid)
		if !errors.Is(err, workspaces.ErrPermissionGenerationChanged) {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	if !access.Read {
		return nil, documentReadError(workspaces.ErrDocRoleDenied, workspaceID, docID, "Doc.History.Read")
	}
	if r.DocumentHistory.Repository == nil {
		return nil, documentReadError(workspaces.ErrAuthorizationUnavailable, workspaceID, docID, "Doc.History.Read")
	}
	beforeTime := time.Now().UTC()
	if before != nil {
		beforeTime, err = parseHistoryDateTime(*before)
		if err != nil {
			return nil, err
		}
	}
	limit := 100
	if take != nil {
		limit = *take
	}
	histories, err := r.DocumentHistory.Repository.ListWorkspaceDocHistories(ctx, workspaceID, docID, beforeTime, limit)
	if err != nil {
		return nil, documentReadError(workspaces.ErrAuthorizationUnavailable, workspaceID, docID, "Doc.History.Read")
	}
	result := make([]*model.DocHistoryType, 0, len(histories))
	for _, history := range histories {
		item := &model.DocHistoryType{WorkspaceID: workspaceID, ID: docID, Timestamp: formatHistoryDateTime(history.Timestamp)}
		if history.Editor != nil {
			item.Editor = &model.EditorType{Name: history.Editor.Name, AvatarURL: history.Editor.AvatarURL}
		}
		result = append(result, item)
	}
	return result, nil
}

func (r *Resolver) recoverDocumentHistory(ctx context.Context, workspaceID, guid, timestampValue string) (string, error) {
	timestamp, err := parseHistoryDateTime(timestampValue)
	if err != nil {
		return "", err
	}
	principal := auth.PrincipalFromContext(ctx)
	if principal == nil {
		return "", ErrAuthenticationRequired
	}
	identity, err := canonicalHistoryDocument(workspaceID, guid)
	if err != nil {
		return "", err
	}
	if r.DocumentHistory.Repository == nil || r.DocumentHistory.Access == nil {
		return "", documentReadError(workspaces.ErrAuthorizationUnavailable, workspaceID, identity.DocumentID, "Doc.History.Read")
	}

	for attempt := 0; attempt < 3; attempt++ {
		actorID, docID, access, err := r.documentHistoryAccess(ctx, workspaceID, guid)
		if errors.Is(err, workspaces.ErrPermissionGenerationChanged) && attempt < 2 {
			continue
		}
		if err != nil {
			return "", err
		}
		if !access.Read {
			return "", documentReadError(workspaces.ErrDocRoleDenied, workspaceID, docID, "Doc.History.Read")
		}
		if !access.Update {
			return "", documentReadError(workspaces.ErrDocRoleDenied, workspaceID, docID, "Doc.Update")
		}
		err = r.DocumentHistory.Repository.RecoverWorkspaceDoc(ctx, actorID, identity.WorkspaceID, docID, timestamp, access.Generation)
		if errors.Is(err, documents.ErrWorkspacePermissionGenerationChanged) && attempt < 2 {
			continue
		}
		switch {
		case errors.Is(err, documents.ErrWorkspaceDocHistoryNotFound):
			message := fmt.Sprintf("History for doc %s under Space %s not found.", docID, workspaceID)
			return "", &gqlerror.Error{Message: message, Extensions: userFriendlyGraphQLExtensions(http.StatusNotFound, "resource_not_found", "doc_history_not_found", message, map[string]any{"spaceId": workspaceID, "docId": docID, "timestamp": timestamp.UnixMilli()})}
		case errors.Is(err, documents.ErrWorkspaceDocNotFound):
			return "", documentReadError(workspaces.ErrDocumentNotFound, workspaceID, docID, "Doc.History.Read")
		case errors.Is(err, documents.ErrWorkspacePermissionGenerationChanged):
			return "", documentReadError(workspaces.ErrDocRoleDenied, workspaceID, docID, "Doc.History.Read")
		case err != nil:
			return "", documentReadError(workspaces.ErrAuthorizationUnavailable, workspaceID, docID, "Doc.History.Read")
		default:
			return formatHistoryDateTime(timestamp), nil
		}
	}
	return "", documentReadError(workspaces.ErrDocRoleDenied, workspaceID, identity.DocumentID, "Doc.History.Read")
}
