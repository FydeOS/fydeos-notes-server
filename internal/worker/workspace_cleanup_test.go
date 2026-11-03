// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package worker

import (
	"context"
	"errors"
	"testing"
)

type workspaceCleanupCall struct {
	count int
	err   error
}

type workspaceCleanupRecorder struct {
	calls []workspaceCleanupCall
}

func (r *workspaceCleanupRecorder) CleanupPendingWorkspaceStorage(_ context.Context, limit int) (int, error) {
	if limit != cleanupBatchSize {
		return 0, errors.New("unexpected workspace cleanup batch size")
	}
	if len(r.calls) == 0 {
		return 0, nil
	}
	call := r.calls[0]
	r.calls = r.calls[1:]
	return call.count, call.err
}

func TestCleanupWorkspaceStorageDrainsBatchesAndReturnsFailures(t *testing.T) {
	wantErr := errors.New("temporary object storage failure")
	cleaner := &workspaceCleanupRecorder{calls: []workspaceCleanupCall{
		{count: cleanupBatchSize, err: wantErr},
		{count: 3},
	}}
	count, err := CleanupWorkspaceStorage(context.Background(), cleaner)
	if count != cleanupBatchSize+3 || !errors.Is(err, wantErr) || len(cleaner.calls) != 0 {
		t.Fatalf("cleanup result = %d, %v, remaining calls=%d", count, err, len(cleaner.calls))
	}
}

func TestRunWorkspaceStorageCleanupStopsWhenAlreadyCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cleaner := &workspaceCleanupRecorder{}
	if err := RunWorkspaceStorageCleanup(ctx, cleaner); err != nil {
		t.Fatal(err)
	}
	if len(cleaner.calls) != 0 {
		t.Fatal("canceled workspace cleanup worker called the cleaner")
	}
}
