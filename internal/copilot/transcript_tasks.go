// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package copilot

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/storage"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const maxTranscriptUploadBytes int64 = 50 << 20

var (
	ErrTranscriptAudioNotProvided = errors.New("copilot_transcription_audio_not_provided")
	ErrTranscriptJobExists        = errors.New("copilot_transcription_job_exists")
	ErrTranscriptJobNotFound      = errors.New("copilot_transcription_job_not_found")
	ErrTranscriptRetryNotAllowed  = errors.New("only failed transcript tasks can be retried")
)

type transcriptTaskDB interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Begin(context.Context) (pgx.Tx, error)
}

type transcriptTaskObjects interface {
	PutObject(context.Context, string, string, io.Reader, storage.PutOptions) (storage.ObjectMetadata, error)
	DeleteObject(context.Context, string, string) error
}

type TranscriptUpload struct {
	FileName    string
	ContentType string
	Body        io.Reader
}

type TranscriptSubmitInput struct {
	SourceAudio   json.RawMessage
	Quality       json.RawMessage
	SliceManifest json.RawMessage
}

type TranscriptTask struct {
	ID              string
	Status          string
	ProtectedResult []byte
}

type TranscriptExecutionTask struct {
	ID                 string
	UserID             string
	WorkspaceID        string
	BlobID             string
	ActionRunID        string
	RetryOf            string
	DispatchGeneration string
	InputSnapshot      []byte
}

type TranscriptTaskStatusChange struct {
	ID          string
	WorkspaceID string
	Status      string
	ErrorCode   string
}

type TranscriptTasks struct {
	db        transcriptTaskDB
	objects   transcriptTaskObjects
	preflight func(context.Context, string, string, bool) error
}

func (s *TranscriptTasks) SetPreflight(check func(context.Context, string, string, bool) error) {
	s.preflight = check
}

func NewTranscriptTasks(db transcriptTaskDB, objects ...transcriptTaskObjects) *TranscriptTasks {
	service := &TranscriptTasks{db: db}
	if len(objects) > 0 {
		service.objects = objects[0]
	}
	return service
}

func (s *TranscriptTasks) SubmitForUser(ctx context.Context, userID, workspaceID, blobID string, uploads []TranscriptUpload, input TranscriptSubmitInput, personal bool) (*TranscriptTask, error) {
	if s == nil || s.db == nil || s.objects == nil {
		return nil, errors.New("transcript task service is unavailable")
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(workspaceID) == "" || strings.TrimSpace(blobID) == "" || strings.ContainsAny(blobID, "/\\\x00") {
		return nil, errors.New("invalid transcript task scope")
	}
	if len(uploads) == 0 {
		return nil, ErrTranscriptAudioNotProvided
	}
	if s.preflight != nil {
		if err := s.preflight(ctx, userID, workspaceID, personal); err != nil {
			return nil, err
		}
	}
	taskID, generation := uuid.NewString(), uuid.NewString()
	infos := make([]map[string]any, 0, len(uploads))
	objectKeys := make([]string, 0, len(uploads))
	var total int64
	cleanup := func() {
		for _, key := range objectKeys {
			_ = s.objects.DeleteObject(context.WithoutCancel(ctx), storage.CopilotScope, key)
		}
	}
	for index, upload := range uploads {
		if upload.Body == nil {
			cleanup()
			return nil, ErrTranscriptAudioNotProvided
		}
		data, err := io.ReadAll(io.LimitReader(upload.Body, maxTranscriptUploadBytes-total+1))
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("read transcript audio: %w", err)
		}
		total += int64(len(data))
		if len(data) == 0 || total > maxTranscriptUploadBytes {
			cleanup()
			return nil, errors.New("transcript audio exceeds the 50 MiB limit or is empty")
		}
		contentType := strings.TrimSpace(upload.ContentType)
		if detected := http.DetectContentType(data); detected != "application/octet-stream" {
			contentType = detected
		}
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		key := userID + "/" + workspaceID + "/transcripts/" + taskID + "/" + fmt.Sprint(index)
		length := int64(len(data))
		if _, err := s.objects.PutObject(ctx, storage.CopilotScope, key, bytes.NewReader(data), storage.PutOptions{ContentType: contentType, ContentLength: &length}); err != nil {
			cleanup()
			return nil, fmt.Errorf("persist transcript audio: %w", err)
		}
		objectKeys = append(objectKeys, key)
		infos = append(infos, map[string]any{
			"key": key, "url": "copilot-object://" + base64.RawURLEncoding.EncodeToString([]byte(key)),
			"mimeType": contentType, "index": index,
		})
	}
	manifest := any(nil)
	if len(input.SliceManifest) > 0 && string(input.SliceManifest) != "null" {
		if err := json.Unmarshal(input.SliceManifest, &manifest); err != nil {
			cleanup()
			return nil, errors.New("invalid transcript slice manifest")
		}
	}
	sourceAudio := any(map[string]any{"blobId": blobID})
	if len(input.SourceAudio) > 0 && string(input.SourceAudio) != "null" {
		if err := json.Unmarshal(input.SourceAudio, &sourceAudio); err != nil {
			cleanup()
			return nil, errors.New("invalid transcript source audio")
		}
		if source, ok := sourceAudio.(map[string]any); ok {
			source["blobId"] = blobID
		}
	}
	quality := any(nil)
	if len(input.Quality) > 0 && string(input.Quality) != "null" {
		if err := json.Unmarshal(input.Quality, &quality); err != nil {
			cleanup()
			return nil, errors.New("invalid transcript quality")
		}
	}
	payload := map[string]any{"infos": infos, "sourceAudio": sourceAudio, "sliceManifest": manifest, "quality": quality}
	snapshot := map[string]any{"infos": infos, "sourceAudio": sourceAudio, "sliceManifest": manifest, "quality": quality, "scopeMode": map[bool]string{true: "personal", false: "canonical"}[personal]}
	publicMeta, _ := json.Marshal(map[string]any{"sourceAudio": sourceAudio, "quality": quality, "sliceManifest": manifest, "version": "transcript-result-v1"})
	protectedResult, _ := json.Marshal(payload)
	inputSnapshot, _ := json.Marshal(snapshot)
	tx, err := s.db.Begin(ctx)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("begin transcript task: %w", err)
	}
	defer tx.Rollback(ctx)
	lockKeyBytes, err := json.Marshal([]string{userID, workspaceID, blobID})
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("encode transcript submission lock: %w", err)
	}
	lockKey := string(lockKeyBytes)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
		cleanup()
		return nil, fmt.Errorf("lock transcript submission: %w", err)
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM ai_transcript_tasks WHERE user_id=$1 AND workspace_id=$2 AND blob_id=$3 AND status IN ('pending','running'))`, userID, workspaceID, blobID).Scan(&exists); err != nil {
		cleanup()
		return nil, fmt.Errorf("check transcript task: %w", err)
	}
	if exists {
		cleanup()
		return nil, ErrTranscriptJobExists
	}
	var task TranscriptTask
	if err := tx.QueryRow(ctx, `INSERT INTO ai_transcript_tasks(
		id,user_id,workspace_id,blob_id,status,recipe_id,recipe_version,dispatch_generation,input_snapshot,public_meta,protected_result,created_at,updated_at)
		VALUES($1,$2,$3,$4,'pending','transcript.audio','v1',$5,$6::jsonb,$7::jsonb,$8::jsonb,clock_timestamp(),clock_timestamp())
		RETURNING id,status,protected_result`, taskID, userID, workspaceID, blobID, generation, inputSnapshot, publicMeta, protectedResult).Scan(&task.ID, &task.Status, &task.ProtectedResult); err != nil {
		cleanup()
		return nil, fmt.Errorf("create transcript task: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit transcript task: %w", err)
	}
	return &task, nil
}

func (s *TranscriptTasks) RetryForUser(ctx context.Context, userID, workspaceID, taskID string) (*TranscriptTask, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("transcript task service is unavailable")
	}
	if s.preflight != nil {
		var snapshot []byte
		if err := s.db.QueryRow(ctx, `SELECT input_snapshot FROM ai_transcript_tasks WHERE id=$1 AND user_id=$2 AND workspace_id=$3`, taskID, userID, workspaceID).Scan(&snapshot); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrTranscriptJobNotFound
			}
			return nil, err
		}
		var input transcriptInputSnapshot
		if json.Unmarshal(snapshot, &input) != nil || (input.ScopeMode != "personal" && input.ScopeMode != "canonical") {
			return nil, errTranscriptInput
		}
		if err := s.preflight(ctx, userID, workspaceID, input.ScopeMode == "personal"); err != nil {
			return nil, err
		}
	}
	generation := uuid.NewString()
	var task TranscriptTask
	err := s.db.QueryRow(ctx, `UPDATE ai_transcript_tasks SET status='pending',dispatch_generation=$4,error_code=NULL,settled_at=NULL,updated_at=clock_timestamp()
		WHERE id=$1 AND user_id=$2 AND workspace_id=$3 AND status='failed'
		RETURNING id,status,protected_result`, taskID, userID, workspaceID, generation).Scan(&task.ID, &task.Status, &task.ProtectedResult)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			var exists bool
			if checkErr := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM ai_transcript_tasks WHERE id=$1 AND user_id=$2 AND workspace_id=$3)`, taskID, userID, workspaceID).Scan(&exists); checkErr != nil {
				return nil, checkErr
			}
			if !exists {
				return nil, ErrTranscriptJobNotFound
			}
			return nil, ErrTranscriptRetryNotAllowed
		}
		return nil, fmt.Errorf("retry transcript task: %w", err)
	}
	return &task, nil
}

func (s *TranscriptTasks) GetForUser(ctx context.Context, userID, workspaceID string, taskID, blobID *string) (*TranscriptTask, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("transcript task service is unavailable")
	}
	if (taskID == nil || *taskID == "") && (blobID == nil || *blobID == "") {
		return nil, nil
	}
	var task TranscriptTask
	err := s.db.QueryRow(ctx, `SELECT id,status,protected_result
		FROM ai_transcript_tasks
		WHERE user_id=$1 AND workspace_id=$2
		  AND ($3::text IS NULL OR id=$3)
		  AND ($4::text IS NULL OR blob_id=$4)
		ORDER BY created_at DESC
		LIMIT 1`, userID, workspaceID, taskID, blobID).Scan(&task.ID, &task.Status, &task.ProtectedResult)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &task, nil
}

func (s *TranscriptTasks) SettleForUser(ctx context.Context, userID, workspaceID, taskID string) (*TranscriptTask, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("transcript task service is unavailable")
	}
	var task TranscriptTask
	err := s.db.QueryRow(ctx, `UPDATE ai_transcript_tasks
		SET status='settled',settled_at=coalesce(settled_at,clock_timestamp()),updated_at=clock_timestamp()
		WHERE id=$1 AND user_id=$2 AND workspace_id=$3
		RETURNING id,status,protected_result`, taskID, userID, workspaceID).
		Scan(&task.ID, &task.Status, &task.ProtectedResult)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &task, nil
}

func (s *TranscriptTasks) ClaimNext(ctx context.Context) (*TranscriptExecutionTask, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("transcript task service is unavailable")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transcript task claim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	task := &TranscriptExecutionTask{}
	err = tx.QueryRow(ctx, `SELECT id,user_id,workspace_id,blob_id,
		COALESCE(action_run_id,''),COALESCE(dispatch_generation,''),input_snapshot
		FROM ai_transcript_tasks
		WHERE status='pending'
		ORDER BY created_at,id
		FOR UPDATE SKIP LOCKED
		LIMIT 1`).Scan(&task.ID, &task.UserID, &task.WorkspaceID, &task.BlobID, &task.RetryOf, &task.DispatchGeneration, &task.InputSnapshot)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim transcript task: %w", err)
	}
	if task.DispatchGeneration == "" {
		task.DispatchGeneration = uuid.NewString()
	}
	attempt := 1
	if task.RetryOf != "" {
		var previousAttempt int
		if err := tx.QueryRow(ctx, `SELECT attempt FROM ai_action_runs
			WHERE id=$1 AND user_id=$2 AND workspace_id=$3 AND action_id='transcript.audio' AND action_version='v1'`,
			task.RetryOf, task.UserID, task.WorkspaceID).Scan(&previousAttempt); err != nil {
			return nil, fmt.Errorf("load transcript retry source: %w", err)
		}
		attempt = previousAttempt + 1
	}
	task.ActionRunID = uuid.NewString()
	if _, err := tx.Exec(ctx, `INSERT INTO ai_action_runs(
		id,user_id,workspace_id,action_id,action_version,status,attempt,retry_of,input_snapshot,created_at,updated_at)
		VALUES($1,$2,$3,'transcript.audio','v1','running',$4,NULLIF($5,''),$6::jsonb,clock_timestamp(),clock_timestamp())`,
		task.ActionRunID, task.UserID, task.WorkspaceID, attempt, task.RetryOf, task.InputSnapshot); err != nil {
		return nil, fmt.Errorf("create transcript action run: %w", err)
	}
	tag, err := tx.Exec(ctx, `UPDATE ai_transcript_tasks SET status='running',action_run_id=$2,
		dispatch_generation=$3,updated_at=clock_timestamp() WHERE id=$1 AND status='pending'`,
		task.ID, task.ActionRunID, task.DispatchGeneration)
	if err != nil {
		return nil, fmt.Errorf("mark transcript task running: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return nil, errors.New("transcript task claim lost its locked row")
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit transcript task claim: %w", err)
	}
	return task, nil
}

func (s *TranscriptTasks) CompleteDispatch(ctx context.Context, task TranscriptExecutionTask, result json.RawMessage) (bool, error) {
	if !json.Valid(result) || len(result) == 0 || string(result) == "null" {
		return false, errors.New("invalid transcript execution result")
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(result, &payload); err != nil {
		return false, errors.New("invalid transcript execution result")
	}
	publicMeta := map[string]json.RawMessage{"version": json.RawMessage(`"transcript-result-v1"`)}
	for _, key := range []string{"sourceAudio", "quality", "sliceManifest"} {
		if value, ok := payload[key]; ok {
			publicMeta[key] = value
		}
	}
	encodedMeta, err := json.Marshal(publicMeta)
	if err != nil {
		return false, fmt.Errorf("encode transcript public metadata: %w", err)
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin transcript completion: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `UPDATE ai_transcript_tasks SET status='ready',public_meta=$5::jsonb,protected_result=$6::jsonb,error_code=NULL,updated_at=clock_timestamp()
		WHERE id=$1 AND dispatch_generation=$2 AND status='running' AND user_id=$3 AND action_run_id=$4`, task.ID, task.DispatchGeneration, task.UserID, task.ActionRunID, encodedMeta, result)
	if err != nil {
		return false, fmt.Errorf("complete transcript task: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return false, nil
	}
	tag, err = tx.Exec(ctx, `UPDATE ai_action_runs SET status='succeeded',result=$2::jsonb,error_code=NULL,updated_at=clock_timestamp()
		WHERE id=$1 AND status='running'`, task.ActionRunID, result)
	if err != nil {
		return false, fmt.Errorf("complete transcript action run: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return false, errors.New("transcript action run is not running")
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit transcript completion: %w", err)
	}
	return true, nil
}

func (s *TranscriptTasks) FailDispatch(ctx context.Context, task TranscriptExecutionTask, errorCode string) (bool, error) {
	if !validTranscriptErrorCode(errorCode) {
		errorCode = "transcript_provider_error"
	}
	if len(errorCode) > 128 {
		errorCode = errorCode[:128]
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin transcript failure: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `UPDATE ai_transcript_tasks SET status='failed',error_code=$5,updated_at=clock_timestamp()
		WHERE id=$1 AND dispatch_generation=$2 AND status='running' AND user_id=$3 AND action_run_id=$4`, task.ID, task.DispatchGeneration, task.UserID, task.ActionRunID, errorCode)
	if err != nil {
		return false, fmt.Errorf("fail transcript task: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return false, nil
	}
	tag, err = tx.Exec(ctx, `UPDATE ai_action_runs SET status='failed',error_code=$2,updated_at=clock_timestamp()
		WHERE id=$1 AND status='running'`, task.ActionRunID, errorCode)
	if err != nil {
		return false, fmt.Errorf("fail transcript action run: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return false, errors.New("transcript action run is not running")
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit transcript failure: %w", err)
	}
	return true, nil
}

func validTranscriptErrorCode(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '_' && char != '-' {
			return false
		}
	}
	return true
}

func (s *TranscriptTasks) FailStaleRunning(ctx context.Context, before time.Time) ([]TranscriptTaskStatusChange, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("transcript task service is unavailable")
	}
	rows, err := s.db.Query(ctx, `WITH stale AS (
		UPDATE ai_transcript_tasks
		SET status='failed',error_code='transcript_dispatch_timed_out',updated_at=clock_timestamp()
		WHERE status='running' AND updated_at<$1
		RETURNING id,workspace_id,status,error_code,action_run_id
	), failed_runs AS (
		UPDATE ai_action_runs AS run SET status='failed',error_code='transcript_dispatch_timed_out',updated_at=clock_timestamp()
		FROM stale WHERE run.id=stale.action_run_id AND run.status='running'
		RETURNING run.id
	)
	SELECT id,workspace_id,status,error_code FROM stale`, before)
	if err != nil {
		return nil, fmt.Errorf("fail stale transcript tasks: %w", err)
	}
	defer rows.Close()
	changes := make([]TranscriptTaskStatusChange, 0)
	for rows.Next() {
		var change TranscriptTaskStatusChange
		if err := rows.Scan(&change.ID, &change.WorkspaceID, &change.Status, &change.ErrorCode); err != nil {
			return nil, fmt.Errorf("read stale transcript task result: %w", err)
		}
		changes = append(changes, change)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read stale transcript tasks: %w", err)
	}
	return changes, nil
}
