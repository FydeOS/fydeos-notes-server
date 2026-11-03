// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package worker

import (
	"context"
	"errors"
	"testing"
	"time"
)

type cleanupCall struct {
	limit int64
	count int64
	err   error
}

type cleanupRecorder struct {
	calls []cleanupCall
}

func (r *cleanupRecorder) CleanupExpiredStorageReservationsV1(_ context.Context, limit int64) (int64, error) {
	if len(r.calls) == 0 {
		return 0, nil
	}
	call := r.calls[0]
	r.calls = r.calls[1:]
	if call.limit != limit {
		return 0, errors.New("unexpected batch limit")
	}
	return call.count, call.err
}

func TestCleanupExpiredStorageReservationsDrainsFullBatches(t *testing.T) {
	cleaner := &cleanupRecorder{calls: []cleanupCall{
		{limit: 1000, count: 1000},
		{limit: 1000, count: 23},
	}}
	count, err := CleanupExpiredStorageReservations(context.Background(), cleaner)
	if err != nil || count != 1023 || len(cleaner.calls) != 0 {
		t.Fatalf("cleanup result = %d, %v, remaining calls=%d", count, err, len(cleaner.calls))
	}
}

func TestCleanupExpiredStorageReservationsStopsAtBatchBudget(t *testing.T) {
	cleaner := &cleanupRecorder{}
	for range cleanupMaxBatches {
		cleaner.calls = append(cleaner.calls, cleanupCall{limit: cleanupBatchSize, count: cleanupBatchSize})
	}
	count, err := CleanupExpiredStorageReservations(context.Background(), cleaner)
	if err != nil || count != cleanupBatchSize*cleanupMaxBatches || len(cleaner.calls) != 0 {
		t.Fatalf("budget result = %d, %v, remaining calls=%d", count, err, len(cleaner.calls))
	}
}

func TestCleanupExpiredStorageReservationsStopsOnError(t *testing.T) {
	wantErr := errors.New("database unavailable")
	cleaner := &cleanupRecorder{calls: []cleanupCall{
		{limit: 1000, count: 5, err: wantErr},
	}}
	count, err := CleanupExpiredStorageReservations(context.Background(), cleaner)
	if count != 0 || !errors.Is(err, wantErr) || len(cleaner.calls) != 0 {
		t.Fatalf("error result = %d, %v, remaining calls=%d", count, err, len(cleaner.calls))
	}
}

func TestNextLocalMidnightUsesTheInputLocation(t *testing.T) {
	location := time.FixedZone("UTC+8", 8*60*60)
	now := time.Date(2026, 10, 17, 23, 30, 0, 0, location)
	got := NextLocalMidnight(now)
	want := time.Date(2026, 10, 18, 0, 0, 0, 0, location)
	if !got.Equal(want) || got.Location() != location {
		t.Fatalf("next midnight = %v (%v), want %v (%v)", got, got.Location(), want, location)
	}
}
