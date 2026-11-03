// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/graphql/model"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
	"github.com/vektah/gqlparser/v2/gqlerror"
)

type AdminSharedLinksReader interface {
	PaginateAdminSharedLinks(context.Context, workspaces.AdminSharedLinksInput) (workspaces.AdminSharedLinksPage, error)
}

func (r *Resolver) adminAllSharedLinks(ctx context.Context, filter *model.AdminAllSharedLinksFilterInput, pagination model.PaginationInput) (*model.PaginatedAdminAllSharedLink, error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if r.Config.Deployment != "affine" {
		message := "Not Found"
		return nil, &gqlerror.Error{Message: message, Extensions: userFriendlyGraphQLExtensions(http.StatusNotFound, "resource_not_found", "NOT_FOUND", message, nil)}
	}
	reader, ok := r.DocumentAnalytics.(AdminSharedLinksReader)
	if !ok || reader == nil {
		return nil, adminSharedLinksError(errors.New("admin shared links reader is unavailable"))
	}
	options := workspaces.AdminSharedLinksInput{
		First: pagination.First, Offset: pagination.Offset, After: pagination.After,
		OrderBy: workspaces.AdminSharedLinksUpdatedAtDesc,
	}
	if filter != nil {
		options.Keyword = filter.Keyword
		options.WorkspaceID = filter.WorkspaceID
		options.AnalyticsWindowDays = filter.AnalyticsWindowDays
		options.IncludeTotal = filter.IncludeTotal != nil && *filter.IncludeTotal
		if filter.OrderBy != nil {
			options.OrderBy = workspaces.AdminSharedLinksOrder(*filter.OrderBy)
		}
		if filter.UpdatedAfter != nil {
			parsed, err := time.Parse(time.RFC3339Nano, *filter.UpdatedAfter)
			if err != nil {
				return nil, adminSharedLinksError(workspaces.ErrInvalidAdminSharedLinksCursor)
			}
			options.UpdatedAfter = &parsed
		}
	}
	page, err := reader.PaginateAdminSharedLinks(ctx, options)
	if err != nil {
		return nil, adminSharedLinksError(err)
	}
	result := &model.PaginatedAdminAllSharedLink{
		Edges: make([]*model.AdminAllSharedLinkEdge, 0, len(page.Edges)),
		PageInfo: &model.PageInfo{
			StartCursor: page.StartCursor, EndCursor: page.EndCursor,
			HasNextPage: page.HasNextPage, HasPreviousPage: page.HasPreviousPage,
		},
		TotalCount: page.TotalCount,
		AnalyticsWindow: &model.TimeWindow{
			From: *createdAt(&page.AnalyticsWindow.From), To: *createdAt(&page.AnalyticsWindow.To),
			Timezone: page.AnalyticsWindow.Timezone, Bucket: model.TimeBucket(page.AnalyticsWindow.Bucket),
			RequestedSize: page.AnalyticsWindow.RequestedSize, EffectiveSize: page.AnalyticsWindow.EffectiveSize,
		},
	}
	baseURL := config.PublicOrigin(r.Config)
	for _, edge := range page.Edges {
		node := edge.Node
		shareURL := fmt.Sprintf("%s/workspace/%s/%s", baseURL, node.WorkspaceID, node.DocID)
		var views, uniqueViews, guestViews *int64
		viewsValue, uniqueViewsValue, guestViewsValue := node.Views, node.UniqueViews, node.GuestViews
		views, uniqueViews, guestViews = &viewsValue, &uniqueViewsValue, &guestViewsValue
		result.Edges = append(result.Edges, &model.AdminAllSharedLinkEdge{
			Cursor: edge.Cursor,
			Node: &model.AdminAllSharedLink{
				WorkspaceID: node.WorkspaceID, DocID: node.DocID, Title: node.Title,
				PublishedAt: createdAt(node.PublishedAt), DocUpdatedAt: createdAt(node.DocUpdatedAt),
				WorkspaceOwnerID: node.WorkspaceOwner, LastUpdaterID: node.LastUpdaterID,
				ShareURL: shareURL, Views: views, UniqueViews: uniqueViews, GuestViews: guestViews,
				LastAccessedAt: createdAt(node.LastAccessedAt),
			},
		})
	}
	return result, nil
}

func adminSharedLinksError(err error) error {
	switch {
	case errors.Is(err, workspaces.ErrInvalidAdminSharedLinksCursor):
		message := "Invalid pagination cursor"
		return &gqlerror.Error{Message: message, Extensions: userFriendlyGraphQLExtensions(http.StatusBadRequest, "BAD_REQUEST", "BAD_REQUEST", message, nil)}
	case errors.Is(err, workspaces.ErrAdminSharedLinksQueryTooLong):
		message := "Query is too long, max length is 255"
		return &gqlerror.Error{Message: message, Extensions: userFriendlyGraphQLExtensions(http.StatusBadRequest, "invalid_input", "query_too_long", message, map[string]int{"max": 255})}
	default:
		message := "An internal error occurred."
		return &gqlerror.Error{Message: message, Extensions: userFriendlyGraphQLExtensions(http.StatusInternalServerError, "internal_server_error", "internal_server_error", message, nil)}
	}
}
