// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"errors"
	"net/http"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/graphql/model"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
	"github.com/vektah/gqlparser/v2/gqlerror"
)

func (r *Resolver) documentAnalyticsPermission(ctx context.Context, doc *model.DocType, action string) (string, error) {
	principal := auth.PrincipalFromContext(ctx)
	if principal == nil {
		return "", ErrAuthenticationRequired
	}
	if r.DocumentReader == nil {
		return "", documentReadError(workspaces.ErrAuthorizationUnavailable, doc.WorkspaceID, doc.ID, action)
	}
	permissions, err := r.DocumentReader.ReadDocumentPermissions(ctx, principal.User.ID, doc.WorkspaceID, doc.ID)
	if err != nil {
		return "", documentReadError(err, doc.WorkspaceID, doc.ID, action)
	}
	if !permissions[action] {
		return "", documentReadError(workspaces.ErrDocRoleDenied, doc.WorkspaceID, doc.ID, action)
	}
	if r.DocumentAnalytics == nil {
		return "", documentReadError(workspaces.ErrAuthorizationUnavailable, doc.WorkspaceID, doc.ID, action)
	}
	return principal.User.ID, nil
}

func documentAnalyticsError(err error, workspace, doc string) error {
	switch {
	case errors.Is(err, workspaces.ErrInvalidDocAnalyticsCursor):
		message := "Invalid pagination cursor"
		return &gqlerror.Error{Message: message, Extensions: userFriendlyGraphQLExtensions(http.StatusBadRequest, "BAD_REQUEST", "BAD_REQUEST", message, nil)}
	case errors.Is(err, workspaces.ErrDocAnalyticsQueryTooLong):
		message := "Query is too long, max length is 255"
		return &gqlerror.Error{Message: message, Extensions: userFriendlyGraphQLExtensions(http.StatusBadRequest, "invalid_input", "query_too_long", message, map[string]int{"max": 255})}
	default:
		return documentReadError(err, workspace, doc, "Doc.Analytics.Read")
	}
}

func (r *Resolver) documentPageAnalytics(ctx context.Context, doc *model.DocType, input *model.DocPageAnalyticsInput) (*model.DocPageAnalytics, error) {
	actor, err := r.documentAnalyticsPermission(ctx, doc, "Doc.Analytics.Read")
	if err != nil {
		return nil, err
	}
	options := workspaces.DocPageAnalyticsInput{}
	if input != nil {
		options.WindowDays = input.WindowDays
		options.Timezone = input.Timezone
	}
	result, err := r.DocumentAnalytics.GetDocPageAnalytics(ctx, actor, doc.WorkspaceID, doc.ID, options)
	if err != nil {
		return nil, documentAnalyticsError(err, doc.WorkspaceID, doc.ID)
	}
	series := make([]*model.DocPageAnalyticsPoint, 0, len(result.Series))
	for _, point := range result.Series {
		date := createdAt(&point.Date)
		series = append(series, &model.DocPageAnalyticsPoint{Date: *date, TotalViews: point.TotalViews, UniqueViews: point.UniqueViews, GuestViews: point.GuestViews})
	}
	windowFrom, windowTo, generatedAt := createdAt(&result.Window.From), createdAt(&result.Window.To), createdAt(&result.GeneratedAt)
	var lastAccessedAt *string
	if result.Summary.LastAccessedAt != nil {
		lastAccessedAt = createdAt(result.Summary.LastAccessedAt)
	}
	return &model.DocPageAnalytics{
		Window: &model.TimeWindow{
			From: *windowFrom, To: *windowTo, Timezone: result.Window.Timezone,
			Bucket: model.TimeBucket(result.Window.Bucket), RequestedSize: result.Window.RequestedSize, EffectiveSize: result.Window.EffectiveSize,
		},
		Series: series,
		Summary: &model.DocPageAnalyticsSummary{
			TotalViews: result.Summary.TotalViews, UniqueViews: result.Summary.UniqueViews,
			GuestViews: result.Summary.GuestViews, LastAccessedAt: lastAccessedAt,
		},
		GeneratedAt: *generatedAt,
	}, nil
}

func (r *Resolver) documentLastAccessedMembers(ctx context.Context, doc *model.DocType, pagination model.PaginationInput, query *string, includeTotal *bool) (*model.PaginatedDocMemberLastAccess, error) {
	actor, err := r.documentAnalyticsPermission(ctx, doc, "Doc.Analytics.Viewers.Read")
	if err != nil {
		return nil, err
	}
	options := workspaces.DocMemberLastAccessPagination{}
	if pagination.First != nil {
		options.First = *pagination.First
	}
	if pagination.Offset != nil {
		options.Offset = *pagination.Offset
	}
	options.After = pagination.After
	withTotal := includeTotal != nil && *includeTotal
	result, err := r.DocumentAnalytics.PaginateDocLastAccessedMembers(ctx, actor, doc.WorkspaceID, doc.ID, options, query, withTotal)
	if err != nil {
		return nil, documentAnalyticsError(err, doc.WorkspaceID, doc.ID)
	}
	edges := make([]*model.DocMemberLastAccessEdge, 0, len(result.Members))
	for index, member := range result.Members {
		cursor := ""
		if index == 0 && result.StartCursor != nil {
			cursor = *result.StartCursor
		} else if index == len(result.Members)-1 && result.EndCursor != nil {
			cursor = *result.EndCursor
		} else {
			cursor, err = workspaces.EncodeDocMemberLastAccessCursor(member)
			if err != nil {
				return nil, documentAnalyticsError(err, doc.WorkspaceID, doc.ID)
			}
		}
		lastAccessedAt := createdAt(&member.LastAccessedAt)
		edges = append(edges, &model.DocMemberLastAccessEdge{
			Cursor: cursor,
			Node: &model.DocMemberLastAccess{
				User:           &model.PublicUserType{ID: member.UserID, Name: member.Name, AvatarURL: member.AvatarURL},
				LastAccessedAt: *lastAccessedAt, LastDocID: member.LastDocID,
			},
		})
	}
	return &model.PaginatedDocMemberLastAccess{
		Edges: edges,
		PageInfo: &model.PageInfo{
			StartCursor: result.StartCursor, EndCursor: result.EndCursor,
			HasNextPage: result.HasNextPage, HasPreviousPage: result.HasPreviousPage,
		},
		TotalCount: result.TotalCount,
	}, nil
}
