// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"
)

const TranscriptProviderErrorCode = "transcript_provider_error"
const transcriptDispatchTimeout = time.Hour

type TranscriptTaskExecutor interface {
	ExecuteTranscript(context.Context, TranscriptExecutionTask) (json.RawMessage, error)
}

type TranscriptTaskChangePublisher interface {
	PublishTranscriptTaskChanged(workspaceID, taskID, status, errorCode string)
}

type TranscriptTaskWorker struct {
	tasks            *TranscriptTasks
	executor         TranscriptTaskExecutor
	publisher        TranscriptTaskChangePublisher
	interval         time.Duration
	recoveryInterval time.Duration
}

func NewTranscriptTaskWorker(tasks *TranscriptTasks, executor TranscriptTaskExecutor, publisher TranscriptTaskChangePublisher) *TranscriptTaskWorker {
	return &TranscriptTaskWorker{tasks: tasks, executor: executor, publisher: publisher, interval: time.Second}
}

func (w *TranscriptTaskWorker) Run(ctx context.Context) error {
	if w == nil || w.tasks == nil || w.executor == nil {
		return errors.New("transcript worker dependencies are required")
	}
	workerCtx, cancel := context.WithCancel(ctx)
	var running sync.WaitGroup
	defer func() { cancel(); running.Wait() }()
	// Keep the provider concurrency bounded without letting one long recording
	// block all other tasks. Recovery runs independently of both provider calls.
	for range 2 {
		running.Add(1)
		go func() { defer running.Done(); w.runDispatchLoop(workerCtx) }()
	}
	recoveryInterval := w.recoveryInterval
	if recoveryInterval <= 0 {
		recoveryInterval = time.Minute
	}
	for {
		if ctx.Err() != nil {
			return nil
		}
		delay := recoveryInterval
		if err := w.RecoverStale(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			slog.ErrorContext(ctx, "transcript recovery failed", "error", err)
			delay = time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

func (w *TranscriptTaskWorker) runDispatchLoop(ctx context.Context) {
	interval := w.interval
	if interval <= 0 {
		interval = time.Second
	}
	for {
		if ctx.Err() != nil {
			return
		}
		processed, err := w.ProcessOne(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.ErrorContext(ctx, "transcript dispatch failed", "error", err)
		} else if processed {
			continue
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (w *TranscriptTaskWorker) RecoverStale(ctx context.Context) error {
	if w == nil || w.tasks == nil {
		return errors.New("transcript worker dependencies are required")
	}
	changes, err := w.tasks.FailStaleRunning(ctx, time.Now().Add(-transcriptDispatchTimeout))
	if err != nil {
		return err
	}
	for _, change := range changes {
		if w.publisher != nil {
			w.publisher.PublishTranscriptTaskChanged(change.WorkspaceID, change.ID, change.Status, change.ErrorCode)
		}
	}
	return nil
}

func (w *TranscriptTaskWorker) ProcessOne(ctx context.Context) (bool, error) {
	if w == nil || w.tasks == nil || w.executor == nil {
		return false, errors.New("transcript worker dependencies are required")
	}
	task, err := w.tasks.ClaimNext(ctx)
	if err != nil || task == nil {
		return false, err
	}
	w.publish(task, "running", "")
	executeCtx, cancelExecute := context.WithTimeout(ctx, transcriptDispatchTimeout-5*time.Minute)
	result, executeErr := w.executor.ExecuteTranscript(executeCtx, *task)
	cancelExecute()
	if executeErr == nil {
		var payload map[string]json.RawMessage
		if json.Unmarshal(result, &payload) != nil || payload == nil {
			executeErr = errTranscriptResult
		}
	}
	if executeErr != nil {
		finishCtx, cancelFinish := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancelFinish()
		code := transcriptErrorCode(executeErr)
		updated, err := w.tasks.FailDispatch(finishCtx, *task, code)
		if err != nil {
			return true, err
		}
		if updated {
			w.publish(task, "failed", code)
		}
		return true, nil
	}
	updated, err := w.tasks.CompleteDispatch(ctx, *task, result)
	if err != nil {
		return true, err
	}
	if updated {
		w.publish(task, "ready", "")
	}
	return true, nil
}

func (w *TranscriptTaskWorker) publish(task *TranscriptExecutionTask, status, errorCode string) {
	if w.publisher != nil && task != nil {
		w.publisher.PublishTranscriptTaskChanged(task.WorkspaceID, task.ID, status, errorCode)
	}
}

type transcriptCodedError interface {
	TranscriptErrorCode() string
}

func transcriptErrorCode(err error) string {
	var coded transcriptCodedError
	if errors.As(err, &coded) {
		if code := strings.TrimSpace(coded.TranscriptErrorCode()); validTranscriptErrorCode(code) && len(code) <= 128 {
			return code
		}
	}
	return TranscriptProviderErrorCode
}
