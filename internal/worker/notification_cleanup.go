// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package worker

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/notifications"
)

const notificationRetention = 365 * 24 * time.Hour

type NotificationCleaner interface {
	CleanupExpiredNotifications(context.Context, time.Time, int) (notifications.CleanupResult, error)
}

type NotificationCountReader interface {
	CountUnread(context.Context, string) (int, error)
}

type NotificationCountPublisher interface {
	PublishNotificationCountChanged(string, int, string)
}

func CleanupExpiredNotifications(ctx context.Context, cleaner NotificationCleaner, counts NotificationCountReader, publisher NotificationCountPublisher) (int, error) {
	return cleanupExpiredNotificationsAt(ctx, cleaner, counts, publisher, time.Now())
}

func cleanupExpiredNotificationsAt(ctx context.Context, cleaner NotificationCleaner, counts NotificationCountReader, publisher NotificationCountPublisher, now time.Time) (int, error) {
	if cleaner == nil {
		return 0, errors.New("notification cleaner is required")
	}
	if counts == nil {
		return 0, errors.New("notification count reader is required")
	}
	if publisher == nil {
		return 0, errors.New("notification count publisher is required")
	}

	cutoff := now.UTC().Add(-notificationRetention)
	var total int
	for batch := 0; batch < cleanupMaxBatches; batch++ {
		result, err := cleaner.CleanupExpiredNotifications(ctx, cutoff, cleanupBatchSize)
		if err != nil {
			return total, err
		}
		if result.Count < 0 || result.Count > cleanupBatchSize || len(result.UserIDs) > result.Count {
			return total, errors.New("notification cleanup returned an invalid batch result")
		}
		if result.Count > 0 && len(result.UserIDs) == 0 {
			return total, errors.New("notification cleanup omitted affected users")
		}
		total += result.Count

		seen := make(map[string]struct{}, len(result.UserIDs))
		for _, userID := range result.UserIDs {
			if userID == "" {
				return total, errors.New("notification cleanup returned an empty user ID")
			}
			if _, exists := seen[userID]; exists {
				continue
			}
			seen[userID] = struct{}{}
			count, err := counts.CountUnread(ctx, userID)
			if err != nil {
				return total, err
			}
			publisher.PublishNotificationCountChanged(userID, count, "expired-cleanup")
		}
		if result.Count < cleanupBatchSize {
			return total, nil
		}
	}
	return total, nil
}

func RunDailyNotificationCleanup(ctx context.Context, cleaner NotificationCleaner, counts NotificationCountReader, publisher NotificationCountPublisher) error {
	if cleaner == nil {
		return errors.New("notification cleaner is required")
	}
	if counts == nil {
		return errors.New("notification count reader is required")
	}
	if publisher == nil {
		return errors.New("notification count publisher is required")
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
			count, err := CleanupExpiredNotifications(ctx, cleaner, counts, publisher)
			if err != nil {
				slog.ErrorContext(ctx, "expired notification cleanup failed", "deleted", count, "error", err)
				continue
			}
			slog.InfoContext(ctx, "expired notifications cleaned", "deleted", count)
		}
	}
}
