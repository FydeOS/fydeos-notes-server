// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package worker

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

const (
	cleanupBatchSize  = 1000
	cleanupMaxBatches = 100
)

type StorageReservationCleaner interface {
	CleanupExpiredStorageReservationsV1(context.Context, int64) (int64, error)
}

func CleanupExpiredStorageReservations(ctx context.Context, cleaner StorageReservationCleaner) (int64, error) {
	if cleaner == nil {
		return 0, errors.New("storage reservation cleaner is required")
	}
	var total int64
	for batch := 0; batch < cleanupMaxBatches; batch++ {
		count, err := cleaner.CleanupExpiredStorageReservationsV1(ctx, cleanupBatchSize)
		if err != nil {
			return total, err
		}
		if count < 0 || count > cleanupBatchSize {
			return total, errors.New("storage reservation cleanup returned an invalid batch count")
		}
		total += count
		if count < cleanupBatchSize {
			return total, nil
		}
	}
	return total, nil
}

func NextLocalMidnight(now time.Time) time.Time {
	year, month, day := now.Date()
	return time.Date(year, month, day+1, 0, 0, 0, 0, now.Location())
}

func RunDailyBlobCleanup(ctx context.Context, cleaner StorageReservationCleaner) error {
	if cleaner == nil {
		return errors.New("storage reservation cleaner is required")
	}
	for {
		timer := time.NewTimer(time.Until(NextLocalMidnight(time.Now())))
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return nil
		case <-timer.C:
			count, err := CleanupExpiredStorageReservations(ctx, cleaner)
			if err != nil {
				slog.ErrorContext(ctx, "expired storage reservation cleanup failed", "deleted", count, "error", err)
				continue
			}
			slog.InfoContext(ctx, "expired storage reservations cleaned", "deleted", count)
		}
	}
}

func RunDailyMaintenance(ctx context.Context, storageCleaner StorageReservationCleaner, notificationCleaner NotificationCleaner, counts NotificationCountReader, publisher NotificationCountPublisher, quotaCleaner RollingQuotaCleaner) error {
	if storageCleaner == nil {
		return errors.New("storage reservation cleaner is required")
	}
	if notificationCleaner == nil {
		return errors.New("notification cleaner is required")
	}
	if counts == nil {
		return errors.New("notification count reader is required")
	}
	if publisher == nil {
		return errors.New("notification count publisher is required")
	}
	if quotaCleaner == nil {
		return errors.New("rolling quota cleaner is required")
	}

	maintenanceCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errorsFound := make(chan error, 3)
	go func() { errorsFound <- RunDailyBlobCleanup(maintenanceCtx, storageCleaner) }()
	go func() {
		errorsFound <- RunDailyNotificationCleanup(maintenanceCtx, notificationCleaner, counts, publisher)
	}()
	go func() { errorsFound <- RunRollingQuotaCleanup(maintenanceCtx, quotaCleaner) }()
	first := <-errorsFound
	cancel()
	second := <-errorsFound
	if first != nil {
		return first
	}
	return second
}
