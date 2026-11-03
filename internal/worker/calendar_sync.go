// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package worker

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

type CalendarSyncer interface {
	SyncDue(context.Context) error
}

func RunCalendarSync(ctx context.Context, syncer CalendarSyncer) error {
	if syncer == nil {
		return errors.New("calendar syncer is required")
	}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return nil
		}
		if err := syncer.SyncDue(ctx); err != nil {
			slog.ErrorContext(ctx, "calendar sync batch failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

type CalendarSyncGroup []CalendarSyncer

func (group CalendarSyncGroup) SyncDue(ctx context.Context) error {
	var failures []error
	for _, syncer := range group {
		if syncer == nil {
			continue
		}
		if err := syncer.SyncDue(ctx); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
