// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package worker

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/notifications"
)

type notificationCleanupCall struct {
	limit  int
	result notifications.CleanupResult
	err    error
}

type notificationCleanupRecorder struct {
	calls           []notificationCleanupCall
	observedCutoffs []time.Time
	observedLimits  []int
}

func (r *notificationCleanupRecorder) CleanupExpiredNotifications(_ context.Context, cutoff time.Time, limit int) (notifications.CleanupResult, error) {
	if len(r.calls) == 0 {
		return notifications.CleanupResult{}, nil
	}
	call := r.calls[0]
	r.calls = r.calls[1:]
	if call.limit != limit {
		return notifications.CleanupResult{}, errors.New("unexpected notification cleanup batch limit")
	}
	r.observedCutoffs = append(r.observedCutoffs, cutoff)
	r.observedLimits = append(r.observedLimits, limit)
	return call.result, call.err
}

type notificationCountRecorder struct {
	counts map[string]int
	err    error
	users  []string
}

func (r *notificationCountRecorder) CountUnread(_ context.Context, userID string) (int, error) {
	r.users = append(r.users, userID)
	return r.counts[userID], r.err
}

type notificationCountEvent struct {
	userID string
	count  int
	reason string
}

type notificationPublisherRecorder struct {
	events []notificationCountEvent
}

func (r *notificationPublisherRecorder) PublishNotificationCountChanged(userID string, count int, reason string) {
	r.events = append(r.events, notificationCountEvent{userID: userID, count: count, reason: reason})
}

func TestCleanupExpiredNotificationsUsesLegacyRetentionAndPublishesUniqueUsersPerBatch(t *testing.T) {
	now := time.Date(2026, 10, 17, 3, 0, 0, 0, time.FixedZone("UTC+8", 8*60*60))
	cleaner := &notificationCleanupRecorder{calls: []notificationCleanupCall{
		{limit: cleanupBatchSize, result: notifications.CleanupResult{Count: cleanupBatchSize, UserIDs: []string{"user-b", "user-a", "user-a"}}},
		{limit: cleanupBatchSize, result: notifications.CleanupResult{Count: 17, UserIDs: []string{"user-a"}}},
	}}
	counts := &notificationCountRecorder{counts: map[string]int{"user-a": 2, "user-b": 5}}
	publisher := &notificationPublisherRecorder{}

	deleted, err := cleanupExpiredNotificationsAt(context.Background(), cleaner, counts, publisher, now)
	if err != nil || deleted != cleanupBatchSize+17 || len(cleaner.calls) != 0 {
		t.Fatalf("cleanup result = %d, %v; remaining batches=%d", deleted, err, len(cleaner.calls))
	}
	wantCutoff := now.UTC().Add(-365 * 24 * time.Hour)
	wantEvents := []notificationCountEvent{
		{userID: "user-b", count: 5, reason: "expired-cleanup"},
		{userID: "user-a", count: 2, reason: "expired-cleanup"},
		{userID: "user-a", count: 2, reason: "expired-cleanup"},
	}
	if len(publisher.events) != len(wantEvents) || !reflect.DeepEqual(publisher.events, wantEvents) {
		t.Fatalf("published events = %#v, want %#v", publisher.events, wantEvents)
	}
	if !reflect.DeepEqual(counts.users, []string{"user-b", "user-a", "user-a"}) {
		t.Fatalf("counted users = %#v", counts.users)
	}
	wantCutoffs := []time.Time{wantCutoff, wantCutoff}
	wantLimits := []int{cleanupBatchSize, cleanupBatchSize}
	if !reflect.DeepEqual(cleaner.observedCutoffs, wantCutoffs) || !reflect.DeepEqual(cleaner.observedLimits, wantLimits) {
		t.Fatalf("cleanup cutoff/limits = %#v/%#v, want %#v/%#v", cleaner.observedCutoffs, cleaner.observedLimits, wantCutoffs, wantLimits)
	}
}

func TestCleanupExpiredNotificationsStopsAtBatchBudget(t *testing.T) {
	cleaner := &notificationCleanupRecorder{}
	for range cleanupMaxBatches {
		cleaner.calls = append(cleaner.calls, notificationCleanupCall{
			limit:  cleanupBatchSize,
			result: notifications.CleanupResult{Count: cleanupBatchSize, UserIDs: []string{"user-1"}},
		})
	}
	counts := &notificationCountRecorder{counts: map[string]int{"user-1": 1}}
	publisher := &notificationPublisherRecorder{}
	deleted, err := CleanupExpiredNotifications(context.Background(), cleaner, counts, publisher)
	if err != nil || deleted != cleanupBatchSize*cleanupMaxBatches || len(cleaner.calls) != 0 || len(publisher.events) != cleanupMaxBatches {
		t.Fatalf("batch budget result = %d, %v; remaining=%d events=%d", deleted, err, len(cleaner.calls), len(publisher.events))
	}
}

func TestCleanupExpiredNotificationsReturnsCountFailure(t *testing.T) {
	wantErr := errors.New("database unavailable")
	cleaner := &notificationCleanupRecorder{calls: []notificationCleanupCall{{
		limit:  cleanupBatchSize,
		result: notifications.CleanupResult{Count: 1, UserIDs: []string{"user-1"}},
	}}}
	counts := &notificationCountRecorder{err: wantErr}
	publisher := &notificationPublisherRecorder{}
	deleted, err := CleanupExpiredNotifications(context.Background(), cleaner, counts, publisher)
	if deleted != 1 || !errors.Is(err, wantErr) || len(publisher.events) != 0 {
		t.Fatalf("count failure result = %d, %v; events=%#v", deleted, err, publisher.events)
	}
}

func TestRunDailyMaintenanceStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cleaner := &notificationCleanupRecorder{}
	counts := &notificationCountRecorder{}
	publisher := &notificationPublisherRecorder{}
	if err := RunDailyMaintenance(ctx, &cleanupRecorder{}, cleaner, counts, publisher, &rollingQuotaCleanupRecorder{}); err != nil {
		t.Fatalf("run daily maintenance after cancellation: %v", err)
	}
}
