// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package worker

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

const workspaceCleanupInterval = time.Minute

type WorkspaceStorageCleaner interface {
	CleanupPendingWorkspaceStorage(context.Context, int) (int, error)
}

func CleanupWorkspaceStorage(ctx context.Context, cleaner WorkspaceStorageCleaner) (int, error) {
	if cleaner == nil {
		return 0, errors.New("workspace storage cleaner is required")
	}
	var total int
	var failures []error
	for batch := 0; batch < cleanupMaxBatches; batch++ {
		count, err := cleaner.CleanupPendingWorkspaceStorage(ctx, cleanupBatchSize)
		if err != nil {
			failures = append(failures, err)
		}
		if count < 0 || count > cleanupBatchSize {
			return total, errors.New("workspace storage cleanup returned an invalid batch count")
		}
		total += count
		if count < cleanupBatchSize {
			return total, errors.Join(failures...)
		}
	}
	return total, errors.Join(failures...)
}

func RunWorkspaceStorageCleanup(ctx context.Context, cleaner WorkspaceStorageCleaner) error {
	if cleaner == nil {
		return errors.New("workspace storage cleaner is required")
	}
	for {
		if ctx.Err() != nil {
			return nil
		}
		count, err := CleanupWorkspaceStorage(ctx, cleaner)
		if err != nil {
			slog.ErrorContext(ctx, "workspace storage cleanup pass failed", "attempted", count, "error", err)
		} else if count != 0 {
			slog.InfoContext(ctx, "workspace storage cleanup pass completed", "attempted", count)
		}
		timer := time.NewTimer(workspaceCleanupInterval)
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
		}
	}
}
