// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"errors"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/calendar"
	"github.com/fydeos/fydeos-notes-server/internal/graphql/model"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
	"github.com/vektah/gqlparser/v2/gqlerror"
)

type workspaceCalendarStore interface {
	ListCalendarAccounts(context.Context, string) ([]workspaces.CalendarAccount, error)
	ListCalendarSubscriptions(context.Context, string, string) ([]workspaces.CalendarSubscription, error)
	UpdateCalendarAccountRefreshInterval(context.Context, string, string, int) (*workspaces.CalendarAccount, error)
	UnlinkCalendarAccount(context.Context, string, string) (bool, error)
	ListWorkspaceCalendars(context.Context, string) ([]workspaces.WorkspaceCalendar, error)
	UpdateWorkspaceCalendarItems(context.Context, string, string, []workspaces.WorkspaceCalendarItemInput) (workspaces.WorkspaceCalendar, error)
	ListWorkspaceCalendarEvents(context.Context, string, time.Time, time.Time) ([]workspaces.CalendarEvent, error)
}

func (r *Resolver) linkCalendarAccount(ctx context.Context, input model.LinkCalendarAccountInput) (string, error) {
	principal := auth.PrincipalFromContext(ctx)
	if principal == nil {
		return "", ErrAuthenticationRequired
	}
	if input.Provider != model.CalendarProviderTypeGoogle {
		return "", &gqlerror.Error{Message: "Selected calendar provider does not support OAuth.", Extensions: map[string]any{"code": "calendar_provider_oauth_unsupported"}}
	}
	if r.GoogleCalendarAccountLinker == nil {
		return "", errors.New("Google Calendar OAuth is unavailable")
	}
	return r.GoogleCalendarAccountLinker.CreateAuthorizationURL(ctx, principal.User.ID, input.RedirectURI)
}

func (r *Resolver) availableCalendarProviders(ctx context.Context) ([]model.CalendarProviderType, error) {
	if r.CalendarProviderCatalog == nil {
		return []model.CalendarProviderType{}, nil
	}
	providers, err := r.CalendarProviderCatalog.AvailableCalendarProviders(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]model.CalendarProviderType, 0, len(providers))
	for _, provider := range providers {
		result = append(result, model.CalendarProviderType(provider))
	}
	return result, nil
}

func (r *Resolver) linkCalDAVAccount(ctx context.Context, input model.LinkCalDAVAccountInput) (*model.CalendarAccountObjectType, error) {
	principal := auth.PrincipalFromContext(ctx)
	if principal == nil {
		return nil, ErrAuthenticationRequired
	}
	if r.CalDAVAccountLinker == nil {
		return nil, errors.New("CalDAV account linker is unavailable")
	}
	linked, err := r.CalDAVAccountLinker.LinkCalDAVAccount(ctx, principal.User.ID, calendar.LinkInput{
		ProviderPresetID: input.ProviderPresetID,
		Username:         input.Username,
		Password:         input.Password,
		DisplayName:      input.DisplayName,
	})
	if err != nil {
		var providerError *calendar.Error
		if errors.As(err, &providerError) {
			return nil, &gqlerror.Error{Message: providerError.Message, Extensions: map[string]any{"code": providerError.Code}}
		}
		return nil, err
	}
	store, ok := r.WorkspaceRepo.(workspaceCalendarStore)
	if !ok || store == nil {
		return nil, errors.New("calendar account store is unavailable")
	}
	accounts, err := store.ListCalendarAccounts(ctx, principal.User.ID)
	if err != nil {
		return nil, err
	}
	for _, account := range accounts {
		if account.ID == linked.ID {
			return calendarAccountModel(account), nil
		}
	}
	return nil, errors.New("linked calendar account was not found")
}

func (r *Resolver) userCalendarAccounts(ctx context.Context, user *model.UserType) ([]*model.CalendarAccountObjectType, error) {
	principal := auth.PrincipalFromContext(ctx)
	if principal == nil {
		return nil, ErrAuthenticationRequired
	}
	if principal.User.ID != user.ID {
		return nil, auth.ErrActionForbidden
	}
	store, ok := r.WorkspaceRepo.(workspaceCalendarStore)
	if !ok || store == nil {
		return nil, errors.New("calendar account reader is unavailable")
	}
	accounts, err := store.ListCalendarAccounts(ctx, user.ID)
	if err != nil {
		return nil, err
	}
	result := make([]*model.CalendarAccountObjectType, 0, len(accounts))
	for _, account := range accounts {
		result = append(result, calendarAccountModel(account))
	}
	return result, nil
}

func (r *Resolver) updateCalendarAccount(ctx context.Context, accountID string, interval int) (*model.CalendarAccountObjectType, error) {
	principal := auth.PrincipalFromContext(ctx)
	if principal == nil {
		return nil, ErrAuthenticationRequired
	}
	store, ok := r.WorkspaceRepo.(workspaceCalendarStore)
	if !ok || store == nil {
		return nil, errors.New("calendar account store is unavailable")
	}
	account, err := store.UpdateCalendarAccountRefreshInterval(ctx, principal.User.ID, accountID, interval)
	if errors.Is(err, workspaces.ErrInvalidCalendarRefreshInterval) {
		return nil, documentListBadRequest("Invalid calendar refresh interval")
	}
	if err != nil || account == nil {
		return nil, err
	}
	return calendarAccountModel(*account), nil
}

func (r *Resolver) unlinkCalendarAccount(ctx context.Context, accountID string) (bool, error) {
	principal := auth.PrincipalFromContext(ctx)
	if principal == nil {
		return false, ErrAuthenticationRequired
	}
	store, ok := r.WorkspaceRepo.(workspaceCalendarStore)
	if !ok || store == nil {
		return false, errors.New("calendar account store is unavailable")
	}
	return store.UnlinkCalendarAccount(ctx, principal.User.ID, accountID)
}

func calendarAccountModel(account workspaces.CalendarAccount) *model.CalendarAccountObjectType {
	return &model.CalendarAccountObjectType{
		ID: account.ID, Provider: model.CalendarProviderType(account.Provider), ProviderAccountID: account.ProviderAccountID,
		DisplayName: account.DisplayName, Email: account.Email, Status: account.Status, LastError: account.LastError,
		RefreshIntervalMinutes: account.RefreshIntervalMinutes, CalendarsCount: account.CalendarsCount,
		CreatedAt: account.CreatedAt.UTC().Format(time.RFC3339Nano), UpdatedAt: account.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
}

func (r *Resolver) calendarAccountSubscriptions(ctx context.Context, account *model.CalendarAccountObjectType) ([]*model.CalendarSubscriptionObjectType, error) {
	principal := auth.PrincipalFromContext(ctx)
	if principal == nil {
		return nil, ErrAuthenticationRequired
	}
	store, ok := r.WorkspaceRepo.(workspaceCalendarStore)
	if !ok || store == nil {
		return nil, errors.New("calendar account reader is unavailable")
	}
	items, err := store.ListCalendarSubscriptions(ctx, principal.User.ID, account.ID)
	if err != nil {
		return nil, err
	}
	result := make([]*model.CalendarSubscriptionObjectType, 0, len(items))
	for _, item := range items {
		var lastSyncAt *string
		if item.LastSyncAt != nil {
			value := item.LastSyncAt.UTC().Format(time.RFC3339Nano)
			lastSyncAt = &value
		}
		result = append(result, &model.CalendarSubscriptionObjectType{
			ID: item.ID, AccountID: item.AccountID, Provider: model.CalendarProviderType(item.Provider),
			ExternalCalendarID: item.ExternalCalendarID, DisplayName: item.DisplayName, Timezone: item.Timezone,
			Color: item.Color, Enabled: item.Enabled, LastSyncAt: lastSyncAt,
		})
	}
	return result, nil
}

func (r *Resolver) workspaceCalendars(ctx context.Context, workspaceID string) ([]*model.WorkspaceCalendarObjectType, error) {
	principal := auth.PrincipalFromContext(ctx)
	if principal == nil {
		return nil, ErrAuthenticationRequired
	}
	if r.WorkspacePermissionReader == nil {
		return nil, documentWorkspaceReadError(workspaces.ErrAuthorizationUnavailable, workspaceID)
	}
	access, err := r.WorkspacePermissionReader.GetWorkspaceRolePermissions(ctx, principal.User.ID, workspaceID)
	if errors.Is(err, workspaces.ErrAccessDenied) || errors.Is(err, workspaces.ErrNotFound) || err == nil && !access.Permissions["Workspace_Settings_Read"] {
		return nil, &WorkspacePermissionsDeniedError{WorkspaceID: workspaceID}
	}
	if err != nil {
		return nil, err
	}
	store, ok := r.WorkspaceRepo.(workspaceCalendarStore)
	if !ok || store == nil {
		return nil, documentWorkspaceReadError(workspaces.ErrAuthorizationUnavailable, workspaceID)
	}
	calendars, err := store.ListWorkspaceCalendars(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	result := make([]*model.WorkspaceCalendarObjectType, 0, len(calendars))
	for _, calendar := range calendars {
		result = append(result, workspaceCalendarModel(calendar))
	}
	return result, nil
}

func (r *Resolver) updateWorkspaceCalendars(ctx context.Context, input model.UpdateWorkspaceCalendarsInput) (*model.WorkspaceCalendarObjectType, error) {
	principal := auth.PrincipalFromContext(ctx)
	if principal == nil {
		return nil, ErrAuthenticationRequired
	}
	if r.WorkspacePermissionReader == nil {
		return nil, documentWorkspaceReadError(workspaces.ErrAuthorizationUnavailable, input.WorkspaceID)
	}
	access, err := r.WorkspacePermissionReader.GetWorkspaceRolePermissions(ctx, principal.User.ID, input.WorkspaceID)
	if errors.Is(err, workspaces.ErrAccessDenied) || errors.Is(err, workspaces.ErrNotFound) || err == nil && !access.Permissions["Workspace_Settings_Update"] {
		return nil, &WorkspacePermissionsDeniedError{WorkspaceID: input.WorkspaceID}
	}
	if err != nil {
		return nil, err
	}
	store, ok := r.WorkspaceRepo.(workspaceCalendarStore)
	if !ok || store == nil {
		return nil, documentWorkspaceReadError(workspaces.ErrAuthorizationUnavailable, input.WorkspaceID)
	}
	items := make([]workspaces.WorkspaceCalendarItemInput, 0, len(input.Items))
	for _, item := range input.Items {
		if item == nil || item.SubscriptionID == "" {
			return nil, documentListBadRequest("Invalid calendar subscription")
		}
		items = append(items, workspaces.WorkspaceCalendarItemInput{SubscriptionID: item.SubscriptionID, SortOrder: item.SortOrder, ColorOverride: item.ColorOverride})
	}
	calendar, err := store.UpdateWorkspaceCalendarItems(ctx, input.WorkspaceID, principal.User.ID, items)
	if errors.Is(err, workspaces.ErrNotFound) {
		return nil, documentWorkspaceReadError(err, input.WorkspaceID)
	}
	if err != nil {
		return nil, err
	}
	return workspaceCalendarModel(calendar), nil
}

func (r *Resolver) workspaceCalendarEvents(ctx context.Context, calendar *model.WorkspaceCalendarObjectType, from, to string) ([]*model.CalendarEventObjectType, error) {
	principal := auth.PrincipalFromContext(ctx)
	if principal == nil {
		return nil, ErrAuthenticationRequired
	}
	if r.WorkspacePermissionReader == nil {
		return nil, documentWorkspaceReadError(workspaces.ErrAuthorizationUnavailable, calendar.WorkspaceID)
	}
	access, err := r.WorkspacePermissionReader.GetWorkspaceRolePermissions(ctx, principal.User.ID, calendar.WorkspaceID)
	if errors.Is(err, workspaces.ErrAccessDenied) || errors.Is(err, workspaces.ErrNotFound) || err == nil && !access.Permissions["Workspace_Settings_Read"] {
		return nil, &WorkspacePermissionsDeniedError{WorkspaceID: calendar.WorkspaceID}
	}
	if err != nil {
		return nil, err
	}
	start, err := time.Parse(time.RFC3339Nano, from)
	if err != nil {
		return nil, documentListBadRequest("Invalid calendar event range")
	}
	end, err := time.Parse(time.RFC3339Nano, to)
	if err != nil || !end.After(start) {
		return nil, documentListBadRequest("Invalid calendar event range")
	}
	store, ok := r.WorkspaceRepo.(workspaceCalendarStore)
	if !ok || store == nil {
		return nil, documentWorkspaceReadError(workspaces.ErrAuthorizationUnavailable, calendar.WorkspaceID)
	}
	events, err := store.ListWorkspaceCalendarEvents(ctx, calendar.ID, start, end)
	if err != nil {
		return nil, err
	}
	result := make([]*model.CalendarEventObjectType, 0, len(events))
	for _, event := range events {
		result = append(result, &model.CalendarEventObjectType{
			ID: event.ID, SubscriptionID: event.SubscriptionID, ExternalEventID: event.ExternalEventID,
			RecurrenceID: event.RecurrenceID, Status: event.Status, Title: event.Title, Description: event.Description,
			Location: event.Location, StartAtUtc: event.StartAtUTC.UTC().Format(time.RFC3339Nano), EndAtUtc: event.EndAtUTC.UTC().Format(time.RFC3339Nano),
			OriginalTimezone: event.OriginalTimezone, AllDay: event.AllDay,
		})
	}
	return result, nil
}

func workspaceCalendarModel(calendar workspaces.WorkspaceCalendar) *model.WorkspaceCalendarObjectType {
	result := &model.WorkspaceCalendarObjectType{
		ID: calendar.ID, WorkspaceID: calendar.WorkspaceID, CreatedByUserID: calendar.CreatedByUserID,
		DisplayNameOverride: calendar.DisplayNameOverride, ColorOverride: calendar.ColorOverride, Enabled: calendar.Enabled,
		Items: make([]*model.WorkspaceCalendarItemObjectType, 0, len(calendar.Items)),
	}
	for _, item := range calendar.Items {
		result.Items = append(result.Items, &model.WorkspaceCalendarItemObjectType{ID: item.ID, SubscriptionID: item.SubscriptionID, SortOrder: item.SortOrder, ColorOverride: item.ColorOverride, Enabled: item.Enabled})
	}
	return result
}
