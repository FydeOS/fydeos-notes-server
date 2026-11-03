// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package worker

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

type RollingQuotaCleaner interface {
	CleanupExpiredRollingQuota(context.Context, int64) (int64, error)
}

func CleanupExpiredRollingQuota(ctx context.Context, cleaner RollingQuotaCleaner) (int64, error) {
	if cleaner == nil {
		return 0, errors.New("rolling quota cleaner is required")
	}
	var total int64
	for batch := 0; batch < cleanupMaxBatches; batch++ {
		count, err := cleaner.CleanupExpiredRollingQuota(ctx, cleanupBatchSize)
		if err != nil {
			return total, err
		}
		if count < 0 || count > 2*cleanupBatchSize {
			return total, errors.New("rolling quota cleanup returned an invalid batch count")
		}
		total += count
		if count < cleanupBatchSize {
			return total, nil
		}
	}
	return total, nil
}

func RunRollingQuotaCleanup(ctx context.Context, cleaner RollingQuotaCleaner) error {
	if cleaner == nil {
		return errors.New("rolling quota cleaner is required")
	}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return nil
		}
		count, err := CleanupExpiredRollingQuota(ctx, cleaner)
		if err != nil {
			slog.ErrorContext(ctx, "expired rolling quota cleanup failed", "updated_or_deleted", count, "error", err)
		} else {
			slog.InfoContext(ctx, "expired rolling quota entries cleaned", "updated_or_deleted", count)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
