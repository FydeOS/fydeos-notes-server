// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"fmt"
	"time"

	gqlgraphql "github.com/99designs/gqlgen/graphql"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/graphql/model"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

type AdminDashboardReader interface {
	GetAdminDashboard(context.Context, workspaces.AdminDashboardInput) (workspaces.AdminDashboard, error)
}

func (r *Resolver) adminDashboard(ctx context.Context, input *model.AdminDashboardInput) (*model.AdminDashboard, error) {
	if err := requireCloudAdministrator(ctx, r.Config); err != nil {
		return nil, err
	}
	reader, ok := r.DocumentAnalytics.(AdminDashboardReader)
	if !ok || reader == nil {
		return nil, adminWorkspaceInternalError()
	}
	options := workspaces.AdminDashboardInput{}
	if input != nil {
		options = workspaces.AdminDashboardInput{
			Timezone: input.Timezone, StorageHistoryDays: input.StorageHistoryDays,
			SyncHistoryHours: input.SyncHistoryHours, SharedLinkWindowDays: input.SharedLinkWindowDays,
			CopilotWindowDays: input.CopilotWindowDays,
		}
	}
	for _, field := range gqlgraphql.CollectAllFields(ctx) {
		if field == "topSharedLinks" {
			options.IncludeTopSharedLinks = true
			break
		}
	}
	dashboard, err := reader.GetAdminDashboard(ctx, options)
	if err != nil {
		return nil, adminWorkspaceInternalError()
	}
	result := &model.AdminDashboard{
		BlobStorageBytes:        dashboard.BlobStorageBytes,
		CopilotConversations:    dashboard.CopilotConversations,
		GeneratedAt:             dashboard.GeneratedAt.UTC().Format(time.RFC3339Nano),
		SyncActiveUsers:         dashboard.SyncActiveUsers,
		WorkspaceStorageBytes:   dashboard.WorkspaceStorageBytes,
		TopSharedLinks:          make([]*model.AdminSharedLinkTopItem, 0, len(dashboard.TopSharedLinks)),
		SyncActiveUsersTimeline: make([]*model.AdminDashboardMinutePoint, 0, len(dashboard.SyncActiveUsersTimeline)),
		WorkspaceStorageHistory: make([]*model.AdminDashboardValueDayPoint, 0, len(dashboard.WorkspaceStorageHistory)),
		BlobStorageHistory:      make([]*model.AdminDashboardValueDayPoint, 0, len(dashboard.BlobStorageHistory)),
	}
	result.SyncWindow = adminDashboardWindowModel(dashboard.SyncWindow)
	result.CopilotWindow = adminDashboardWindowModel(dashboard.CopilotWindow)
	result.StorageWindow = adminDashboardWindowModel(dashboard.StorageWindow)
	result.TopSharedLinksWindow = adminDashboardWindowModel(dashboard.TopSharedLinksWindow)
	for _, point := range dashboard.SyncActiveUsersTimeline {
		result.SyncActiveUsersTimeline = append(result.SyncActiveUsersTimeline, &model.AdminDashboardMinutePoint{
			Minute: point.Minute.UTC().Format(time.RFC3339Nano), ActiveUsers: point.ActiveUsers,
		})
	}
	for _, point := range dashboard.WorkspaceStorageHistory {
		result.WorkspaceStorageHistory = append(result.WorkspaceStorageHistory, &model.AdminDashboardValueDayPoint{
			Date: point.Date.UTC().Format(time.RFC3339Nano), Value: point.Value,
		})
	}
	for _, point := range dashboard.BlobStorageHistory {
		result.BlobStorageHistory = append(result.BlobStorageHistory, &model.AdminDashboardValueDayPoint{
			Date: point.Date.UTC().Format(time.RFC3339Nano), Value: point.Value,
		})
	}
	baseURL := config.PublicOrigin(r.Config)
	for _, link := range dashboard.TopSharedLinks {
		result.TopSharedLinks = append(result.TopSharedLinks, &model.AdminSharedLinkTopItem{
			WorkspaceID: link.WorkspaceID, DocID: link.DocID, Title: link.Title,
			ShareURL:    fmt.Sprintf("%s/workspace/%s/%s", baseURL, link.WorkspaceID, link.DocID),
			PublishedAt: createdAt(link.PublishedAt), Views: link.Views, UniqueViews: link.UniqueViews,
			GuestViews: link.GuestViews, LastAccessedAt: createdAt(link.LastAccessedAt),
		})
	}
	return result, nil
}

func adminDashboardWindowModel(window workspaces.AdminDashboardTimeWindow) *model.TimeWindow {
	return &model.TimeWindow{
		From:     window.From.UTC().Format(time.RFC3339Nano),
		To:       window.To.UTC().Format(time.RFC3339Nano),
		Timezone: window.Timezone, Bucket: model.TimeBucket(window.Bucket),
		RequestedSize: window.RequestedSize, EffectiveSize: window.EffectiveSize,
	}
}
