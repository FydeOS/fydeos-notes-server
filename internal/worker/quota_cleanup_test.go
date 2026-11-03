// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package worker

import (
	"context"
	"errors"
	"testing"
)

type rollingQuotaCleanupCall struct {
	count int64
	err   error
}

type rollingQuotaCleanupRecorder struct {
	calls     []int64
	responses []rollingQuotaCleanupCall
}

func (r *rollingQuotaCleanupRecorder) CleanupExpiredRollingQuota(_ context.Context, limit int64) (int64, error) {
	r.calls = append(r.calls, limit)
	if len(r.responses) == 0 {
		return 0, nil
	}
	response := r.responses[0]
	r.responses = r.responses[1:]
	return response.count, response.err
}

func TestCleanupExpiredRollingQuotaDrainsFullBatches(t *testing.T) {
	cleaner := &rollingQuotaCleanupRecorder{responses: []rollingQuotaCleanupCall{{count: 2000}, {count: 1000}, {count: 3}}}
	count, err := CleanupExpiredRollingQuota(context.Background(), cleaner)
	if err != nil || count != 3003 || len(cleaner.calls) != 3 {
		t.Fatalf("cleanup result=%d err=%v calls=%v", count, err, cleaner.calls)
	}
	for _, limit := range cleaner.calls {
		if limit != cleanupBatchSize {
			t.Fatalf("cleanup batch limit=%d; want %d", limit, cleanupBatchSize)
		}
	}
}

func TestCleanupExpiredRollingQuotaStopsAtBatchBudget(t *testing.T) {
	cleaner := &rollingQuotaCleanupRecorder{}
	for range cleanupMaxBatches {
		cleaner.responses = append(cleaner.responses, rollingQuotaCleanupCall{count: 2000})
	}
	count, err := CleanupExpiredRollingQuota(context.Background(), cleaner)
	if err != nil || count != int64(2*cleanupBatchSize*cleanupMaxBatches) || len(cleaner.calls) != cleanupMaxBatches {
		t.Fatalf("cleanup budget result=%d err=%v calls=%d", count, err, len(cleaner.calls))
	}
}

func TestCleanupExpiredRollingQuotaReturnsDatabaseFailure(t *testing.T) {
	wantErr := errors.New("database unavailable")
	cleaner := &rollingQuotaCleanupRecorder{responses: []rollingQuotaCleanupCall{{count: 2000}, {err: wantErr}}}
	count, err := CleanupExpiredRollingQuota(context.Background(), cleaner)
	if count != 2000 || !errors.Is(err, wantErr) {
		t.Fatalf("cleanup failure result=%d err=%v", count, err)
	}
}

func TestCleanupExpiredRollingQuotaRejectsInvalidCounts(t *testing.T) {
	for _, invalid := range []int64{-1, 2*cleanupBatchSize + 1} {
		cleaner := &rollingQuotaCleanupRecorder{responses: []rollingQuotaCleanupCall{{count: invalid}}}
		if _, err := CleanupExpiredRollingQuota(context.Background(), cleaner); err == nil {
			t.Fatalf("cleanup accepted invalid count %d", invalid)
		}
	}
}
