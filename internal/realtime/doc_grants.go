// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

var (
	errDocGrantsUnavailable = errors.New("document grants realtime is unavailable")
	errDocGrantsDenied      = errors.New("document grants read access is denied")
	errInvalidDocGrants     = errors.New("invalid document grants request")
)

const docGrantsPageSizeMax = 100

type docGrantsInputKey struct {
	WorkspaceID string `json:"workspaceId"`
	DocID       string `json:"docId"`
}

func (s *Server) docGrants(ctx context.Context, userID string, input map[string]any) (map[string]any, error) {
	if s == nil || s.workspace.UserUpdates == nil || s.workspace.GrantsAuth == nil {
		return nil, errDocGrantsUnavailable
	}
	workspaceID, docID, first, offset, cursor, err := parseDocGrantsInput(input)
	if err != nil {
		return nil, err
	}
	authorization, err := s.workspace.GrantsAuth.AuthorizeDocument(ctx, userID, workspaceID, docID)
	if err != nil {
		return nil, err
	}
	if !authorization.UsersRead {
		return nil, errDocGrantsDenied
	}
	after, err := workspaces.DecodeDocGrantCursor(cursor)
	if err != nil {
		return nil, errInvalidDocGrants
	}
	page, err := s.workspace.UserUpdates.ListDirectDocGrantUsers(ctx, workspaceID, docID, first, offset, after)
	if err != nil {
		return nil, err
	}
	edges := make([]map[string]any, 0, len(page.Users))
	for _, grant := range page.Users {
		edges = append(edges, map[string]any{"node": map[string]any{
			"role": docGrantRoleName(grant.Role),
			"user": map[string]any{
				"id": grant.UserID, "name": grant.Name, "email": grant.Email, "avatarUrl": grant.AvatarURL,
			},
		}})
	}
	return map[string]any{
		"totalCount": page.TotalCount,
		"pageInfo":   map[string]any{"endCursor": page.EndCursor, "hasNextPage": page.HasNext},
		"edges":      edges,
	}, nil
}

func parseDocGrantsInput(input map[string]any) (workspaceID, docID string, first, offset int, cursor string, err error) {
	if len(input) != 3 {
		return "", "", 0, 0, "", errInvalidDocGrants
	}
	for key := range input {
		if key != "workspaceId" && key != "docId" && key != "pagination" {
			return "", "", 0, 0, "", errInvalidDocGrants
		}
	}
	workspaceID = stringField(input, "workspaceId")
	docID = stringField(input, "docId")
	if strings.TrimSpace(workspaceID) == "" || strings.TrimSpace(docID) == "" {
		return "", "", 0, 0, "", errInvalidDocGrants
	}
	pagination, ok := input["pagination"].(map[string]any)
	if !ok {
		return "", "", 0, 0, "", errInvalidDocGrants
	}
	for key := range pagination {
		if key != "first" && key != "offset" && key != "after" {
			return "", "", 0, 0, "", errInvalidDocGrants
		}
	}
	first, hasFirst := integerField(pagination, "first")
	if _, exists := pagination["first"]; exists && !hasFirst {
		return "", "", 0, 0, "", errInvalidDocGrants
	}
	if !hasFirst {
		first = 10
	}
	if first < 1 {
		first = 1
	}
	if first > docGrantsPageSizeMax {
		first = docGrantsPageSizeMax
	}
	offset, hasOffset := integerField(pagination, "offset")
	if _, exists := pagination["offset"]; exists && !hasOffset {
		return "", "", 0, 0, "", errInvalidDocGrants
	}
	if !hasOffset || offset < 0 {
		offset = 0
	}
	if value, exists := pagination["after"]; exists && value != nil {
		var ok bool
		cursor, ok = value.(string)
		if !ok {
			return "", "", 0, 0, "", errInvalidDocGrants
		}
	}
	if cursor != "" && offset > 0 {
		return "", "", 0, 0, "", errInvalidDocGrants
	}
	return workspaceID, docID, first, offset, cursor, nil
}

func docGrantRoleName(role string) string {
	if role == "" {
		return "Reader"
	}
	return strings.ToUpper(role[:1]) + role[1:]
}

func docGrantsRoom(workspaceID, docID string) string {
	return fmt.Sprintf("workspace:%s:doc:%s:grants", workspaceID, docID)
}

func marshalDocGrantsInput(workspaceID, docID string) ([]byte, error) {
	return json.Marshal(docGrantsInputKey{WorkspaceID: workspaceID, DocID: docID})
}

func docGrantsError(err error) error {
	switch {
	case errors.Is(err, errInvalidDocGrants):
		return syncError("BAD_REQUEST", "Invalid document grants request.")
	case errors.Is(err, errDocGrantsUnavailable):
		return syncError("INTERNAL_SERVER_ERROR", "Document grants realtime is unavailable.")
	case errors.Is(err, errDocGrantsDenied), errors.Is(err, workspaces.ErrAccessDenied):
		return syncError("DOC_ACTION_DENIED", "Document grant read access is denied.")
	case errors.Is(err, workspaces.ErrNotFound):
		return syncError("DOC_NOT_FOUND", "Document not found.")
	default:
		return syncError("INTERNAL_SERVER_ERROR", "The request could not be completed.")
	}
}

func docGrantsSubscriptionInput(value any) (string, string, bool) {
	input, ok := value.(map[string]any)
	if !ok || len(input) != 2 {
		return "", "", false
	}
	workspaceID, workspaceOK := input["workspaceId"].(string)
	docID, docOK := input["docId"].(string)
	return workspaceID, docID, workspaceOK && docOK && strings.TrimSpace(workspaceID) != "" && strings.TrimSpace(docID) != ""
}
