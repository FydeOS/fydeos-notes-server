// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package copilot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/storage"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestTranscriptTasksPostgresScopeAndLatestBlob(t *testing.T) {
	databaseURL := os.Getenv("NOTES_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set NOTES_TEST_DATABASE_URL to a disposable local notes_test database")
	}
	parsed, err := pgxpool.ParseConfig(databaseURL)
	if err != nil || (parsed.ConnConfig.Host != "localhost" && parsed.ConnConfig.Host != "127.0.0.1" && parsed.ConnConfig.Host != "::1") {
		t.Fatal("transcript task integration test requires a loopback PostgreSQL URL")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	schema := "transcript_task_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	parsed.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, `SET search_path TO `+schema)
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, parsed)
	if err != nil {
		_, _ = admin.Exec(ctx, `DROP SCHEMA `+schema+` CASCADE`)
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		if _, err := admin.Exec(ctx, `DROP SCHEMA `+schema+` CASCADE`); err != nil {
			t.Errorf("drop transcript task test schema: %v", err)
		}
		admin.Close()
	})
	if _, err := pool.Exec(ctx, `CREATE TABLE ai_transcript_tasks(
		id varchar PRIMARY KEY,user_id varchar NOT NULL,workspace_id varchar NOT NULL,blob_id varchar NOT NULL,
		status varchar NOT NULL,recipe_id varchar,recipe_version varchar,action_run_id varchar,dispatch_generation varchar,
		input_snapshot jsonb,public_meta jsonb,protected_result jsonb,error_code varchar,settled_at timestamptz,created_at timestamptz NOT NULL DEFAULT current_timestamp,
		updated_at timestamptz NOT NULL DEFAULT current_timestamp)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE ai_action_runs(
		id varchar PRIMARY KEY,user_id varchar NOT NULL,workspace_id varchar NOT NULL,action_id varchar NOT NULL,
		action_version varchar NOT NULL,status varchar NOT NULL,attempt integer NOT NULL,retry_of varchar,
		input_snapshot json,result json,error_code varchar,created_at timestamptz NOT NULL DEFAULT current_timestamp,
		updated_at timestamptz NOT NULL DEFAULT current_timestamp)`); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct{ id, user, workspace, blob, status, result string }{
		{"task-old", "user-1", "workspace-1", "blob-1", "failed", `null`},
		{"task-new", "user-1", "workspace-1", "blob-1", "settled", `{"version":"v2"}`},
		{"task-ready", "user-1", "workspace-1", "blob-2", "ready", `{"version":"v2"}`},
		{"task-other-user", "user-2", "workspace-1", "blob-1", "settled", `{"version":"private"}`},
		{"task-other-workspace", "user-1", "workspace-2", "blob-1", "settled", `{"version":"private"}`},
	} {
		if _, err := pool.Exec(ctx, `INSERT INTO ai_transcript_tasks(id,user_id,workspace_id,blob_id,status,protected_result)
			VALUES($1,$2,$3,$4,$5,$6::jsonb)`, row.id, row.user, row.workspace, row.blob, row.status, row.result); err != nil {
			t.Fatal(err)
		}
	}
	objects := &transcriptTaskObjectStore{objects: make(map[string][]byte)}
	service := NewTranscriptTasks(pool, objects)
	blobID := "blob-1"
	task, err := service.GetForUser(ctx, "user-1", "workspace-1", nil, &blobID)
	if err != nil || task == nil || task.ID != "task-new" || task.Status != "settled" || string(task.ProtectedResult) != `{"version": "v2"}` {
		t.Fatalf("latest scoped task = %#v, err=%v", task, err)
	}
	taskID := "task-other-user"
	task, err = service.GetForUser(ctx, "user-1", "workspace-1", &taskID, nil)
	if err != nil || task != nil {
		t.Fatalf("cross-user task = %#v, err=%v", task, err)
	}
	taskID = "task-old"
	task, err = service.GetForUser(ctx, "user-1", "workspace-2", &taskID, &blobID)
	if err != nil || task != nil {
		t.Fatalf("cross-workspace task = %#v, err=%v", task, err)
	}
	task, err = service.GetForUser(ctx, "user-1", "workspace-1", nil, nil)
	if err != nil || task != nil {
		t.Fatalf("empty selector task = %#v, err=%v", task, err)
	}
	settled, err := service.SettleForUser(ctx, "user-1", "workspace-1", "task-ready")
	if err != nil || settled == nil || settled.Status != "settled" || len(settled.ProtectedResult) == 0 {
		t.Fatalf("settle task = %#v, err=%v", settled, err)
	}
	var settledAt *string
	if err := pool.QueryRow(ctx, `SELECT settled_at::text FROM ai_transcript_tasks WHERE id='task-ready'`).Scan(&settledAt); err != nil || settledAt == nil {
		t.Fatalf("settled_at = %v, err=%v", settledAt, err)
	}

	var wg sync.WaitGroup
	type result struct {
		task *TranscriptTask
		err  error
	}
	results := make(chan result, 2)
	for _, audio := range []string{"first audio", "second audio"} {
		wg.Add(1)
		go func(audio string) {
			defer wg.Done()
			task, err := service.SubmitForUser(ctx, "user-1", "workspace-1", "blob-race", []TranscriptUpload{{FileName: "meeting.mp3", ContentType: "audio/mpeg", Body: bytes.NewReader([]byte(audio))}}, TranscriptSubmitInput{}, false)
			results <- result{task: task, err: err}
		}(audio)
	}
	wg.Wait()
	close(results)
	var accepted *TranscriptTask
	var duplicateCount int
	for item := range results {
		if item.err == nil {
			accepted = item.task
		} else if errors.Is(item.err, ErrTranscriptJobExists) {
			duplicateCount++
		} else {
			t.Fatalf("concurrent transcript submit error = %v", item.err)
		}
	}
	if accepted == nil || accepted.Status != "pending" || duplicateCount != 1 || objects.count() != 1 {
		t.Fatalf("accepted task=%#v duplicate count=%d stored object count=%d", accepted, duplicateCount, objects.count())
	}
	var generation string
	if err := pool.QueryRow(ctx, `SELECT dispatch_generation FROM ai_transcript_tasks WHERE id=$1`, accepted.ID).Scan(&generation); err != nil || generation == "" {
		t.Fatalf("dispatch generation=%q err=%v", generation, err)
	}
	claimed := make(chan *TranscriptExecutionTask, 2)
	claimErrors := make(chan error, 2)
	var claimWG sync.WaitGroup
	for range 2 {
		claimWG.Add(1)
		go func() {
			defer claimWG.Done()
			task, err := service.ClaimNext(ctx)
			if err != nil {
				claimErrors <- err
				return
			}
			claimed <- task
		}()
	}
	claimWG.Wait()
	close(claimed)
	close(claimErrors)
	for err := range claimErrors {
		t.Fatal(err)
	}
	var claimedTask *TranscriptExecutionTask
	claimCount := 0
	for task := range claimed {
		if task != nil {
			claimedTask = task
			claimCount++
		}
	}
	if claimCount != 1 || claimedTask == nil {
		t.Fatalf("concurrent pending claims = %d, want exactly one", claimCount)
	}
	if claimedTask.ActionRunID == "" || claimedTask.RetryOf != "" {
		t.Fatalf("first transcript action run = %#v", claimedTask)
	}
	var runStatus, actionID, actionVersion string
	var attempt int
	if err := pool.QueryRow(ctx, `SELECT status,action_id,action_version,attempt FROM ai_action_runs WHERE id=$1`, claimedTask.ActionRunID).Scan(&runStatus, &actionID, &actionVersion, &attempt); err != nil || runStatus != "running" || actionID != "transcript.audio" || actionVersion != "v1" || attempt != 1 {
		t.Fatalf("created transcript action run = %q/%q/%q/%d, err=%v", runStatus, actionID, actionVersion, attempt, err)
	}
	completed, err := service.CompleteDispatch(ctx, *claimedTask, json.RawMessage(`{"sourceAudio":{"blobId":"blob-race"},"normalizedTranscript":"00:00:01 Speaker A: hello","version":"transcript-result-v1"}`))
	if err != nil || !completed {
		t.Fatalf("complete claimed transcript = %t, err=%v", completed, err)
	}
	var taskStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM ai_transcript_tasks WHERE id=$1`, accepted.ID).Scan(&taskStatus); err != nil || taskStatus != "ready" {
		t.Fatalf("completed transcript status = %q, err=%v", taskStatus, err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM ai_action_runs WHERE id=$1`, claimedTask.ActionRunID).Scan(&runStatus); err != nil || runStatus != "succeeded" {
		t.Fatalf("completed transcript action run status = %q, err=%v", runStatus, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE ai_transcript_tasks SET status='failed',error_code='provider_error' WHERE id=$1`, accepted.ID); err != nil {
		t.Fatal(err)
	}
	retried, err := service.RetryForUser(ctx, "user-1", "workspace-1", accepted.ID)
	if err != nil || retried == nil || retried.Status != "pending" {
		t.Fatalf("retry task = %#v err=%v", retried, err)
	}
	var retryGeneration string
	var retryError *string
	if err := pool.QueryRow(ctx, `SELECT dispatch_generation,error_code FROM ai_transcript_tasks WHERE id=$1`, accepted.ID).Scan(&retryGeneration, &retryError); err != nil || retryGeneration == generation || retryError != nil {
		t.Fatalf("retry generation/error = %q/%v, err=%v", retryGeneration, retryError, err)
	}
	failingWorker := NewTranscriptTaskWorker(service, transcriptTaskExecutorFunc(func(context.Context, TranscriptExecutionTask) (json.RawMessage, error) {
		return nil, errors.New("provider response included sensitive detail")
	}), nil)
	processed, err := failingWorker.ProcessOne(ctx)
	if err != nil || !processed {
		t.Fatalf("process retried transcript = %t, err=%v", processed, err)
	}
	var retryRunID, retryOf, retryStatus, retryErrorCode string
	var retryAttempt int
	if err := pool.QueryRow(ctx, `SELECT run.id,run.retry_of,run.status,run.error_code,run.attempt
		FROM ai_transcript_tasks task JOIN ai_action_runs run ON run.id=task.action_run_id WHERE task.id=$1`, accepted.ID).
		Scan(&retryRunID, &retryOf, &retryStatus, &retryErrorCode, &retryAttempt); err != nil || retryRunID == claimedTask.ActionRunID || retryOf != claimedTask.ActionRunID || retryStatus != "failed" || retryErrorCode != TranscriptProviderErrorCode || retryAttempt != 2 {
		t.Fatalf("retry transcript action run = %q retry_of=%q status=%q error=%q attempt=%d, err=%v", retryRunID, retryOf, retryStatus, retryErrorCode, retryAttempt, err)
	}
	var taskError *string
	if err := pool.QueryRow(ctx, `SELECT status,error_code FROM ai_transcript_tasks WHERE id=$1`, accepted.ID).Scan(&taskStatus, &taskError); err != nil || taskStatus != "failed" || taskError == nil || *taskError != TranscriptProviderErrorCode {
		t.Fatalf("failed transcript status/error = %q/%v, err=%v", taskStatus, taskError, err)
	}
	updated, err := service.CompleteDispatch(ctx, TranscriptExecutionTask{ID: accepted.ID, UserID: "user-1", DispatchGeneration: generation}, json.RawMessage(`{"version":"transcript-result-v1"}`))
	if err != nil || updated {
		t.Fatalf("stale generation completion updated=%t err=%v", updated, err)
	}
	if _, err := service.RetryForUser(ctx, "user-1", "workspace-1", accepted.ID); err != nil {
		t.Fatalf("retry failed task: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ai_transcript_tasks(id,user_id,workspace_id,blob_id,status,dispatch_generation,updated_at)
		VALUES('task-stale','user-1','workspace-1','blob-stale','running','generation-stale',clock_timestamp()-interval '2 hours')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ai_action_runs(id,user_id,workspace_id,action_id,action_version,status,attempt)
		VALUES('run-stale','user-1','workspace-1','transcript.audio','v1','running',1);
		UPDATE ai_transcript_tasks SET action_run_id='run-stale' WHERE id='task-stale'`); err != nil {
		t.Fatal(err)
	}
	stale, err := service.FailStaleRunning(ctx, time.Now().Add(-time.Hour))
	if err != nil || len(stale) != 1 || stale[0].ID != "task-stale" || stale[0].Status != "failed" || stale[0].ErrorCode != "transcript_dispatch_timed_out" {
		t.Fatalf("stale transcript recovery = %#v, err=%v", stale, err)
	}
	if err := pool.QueryRow(ctx, `SELECT status,error_code FROM ai_action_runs WHERE id='run-stale'`).Scan(&runStatus, &retryErrorCode); err != nil || runStatus != "failed" || retryErrorCode != "transcript_dispatch_timed_out" {
		t.Fatalf("stale transcript action run = %q/%q, err=%v", runStatus, retryErrorCode, err)
	}
	// Consume the retried persisted upload through real HTTP provider adapters,
	// then read and settle the generated result through the task repository.
	targets, calls := transcriptHTTPFixture(t, nil)
	executor := NewTranscriptExecutor(objects, targets, func(context.Context, string, string, bool) error { return nil })
	service.SetPreflight(executor.Preflight)
	providerWorker := NewTranscriptTaskWorker(service, executor, nil)
	processed, err = providerWorker.ProcessOne(ctx)
	if err != nil || !processed || calls.Load() != 2 {
		t.Fatalf("provider worker processed=%v calls=%d err=%v", processed, calls.Load(), err)
	}
	ready, err := service.GetForUser(ctx, "user-1", "workspace-1", &accepted.ID, nil)
	if err != nil || ready == nil || ready.Status != "ready" || !bytes.Contains(ready.ProtectedResult, []byte("00:00:00 A: Hello")) {
		t.Fatalf("provider result = %#v, %v", ready, err)
	}
	settled, err = service.SettleForUser(ctx, "user-1", "workspace-1", accepted.ID)
	if err != nil || settled == nil || settled.Status != "settled" || !bytes.Contains(settled.ProtectedResult, []byte("summaryJson")) {
		t.Fatalf("provider result settlement = %#v, %v", settled, err)
	}
	service.SetPreflight(func(context.Context, string, string, bool) error { return errTranscriptRoute })
	before := objects.count()
	_, err = service.SubmitForUser(ctx, "user-1", "workspace-1", "unroutable", []TranscriptUpload{{Body: strings.NewReader("audio")}}, TranscriptSubmitInput{}, false)
	if !errors.Is(err, errTranscriptRoute) || objects.count() != before {
		t.Fatalf("unroutable submission stored audio, err=%v", err)
	}
	t.Run("slow provider does not block other tasks or recovery", func(t *testing.T) {
		service.SetPreflight(nil)
		for _, blob := range []string{"slow-one", "slow-two"} {
			if _, err := service.SubmitForUser(ctx, "user-1", "workspace-1", blob, []TranscriptUpload{{Body: strings.NewReader("audio")}}, TranscriptSubmitInput{}, false); err != nil {
				t.Fatal(err)
			}
		}
		started := make(chan struct{}, 2)
		worker := NewTranscriptTaskWorker(service, transcriptTaskExecutorFunc(func(ctx context.Context, _ TranscriptExecutionTask) (json.RawMessage, error) {
			started <- struct{}{}
			<-ctx.Done()
			return nil, ctx.Err()
		}), nil)
		worker.recoveryInterval = 10 * time.Millisecond
		runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		done := make(chan error, 1)
		go func() { done <- worker.Run(runCtx) }()
		defer func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("worker shutdown: %v", err)
			}
		}()
		for range 2 {
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("a slow task blocked the next task")
			}
		}
		if _, err := pool.Exec(ctx, `INSERT INTO ai_transcript_tasks(id,user_id,workspace_id,blob_id,status,dispatch_generation,updated_at) VALUES('stale-during-provider','user-1','workspace-1','stale-concurrent','running','old-generation',clock_timestamp()-interval '2 hours')`); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(2 * time.Second)
		for {
			var status string
			if err := pool.QueryRow(ctx, `SELECT status FROM ai_transcript_tasks WHERE id='stale-during-provider'`).Scan(&status); err != nil {
				t.Fatal(err)
			}
			if status == "failed" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("slow provider blocked stale recovery")
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	t.Run("adopt legacy pending generation", func(t *testing.T) {
		snapshot := `{"scopeMode":"canonical","infos":[{"key":"legacy-blob-0","mimeType":"audio/wav","index":0}]}`
		if _, err := pool.Exec(ctx, `INSERT INTO ai_transcript_tasks(id,user_id,workspace_id,blob_id,status,input_snapshot) VALUES('legacy-pending','user-1','workspace-1','legacy-blob','pending',$1::jsonb)`, snapshot); err != nil {
			t.Fatal(err)
		}
		if _, err := objects.PutObject(ctx, storage.CopilotScope, "user-1/workspace-1/legacy-blob-0", strings.NewReader("legacy audio"), storage.PutOptions{}); err != nil {
			t.Fatal(err)
		}
		task, err := service.ClaimNext(ctx)
		if err != nil || task == nil || task.ID != "legacy-pending" || task.BlobID != "legacy-blob" || task.DispatchGeneration == "" {
			t.Fatalf("legacy pending task not claimed: %#v err=%v", task, err)
		}
		result, err := executor.ExecuteTranscript(ctx, *task)
		if err != nil {
			t.Fatal(err)
		}
		if done, err := service.CompleteDispatch(ctx, *task, result); err != nil || !done {
			t.Fatalf("legacy completion=%v err=%v", done, err)
		}
	})

}

type transcriptTaskExecutorFunc func(context.Context, TranscriptExecutionTask) (json.RawMessage, error)

func (f transcriptTaskExecutorFunc) ExecuteTranscript(ctx context.Context, task TranscriptExecutionTask) (json.RawMessage, error) {
	return f(ctx, task)
}

type transcriptTaskObjectStore struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func (s *transcriptTaskObjectStore) PutObject(_ context.Context, _, key string, body io.Reader, _ storage.PutOptions) (storage.ObjectMetadata, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return storage.ObjectMetadata{}, err
	}
	s.mu.Lock()
	s.objects[key] = data
	s.mu.Unlock()
	return storage.ObjectMetadata{ContentLength: int64(len(data))}, nil
}

func (s *transcriptTaskObjectStore) DeleteObject(_ context.Context, _, key string) error {
	s.mu.Lock()
	delete(s.objects, key)
	s.mu.Unlock()
	return nil
}

func (s *transcriptTaskObjectStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.objects)
}
