// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package storage

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type prefixRecorder struct {
	count    int64
	failures int
	started  chan struct{}
	resume   chan struct{}
	prefixes []string
}

func (r *prefixRecorder) DeletePrefix(_ context.Context, prefix string) (int64, error) {
	r.prefixes = append(r.prefixes, prefix)
	if r.failures > 0 {
		r.failures--
		return 0, errors.New("injected object storage failure")
	}
	if r.started != nil {
		select {
		case r.started <- struct{}{}:
		default:
		}
	}
	if r.resume != nil {
		<-r.resume
	}
	return r.count, nil
}

func TestPostgresWorkspaceObjectCleanupWaitsForLifecycleLockAndUsesLegacyPrefixes(t *testing.T) {
	pool := storageTestPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		CREATE TABLE workspaces (id varchar PRIMARY KEY);
		CREATE TABLE workspace_storage_cleanup_jobs (
			workspace_id text PRIMARY KEY,user_ids text[] NOT NULL DEFAULT ARRAY[]::text[],
			attempt integer NOT NULL DEFAULT 0,available_at timestamptz NOT NULL DEFAULT now(),
			lease_token uuid,lease_until timestamptz,last_error text,updated_at timestamptz NOT NULL DEFAULT now()
		);
		INSERT INTO workspaces(id) VALUES('workspace-live')`); err != nil {
		t.Fatal(err)
	}
	blobs := &prefixRecorder{count: 1}
	copilot := &prefixRecorder{count: 2, started: make(chan struct{}, 1), resume: make(chan struct{})}
	runtime := &Runtime{db: pool, backends: map[string]ObjectDeleter{BlobScope: blobs, CopilotScope: copilot}}
	if count, err := runtime.DeleteWorkspaceObjects(ctx, "workspace-live", nil); err != nil || count != 0 {
		t.Fatalf("cleanup of live workspace = %d, %v", count, err)
	}
	if len(blobs.prefixes) != 0 || len(copilot.prefixes) != 0 {
		t.Fatal("object cleanup ran while its workspace still existed")
	}
	if _, err := runtime.DeleteWorkspaceObjects(ctx, "comment-attachments", nil); err != ErrReservedWorkspaceID {
		t.Fatalf("reserved workspace error = %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM workspaces WHERE id='workspace-live'`); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := runtime.DeleteWorkspaceObjects(ctx, "workspace-live", []string{"user-1", "user-1"})
		result <- err
	}()
	select {
	case <-copilot.started:
	case <-time.After(5 * time.Second):
		t.Fatal("object cleanup did not reach the Copilot backend")
	}

	lockKey := "storage-workspace:workspace-live"
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var sharedLockAvailable bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock_shared(hashtextextended($1,0))`, lockKey).Scan(&sharedLockAvailable); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	_ = tx.Rollback(ctx)
	if sharedLockAvailable {
		t.Fatal("workspace document writer acquired a shared lock during prefix deletion")
	}

	close(copilot.resume)
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("delete workspace objects: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("workspace cleanup did not finish")
	}
	if len(blobs.prefixes) != 2 || strings.Join(blobs.prefixes, "|") != "workspace-live/|comment-attachments/workspace-live/" {
		t.Fatalf("Blob prefixes = %#v", blobs.prefixes)
	}
	wantCopilot := []string{
		"workspace-files/workspace-live/", "context-files/workspace-live/",
		"artifacts/workspace-live/", "user-1/workspace-live/",
	}
	if strings.Join(copilot.prefixes, "|") != strings.Join(wantCopilot, "|") {
		t.Fatalf("Copilot prefixes = %#v, want %#v", copilot.prefixes, wantCopilot)
	}
}

func TestPostgresWorkspaceCleanupFailureStaysQueuedAndRetries(t *testing.T) {
	pool := storageTestPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		CREATE TABLE workspaces (id varchar PRIMARY KEY);
		CREATE TABLE workspace_storage_cleanup_jobs (
			workspace_id text PRIMARY KEY,user_ids text[] NOT NULL DEFAULT ARRAY[]::text[],
			attempt integer NOT NULL DEFAULT 0,available_at timestamptz NOT NULL DEFAULT now(),
			lease_token uuid,lease_until timestamptz,last_error text,updated_at timestamptz NOT NULL DEFAULT now()
		);
		INSERT INTO workspace_storage_cleanup_jobs(workspace_id,user_ids) VALUES('workspace-retry',ARRAY['owner-1','member-1'])`); err != nil {
		t.Fatal(err)
	}
	blobs := &prefixRecorder{failures: 1}
	copilot := &prefixRecorder{}
	runtime := &Runtime{db: pool, backends: map[string]ObjectDeleter{BlobScope: blobs, CopilotScope: copilot}}
	cleaned, err := runtime.CleanupPendingWorkspaceStorage(ctx, 10)
	if cleaned != 1 || err == nil {
		t.Fatalf("first cleanup = %d, %v; want one attempted task and a retryable failure", cleaned, err)
	}
	var attempts int
	var lastError string
	var leaseToken, leaseUntil *string
	if err := pool.QueryRow(ctx, `SELECT attempt,last_error,lease_token::text,lease_until::text FROM workspace_storage_cleanup_jobs WHERE workspace_id='workspace-retry'`).Scan(&attempts, &lastError, &leaseToken, &leaseUntil); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || lastError != "storage_cleanup_failed" || leaseToken != nil || leaseUntil != nil {
		t.Fatalf("retry task state = attempt %d, reason %q, lease %v/%v", attempts, lastError, leaseToken, leaseUntil)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspace_storage_cleanup_jobs SET available_at=clock_timestamp()`); err != nil {
		t.Fatal(err)
	}
	cleaned, err = runtime.CleanupPendingWorkspaceStorage(ctx, 10)
	if cleaned != 1 || err != nil {
		t.Fatalf("retry cleanup = %d, %v; want successful task completion", cleaned, err)
	}
	var remaining int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspace_storage_cleanup_jobs`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("remaining cleanup jobs = %d, %v", remaining, err)
	}
	if len(blobs.prefixes) != 3 || len(copilot.prefixes) != 5 {
		t.Fatalf("cleanup prefixes after retry = blob %#v, copilot %#v", blobs.prefixes, copilot.prefixes)
	}
}

func TestPostgresWorkspaceCleanupRenewsActiveLease(t *testing.T) {
	pool := storageTestPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		CREATE TABLE workspaces (id varchar PRIMARY KEY);
		CREATE TABLE workspace_storage_cleanup_jobs (
			workspace_id text PRIMARY KEY,user_ids text[] NOT NULL DEFAULT ARRAY[]::text[],
			attempt integer NOT NULL DEFAULT 0,available_at timestamptz NOT NULL DEFAULT now(),
			lease_token uuid,lease_until timestamptz,last_error text,updated_at timestamptz NOT NULL DEFAULT now()
		);
		INSERT INTO workspace_storage_cleanup_jobs(workspace_id) VALUES('workspace-lease')`); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{}, 1)
	resume := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(resume)
		}
	}()
	blobs := &prefixRecorder{started: started, resume: resume}
	objects := map[string]ObjectDeleter{BlobScope: blobs, CopilotScope: &prefixRecorder{}}
	first := &Runtime{db: pool, backends: objects, workspaceCleanupLeaseDuration: 300 * time.Millisecond}
	firstResult := make(chan error, 1)
	go func() {
		count, err := first.CleanupPendingWorkspaceStorage(ctx, 1)
		if count != 1 && err == nil {
			err = errors.New("active workspace cleanup was not attempted")
		}
		firstResult <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("workspace cleanup did not reach the object store")
	}

	time.Sleep(700 * time.Millisecond)
	secondObjects := map[string]ObjectDeleter{BlobScope: &prefixRecorder{}, CopilotScope: &prefixRecorder{}}
	second := &Runtime{db: pool, backends: secondObjects, workspaceCleanupLeaseDuration: 300 * time.Millisecond}
	count, err := second.CleanupPendingWorkspaceStorage(ctx, 1)
	if err != nil || count != 0 {
		t.Fatalf("second worker claimed active cleanup = %d, %v; want no claim", count, err)
	}

	close(resume)
	released = true
	select {
	case err := <-firstResult:
		if err != nil {
			t.Fatalf("first worker cleanup: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first workspace cleanup did not finish")
	}
}

func storageTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("NOTES_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set NOTES_TEST_DATABASE_URL to a disposable local notes_test database")
	}
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatalf("parse NOTES_TEST_DATABASE_URL: %v", err)
	}
	if parsed.Hostname() != "localhost" && parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "::1" {
		t.Fatal("integration tests require a loopback PostgreSQL host")
	}
	if strings.Trim(parsed.Path, "/") != "notes_test" {
		t.Fatal("integration tests require a disposable database named notes_test")
	}
	adminPool, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		t.Fatalf("connect integration PostgreSQL: %v", err)
	}
	schema := "notes_storage_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := adminPool.Exec(context.Background(), `CREATE SCHEMA `+schema); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated schema: %v", err)
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		_, _ = adminPool.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
		adminPool.Close()
		t.Fatalf("parse integration pool config: %v", err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		_, _ = adminPool.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
		adminPool.Close()
		t.Fatalf("open schema-scoped integration pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = adminPool.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
		adminPool.Close()
	})
	return pool
}
