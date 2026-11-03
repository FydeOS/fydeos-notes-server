// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/byok"
	"github.com/google/uuid"
)

type TranscriptTokenUsage struct {
	Prompt     int64
	Completion int64
	Total      int64
	Cached     int64
}

type TranscriptUsageRecorder interface {
	RecordTranscriptUsage(context.Context, TranscriptExecutionTask, byok.ChatTarget, *TranscriptTokenUsage) error
}

type TranscriptUsageLedger struct{ db DB }

func NewTranscriptUsageLedger(db DB) *TranscriptUsageLedger { return &TranscriptUsageLedger{db: db} }

// A nil usage records route selection (zero tokens). Token events are separate
// rows, matching the legacy runtime's selection/usage distinction. Recording
// errors are handled by the caller as best effort and never resend a paid call.
func (s *TranscriptUsageLedger) RecordTranscriptUsage(ctx context.Context, task TranscriptExecutionTask, target byok.ChatTarget, usage *TranscriptTokenUsage) error {
	if target.Source == "affine_cloud" {
		return nil
	}
	if target.Source != "byok_server" && target.Source != "fydeos_managed" {
		return errors.New("invalid transcript usage source")
	}
	if target.Source == "fydeos_managed" && usage == nil {
		return nil
	}
	var input transcriptInputSnapshot
	if json.Unmarshal(task.InputSnapshot, &input) != nil || (input.ScopeMode != "personal" && input.ScopeMode != "canonical") {
		return errTranscriptInput
	}
	personal := input.ScopeMode == "personal"
	tokens := TranscriptTokenUsage{}
	if usage != nil {
		tokens = *usage
	}
	if !validTranscriptUsage(tokens) {
		return errors.New("invalid transcript usage counters")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if usage == nil && target.Source == "byok_server" && target.ProfileID != "" {
		if _, err := tx.Exec(ctx, `UPDATE ai_workspace_byok_configs SET last_used_at=$3,updated_at=$3 WHERE workspace_id=$1 AND id=$2`, task.WorkspaceID, target.ProfileID, time.Now()); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO ai_usage_events(id,workspace_id,local_workspace_id,user_id,provider,provider_source,feature_kind,model,task_id,action_id,billing_unit_id,prompt_tokens,completion_tokens,total_tokens,cached_tokens,created_at)
 VALUES($1,$2,$3,$4,$5,$6,'transcript',$7,$8,'transcript.audio',$8,$9,$10,$11,$12,clock_timestamp())`, uuid.NewString(), quotaWorkspaceID(task.WorkspaceID, personal), quotaLocalWorkspaceID(task.WorkspaceID, personal), task.UserID, target.Provider, target.Source, target.ModelID, task.ID, tokens.Prompt, tokens.Completion, tokens.Total, tokens.Cached)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func validTranscriptUsage(u TranscriptTokenUsage) bool {
	for _, v := range []int64{u.Prompt, u.Completion, u.Total, u.Cached} {
		if v < 0 || v > math.MaxInt32 {
			return false
		}
	}
	return true
}

func transcriptProviderUsage(body []byte, format string) (*TranscriptTokenUsage, error) {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(body, &envelope) != nil {
		return nil, errTranscriptResult
	}
	field := "usage"
	if format == "gemini" {
		field = "usageMetadata"
	}
	raw, ok := envelope[field]
	if !ok || string(raw) == "null" {
		return nil, nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, errTranscriptResult
	}
	var parseErr error
	count := func(name string) int64 {
		raw, ok := fields[name]
		if !ok {
			return 0
		}
		var n int64
		if json.Unmarshal(raw, &n) != nil || n < 0 || n > math.MaxInt32 {
			parseErr = errTranscriptResult
		}
		return n
	}
	nested := func(parent, key string) int64 {
		raw, ok := fields[parent]
		if !ok || string(raw) == "null" {
			return 0
		}
		var details map[string]json.RawMessage
		if json.Unmarshal(raw, &details) != nil {
			parseErr = errTranscriptResult
			return 0
		}
		value, ok := details[key]
		if !ok {
			return 0
		}
		var n int64
		if json.Unmarshal(value, &n) != nil || n < 0 || n > math.MaxInt32 {
			parseErr = errTranscriptResult
		}
		return n
	}
	var usage TranscriptTokenUsage
	switch format {
	case "gemini":
		usage.Prompt = count("promptTokenCount")
		usage.Completion = count("candidatesTokenCount")
		usage.Total = count("totalTokenCount")
		usage.Cached = count("cachedContentTokenCount")
	case "openai-chat":
		usage.Prompt = count("prompt_tokens")
		usage.Completion = count("completion_tokens")
		usage.Total = count("total_tokens")
		usage.Cached = nested("prompt_tokens_details", "cached_tokens")
	case "openai-responses":
		usage.Prompt = count("input_tokens")
		usage.Completion = count("output_tokens")
		usage.Total = count("total_tokens")
		usage.Cached = nested("input_tokens_details", "cached_tokens")
	case "anthropic":
		usage.Prompt = count("input_tokens") + count("cache_creation_input_tokens") + count("cache_read_input_tokens")
		usage.Completion = count("output_tokens")
		usage.Total = usage.Prompt + usage.Completion
		usage.Cached = count("cache_read_input_tokens")
	default:
		return nil, errTranscriptResult
	}
	totalKey := "total_tokens"
	if format == "gemini" {
		totalKey = "totalTokenCount"
	}
	if _, supplied := fields[totalKey]; !supplied && format != "anthropic" {
		usage.Total = usage.Prompt + usage.Completion
	}
	if parseErr != nil || !validTranscriptUsage(usage) {
		return nil, errTranscriptResult
	}
	return &usage, nil
}
