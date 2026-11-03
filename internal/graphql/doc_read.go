// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/graphql/model"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
	"github.com/vektah/gqlparser/v2/gqlerror"
)

type DocumentReader interface {
	ReadWorkspace(context.Context, string, string) (workspaces.Workspace, error)
	ReadDocument(context.Context, string, string, string) (workspaces.DocumentInfo, error)
	ReadPublicDocuments(context.Context, string) ([]workspaces.DocumentInfo, error)
	ReadDocumentAuthors(context.Context, string, string) (workspaces.DocumentAuthors, error)
	ReadDocumentAuthor(context.Context, string) (*workspaces.DocumentAuthor, error)
	ReadDocumentPermissions(context.Context, string, string, string) (map[string]bool, error)
	ReadDocumentGrants(context.Context, string, string, string, int, int, *time.Time) (workspaces.DocGrantPage, error)
}

type DocumentAnalyticsReader interface {
	GetDocPageAnalytics(context.Context, string, string, string, workspaces.DocPageAnalyticsInput) (workspaces.DocPageAnalytics, error)
	PaginateDocLastAccessedMembers(context.Context, string, string, string, workspaces.DocMemberLastAccessPagination, *string, bool) (workspaces.DocMemberLastAccessPage, error)
}

func documentWorkspaceReadError(err error, workspace string) error {
	status, category, name, message := 500, "internal_server_error", "internal_server_error", "An internal error occurred."
	var data any
	switch {
	case errors.Is(err, workspaces.ErrNotFound):
		status, category, name, message = 404, "resource_not_found", "space_not_found", fmt.Sprintf("Space %s not found.", workspace)
		data = map[string]string{"spaceId": workspace}
	case errors.Is(err, workspaces.ErrAccessDenied):
		status, category, name, message = 403, "no_permission", "space_access_denied", fmt.Sprintf("You do not have permission to access Space %s.", workspace)
		data = map[string]string{"spaceId": workspace}
	}
	return &gqlerror.Error{Message: message, Extensions: userFriendlyGraphQLExtensions(status, category, name, message, data)}
}

func documentReadError(err error, workspace, doc, action string) error {
	if err == nil {
		return nil
	}
	status, category, name, message := 500, "internal_server_error", "internal_server_error", "An internal error occurred."
	var data any
	if errors.Is(err, workspaces.ErrDocRoleDenied) {
		status, category, name = 403, "no_permission", "doc_action_denied"
		message = fmt.Sprintf("You do not have permission to perform %s action on doc %s.", action, doc)
		data = map[string]string{"spaceId": workspace, "docId": doc, "action": action}
	} else if errors.Is(err, workspaces.ErrDocumentNotFound) {
		status, category, name = 404, "resource_not_found", "doc_not_found"
		message = fmt.Sprintf("Doc %s under Space %s not found.", doc, workspace)
		data = map[string]string{"spaceId": workspace, "docId": doc}
	}
	return &gqlerror.Error{Message: message, Extensions: userFriendlyGraphQLExtensions(status, category, name, message, data)}
}

func docRoleModel(role string) model.DocRole {
	for _, candidate := range model.AllDocRole {
		if strings.EqualFold(string(candidate), role) {
			return candidate
		}
	}
	return model.DocRoleManager
}

func documentModel(doc workspaces.DocumentInfo) *model.DocType {
	mode := model.PublicDocModePage
	if doc.Mode == 1 {
		mode = model.PublicDocModeEdgeless
	}
	return &model.DocType{ID: doc.DocID, WorkspaceID: doc.WorkspaceID, Mode: mode, Public: doc.Public, DefaultRole: docRoleModel(doc.DefaultRole), Title: doc.Title, Summary: doc.Summary,
		CreatedAt: createdAt(doc.CreatedAt), UpdatedAt: createdAt(doc.UpdatedAt), CreatorID: doc.CreatorID, LastUpdaterID: doc.LastUpdaterID}
}

func (r *Resolver) readDocument(ctx context.Context, workspace, doc string) (*model.DocType, error) {
	principal := auth.PrincipalFromContext(ctx)
	if principal == nil {
		return nil, ErrAuthenticationRequired
	}
	if r.DocumentReader == nil {
		return nil, documentReadError(workspaces.ErrAuthorizationUnavailable, workspace, doc, "")
	}
	result, err := r.DocumentReader.ReadDocument(ctx, principal.User.ID, workspace, doc)
	if err != nil {
		return nil, documentReadError(err, workspace, doc, "Doc.Read")
	}
	return documentModel(result), nil
}

func (r *Resolver) publicDocuments(ctx context.Context, workspace string) ([]*model.DocType, error) {
	if auth.PrincipalFromContext(ctx) == nil {
		return nil, ErrAuthenticationRequired
	}
	if r.DocumentReader == nil {
		return nil, documentReadError(workspaces.ErrAuthorizationUnavailable, workspace, "", "")
	}
	docs, err := r.DocumentReader.ReadPublicDocuments(ctx, workspace)
	if err != nil {
		return nil, documentReadError(err, workspace, "", "")
	}
	result := make([]*model.DocType, 0, len(docs))
	for _, doc := range docs {
		result = append(result, documentModel(doc))
	}
	return result, nil
}

func (r *Resolver) documentAuthor(ctx context.Context, id *string) (*model.PublicUserType, error) {
	if id == nil || *id == "" {
		return nil, nil
	}
	if r.DocumentReader == nil {
		return nil, documentReadError(workspaces.ErrAuthorizationUnavailable, "", "", "")
	}
	user, err := r.DocumentReader.ReadDocumentAuthor(ctx, *id)
	if err != nil {
		return nil, documentReadError(err, "", "", "")
	}
	if user == nil {
		return nil, nil
	}
	return &model.PublicUserType{ID: user.ID, Name: user.Name, AvatarURL: user.AvatarURL}, nil
}

func (r *Resolver) documentMeta(ctx context.Context, workspace, doc string) (*model.WorkspaceDocMeta, error) {
	if r.DocumentReader == nil {
		return nil, documentReadError(workspaces.ErrAuthorizationUnavailable, workspace, doc, "")
	}
	meta, err := r.DocumentReader.ReadDocumentAuthors(ctx, workspace, doc)
	if err != nil {
		return nil, documentReadError(err, workspace, doc, "")
	}
	editor := func(author *workspaces.DocumentAuthor) *model.EditorType {
		if author == nil {
			return nil
		}
		return &model.EditorType{Name: author.Name, AvatarURL: author.AvatarURL}
	}
	return &model.WorkspaceDocMeta{CreatedAt: *createdAt(&meta.CreatedAt), UpdatedAt: *createdAt(&meta.UpdatedAt), CreatedBy: editor(meta.CreatedBy), UpdatedBy: editor(meta.UpdatedBy)}, nil
}

func (r *Resolver) documentPermissions(ctx context.Context, workspace, doc string) (*model.DocPermissions, error) {
	principal := auth.PrincipalFromContext(ctx)
	if principal == nil {
		return nil, ErrAuthenticationRequired
	}
	if r.DocumentReader == nil {
		return nil, documentReadError(workspaces.ErrAuthorizationUnavailable, workspace, doc, "")
	}
	p, err := r.DocumentReader.ReadDocumentPermissions(ctx, principal.User.ID, workspace, doc)
	if err != nil {
		return nil, documentReadError(err, workspace, doc, "")
	}
	return &model.DocPermissions{
		DocRead: p["Doc.Read"], DocCopy: p["Doc.Copy"], DocPropertiesRead: p["Doc.Properties.Read"], DocCommentsRead: p["Doc.Comments.Read"],
		DocUsersRead: p["Doc.Users.Read"], DocDuplicate: p["Doc.Duplicate"], DocHistoryRead: p["Doc.History.Read"], DocCommentsCreate: p["Doc.Comments.Create"],
		DocPropertiesUpdate: p["Doc.Properties.Update"], DocUpdate: p["Doc.Update"], DocAnalyticsRead: p["Doc.Analytics.Read"], DocCommentsModerate: p["Doc.Comments.Moderate"],
		DocPublish: p["Doc.Publish"], DocUnpublish: p["Doc.Unpublish"], DocTrash: p["Doc.Trash"], DocRestore: p["Doc.Restore"], DocDelete: p["Doc.Delete"], DocUsersManage: p["Doc.Users.Manage"],
		DocTransferOwner: p["Doc.TransferOwner"], DocPreview: p["Doc.Preview"], DocAnalyticsViewersRead: p["Doc.Analytics.Viewers.Read"],
	}, nil
}

func (r *Resolver) documentGrants(ctx context.Context, workspace, doc string, input model.PaginationInput) (*model.PaginatedGrantedDocUserType, error) {
	principal := auth.PrincipalFromContext(ctx)
	if principal == nil {
		return nil, ErrAuthenticationRequired
	}
	first, offset := 10, 0
	if input.First != nil {
		first = min(max(*input.First, 1), 100)
	}
	if input.Offset != nil {
		offset = max(*input.Offset, 0)
	}
	var after *time.Time
	if input.After != nil && *input.After != "" {
		// Buffer.from(cursor, 'base64') accepts URL-safe and unpadded cursors,
		// ignores non-alphabet bytes, and stops at the first padding character.
		raw := strings.SplitN(*input.After, "=", 2)[0]
		raw = strings.Map(func(r rune) rune {
			switch {
			case r == '-':
				return '+'
			case r == '_':
				return '/'
			case r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '+' || r == '/':
				return r
			}
			return -1
		}, raw)
		if len(raw)%4 == 1 {
			raw = raw[:len(raw)-1]
		}
		decoded, _ := base64.RawStdEncoding.DecodeString(raw)
		if len(decoded) > 0 {
			if offset > 0 {
				message := "pagination.after and pagination.offset cannot be used together"
				return nil, &gqlerror.Error{Message: message, Extensions: userFriendlyGraphQLExtensions(400, "bad_request", "bad_request", message, nil)}
			}
			value, err := time.Parse(time.RFC3339Nano, string(decoded))
			if err != nil {
				return nil, documentReadError(err, workspace, doc, "")
			}
			after = &value
		}
	}
	if r.DocumentReader == nil {
		return nil, documentReadError(workspaces.ErrAuthorizationUnavailable, workspace, doc, "")
	}
	page, err := r.DocumentReader.ReadDocumentGrants(ctx, principal.User.ID, workspace, doc, first, offset, after)
	if err != nil {
		return nil, documentReadError(err, workspace, doc, "Doc.Users.Read")
	}
	result := &model.PaginatedGrantedDocUserType{TotalCount: page.TotalCount, Edges: make([]*model.GrantedDocUserTypeEdge, 0, len(page.Users)), PageInfo: &model.PageInfo{HasNextPage: page.HasNext, HasPreviousPage: after != nil || offset > 0, EndCursor: page.EndCursor}}
	for _, grant := range page.Users {
		cursor := base64.StdEncoding.EncodeToString([]byte(grant.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z")))
		result.Edges = append(result.Edges, &model.GrantedDocUserTypeEdge{Cursor: cursor, Node: &model.GrantedDocUserType{Role: docRoleModel(grant.Role), User: &model.WorkspaceUserType{ID: grant.UserID, Name: grant.Name, Email: grant.Email, AvatarURL: grant.AvatarURL}}})
	}
	if len(result.Edges) > 0 {
		result.PageInfo.StartCursor = &result.Edges[0].Cursor
	}
	return result, nil
}
