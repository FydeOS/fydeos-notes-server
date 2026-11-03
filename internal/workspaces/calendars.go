// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package workspaces

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func readCalendarItems(ctx context.Context, q queryer, calendarID string) ([]WorkspaceCalendarItem, error) {
	rows, err := q.Query(ctx, `SELECT id, subscription_id, sort_order, color_override, enabled FROM workspace_calendar_items
		WHERE workspace_calendar_id=$1 ORDER BY sort_order NULLS LAST, created_at, id`, calendarID)
	if err != nil {
		return nil, err
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (WorkspaceCalendarItem, error) {
		var it WorkspaceCalendarItem
		err := row.Scan(&it.ID, &it.SubscriptionID, &it.SortOrder, &it.ColorOverride, &it.Enabled)
		return it, err
	})
	if items == nil && err == nil {
		items = []WorkspaceCalendarItem{}
	}
	return items, err
}

func scanCalendars(ctx context.Context, q queryer, sql string, args ...any) ([]WorkspaceCalendar, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	cals, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (WorkspaceCalendar, error) {
		var c WorkspaceCalendar
		err := row.Scan(&c.ID, &c.WorkspaceID, &c.CreatedByUserID, &c.DisplayNameOverride, &c.ColorOverride, &c.Enabled)
		return c, err
	})
	if err != nil {
		return nil, err
	}
	for i := range cals {
		if cals[i].Items, err = readCalendarItems(ctx, q, cals[i].ID); err != nil {
			return nil, err
		}
	}
	if cals == nil {
		cals = []WorkspaceCalendar{}
	}
	return cals, nil
}

const calendarColumns = `SELECT id, workspace_id, created_by_user_id, display_name_override, color_override, enabled FROM workspace_calendars`

func (r *Repository) ListWorkspaceCalendars(ctx context.Context, workspaceID string) ([]WorkspaceCalendar, error) {
	return scanCalendars(ctx, r.db, calendarColumns+` WHERE workspace_id=$1 ORDER BY created_at, id`, workspaceID)
}

// UpdateWorkspaceCalendarItems replaces the items of the workspace calendar.
func (r *Repository) UpdateWorkspaceCalendarItems(ctx context.Context, workspaceID, actor string, inputs []WorkspaceCalendarItemInput) (WorkspaceCalendar, error) {
	var out WorkspaceCalendar
	err := r.inTx(ctx, nil, func(tx pgx.Tx) error {
		ok, err := workspaceExists(ctx, tx, workspaceID)
		if err != nil {
			return err
		}
		if !ok {
			return ErrNotFound
		}
		var calID string
		err = tx.QueryRow(ctx, `SELECT id FROM workspace_calendars WHERE workspace_id=$1 ORDER BY created_at, id LIMIT 1 FOR UPDATE`, workspaceID).Scan(&calID)
		if noRows(err) {
			calID = uuid.NewString()
			_, err = tx.Exec(ctx, `INSERT INTO workspace_calendars(id, workspace_id, created_by_user_id, enabled, updated_at) VALUES ($1,$2,$3,true,CURRENT_TIMESTAMP)`, calID, workspaceID, actor)
		}
		if err != nil {
			return fmt.Errorf("prepare workspace calendar: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM workspace_calendar_items WHERE workspace_calendar_id=$1`, calID); err != nil {
			return err
		}
		for i, in := range inputs {
			order := i
			if in.SortOrder != nil {
				order = *in.SortOrder
			}
			if _, err := tx.Exec(ctx, `INSERT INTO workspace_calendar_items(id, workspace_calendar_id, subscription_id, sort_order, color_override, enabled, updated_at)
				VALUES ($1,$2,$3,$4,$5,true,CURRENT_TIMESTAMP)`, uuid.NewString(), calID, in.SubscriptionID, order, in.ColorOverride); err != nil {
				return fmt.Errorf("insert calendar item: %w", err)
			}
		}
		cals, err := scanCalendars(ctx, tx, calendarColumns+` WHERE id=$1`, calID)
		if err != nil {
			return err
		}
		out = cals[0]
		return nil
	})
	return out, err
}

// ListWorkspaceCalendarEvents lists events overlapping [from, to).
func (r *Repository) ListWorkspaceCalendarEvents(ctx context.Context, calendarID string, from, to time.Time) ([]CalendarEvent, error) {
	rows, err := r.db.Query(ctx, `
		SELECT e.id, e.subscription_id, e.external_event_id, e.recurrence_id, e.status, e.title, e.description, e.location, e.original_timezone,
		       e.start_at_utc, e.end_at_utc, e.all_day
		FROM calendar_events e
		WHERE e.subscription_id IN (SELECT subscription_id FROM workspace_calendar_items WHERE workspace_calendar_id=$1)
		  AND e.start_at_utc < $3 AND e.end_at_utc > $2
		ORDER BY e.start_at_utc, e.end_at_utc, e.id`, calendarID, from, to)
	if err != nil {
		return nil, err
	}
	events, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (CalendarEvent, error) {
		var e CalendarEvent
		err := row.Scan(&e.ID, &e.SubscriptionID, &e.ExternalEventID, &e.RecurrenceID, &e.Status, &e.Title, &e.Description, &e.Location,
			&e.OriginalTimezone, &e.StartAtUTC, &e.EndAtUTC, &e.AllDay)
		return e, err
	})
	if events == nil && err == nil {
		events = []CalendarEvent{}
	}
	return events, err
}

const accountColumns = `SELECT a.id, a.provider, a.provider_account_id, a.display_name, a.email, a.last_error, a.status, a.refresh_interval_minutes,
	a.created_at, a.updated_at, (SELECT count(*) FROM calendar_subscriptions s WHERE s.account_id=a.id)
	FROM calendar_accounts a`

func scanAccount(row pgx.Row) (CalendarAccount, error) {
	var a CalendarAccount
	err := row.Scan(&a.ID, &a.Provider, &a.ProviderAccountID, &a.DisplayName, &a.Email, &a.LastError, &a.Status, &a.RefreshIntervalMinutes,
		&a.CreatedAt, &a.UpdatedAt, &a.CalendarsCount)
	return a, err
}

func (r *Repository) ListCalendarAccounts(ctx context.Context, userID string) ([]CalendarAccount, error) {
	rows, err := r.db.Query(ctx, accountColumns+` WHERE a.user_id=$1 ORDER BY a.created_at DESC, a.id`, userID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (CalendarAccount, error) { return scanAccount(row) })
	if out == nil && err == nil {
		out = []CalendarAccount{}
	}
	return out, err
}

func (r *Repository) UpdateCalendarAccountRefreshInterval(ctx context.Context, userID, accountID string, minutes int) (*CalendarAccount, error) {
	if minutes < 1 {
		return nil, ErrInvalidCalendarRefreshInterval
	}
	var out *CalendarAccount
	err := r.inTx(ctx, nil, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE calendar_accounts SET refresh_interval_minutes=$3, updated_at=CURRENT_TIMESTAMP WHERE id=$1 AND user_id=$2`, accountID, userID, minutes)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE calendar_subscriptions SET next_sync_at=COALESCE(last_sync_at, CURRENT_TIMESTAMP) + make_interval(mins => $2), updated_at=CURRENT_TIMESTAMP
			WHERE account_id=$1`, accountID, minutes); err != nil {
			return err
		}
		a, err := scanAccount(tx.QueryRow(ctx, accountColumns+` WHERE a.id=$1`, accountID))
		if err != nil {
			return err
		}
		out = &a
		return nil
	})
	return out, err
}

func (r *Repository) UnlinkCalendarAccount(ctx context.Context, userID, accountID string) (bool, error) {
	tag, err := r.db.Exec(ctx, `DELETE FROM calendar_accounts WHERE id=$1 AND user_id=$2`, accountID, userID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func (r *Repository) ListCalendarSubscriptions(ctx context.Context, userID, accountID string) ([]CalendarSubscription, error) {
	rows, err := r.db.Query(ctx, `
		SELECT s.id, s.account_id, s.provider, s.external_calendar_id, s.display_name, s.timezone, s.color, s.enabled, s.last_sync_at
		FROM calendar_subscriptions s JOIN calendar_accounts a ON a.id=s.account_id
		WHERE a.id=$1 AND a.user_id=$2 ORDER BY s.created_at, s.id`, accountID, userID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (CalendarSubscription, error) {
		var s CalendarSubscription
		err := row.Scan(&s.ID, &s.AccountID, &s.Provider, &s.ExternalCalendarID, &s.DisplayName, &s.Timezone, &s.Color, &s.Enabled, &s.LastSyncAt)
		return s, err
	})
	if out == nil && err == nil {
		out = []CalendarSubscription{}
	}
	return out, err
}
