// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/byok"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/copilot"
)

type transcriptUnavailableRoute struct{ err error }

func (s transcriptUnavailableRoute) ResolveServerTarget(context.Context, string, string, string) (byok.ChatTarget, error) {
	return byok.ChatTarget{}, s.err
}

func TestTranscriptCloudFallbackQuotaConfigurationAndBYOKPriority(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	fixture := seedGraphQLFixture(t, ctx, pool)
	if _, err := pool.Exec(ctx, `ALTER TABLE ai_sessions_metadata ADD COLUMN id text, ADD COLUMN "messageCost" integer NOT NULL DEFAULT 0, ADD COLUMN prompt_action text DEFAULT ''`); err != nil {
		t.Fatal(err)
	}
	for _, ddl := range []string{
		`CREATE TABLE ai_action_runs(id text PRIMARY KEY,user_id text,session_id text,action_id text,status text)`,
		`CREATE TABLE ai_transcript_tasks(id text PRIMARY KEY,user_id text,status text)`,
		`CREATE TABLE ai_sessions_messages(id text PRIMARY KEY,session_id text,role text)`,
		`CREATE TABLE ai_usage_events(user_id text,provider_source text,feature_kind text,billing_unit_id text)`,
	} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatal(err)
		}
	}
	setProfiles := func(raw string) {
		if _, err := pool.Exec(ctx, `INSERT INTO app_configs(id,value) VALUES('copilot.providers.profiles',$1::jsonb) ON CONFLICT(id) DO UPDATE SET value=EXCLUDED.value`, raw); err != nil {
			t.Fatal(err)
		}
	}
	setProfiles(`[{"id":"audio","type":"gemini","models":["gemini-3.8-flash"],"config":{"apiKey":"test-key"}},{"id":"summary","type":"openai","models":["gpt-6-luna"],"config":{"apiKey":"summary-key"}},{"id":"embedding","type":"gemini","models":["gemini-embedding-001"],"config":{"apiKey":"embedding-key"}}]`)
	cfg := config.Config{Deployment: "affine"}
	routes := copilot.NewTranscriptRoutes(pool, cfg, transcriptUnavailableRoute{byok.ErrNoCompatibleServerTarget})
	target, err := routes.ResolveServerTarget(ctx, fixture.workspaceID, fixture.userID, "transcript")
	if err != nil {
		t.Fatal(err)
	}
	if target.Source != "affine_cloud" || target.Provider != "gemini" || target.ModelID != "gemini-3.8-flash" || target.BaseURL != "https://generativelanguage.googleapis.com/v1beta" {
		t.Fatal("wrong managed audio target")
	}
	target.Credential.Destroy()
	target, err = routes.ResolveServerTarget(ctx, fixture.workspaceID, fixture.userID, "structured")
	if err != nil {
		t.Fatal(err)
	}
	if target.ModelID != "gpt-6-luna" || target.Endpoint.Dialect != "responses" {
		t.Fatal("wrong summary route")
	}
	target.Credential.Destroy()
	target, err = routes.ResolveServerTarget(ctx, fixture.workspaceID, fixture.userID, "embedding")
	if err != nil {
		t.Fatal(err)
	}
	if target.Source != "affine_cloud" || target.Provider != "gemini" || target.ModelID != "gemini-embedding-001" {
		t.Fatalf("wrong managed embedding target: source=%q provider=%q model=%q", target.Source, target.Provider, target.ModelID)
	}
	target.Credential.Destroy()
	setProfiles(`[{"id":"embedding","type":"openai","models":["text-embedding-3-small"],"config":{"apiKey":"embedding-key"}}]`)
	target, err = routes.ResolveServerTarget(ctx, fixture.workspaceID, fixture.userID, "embedding")
	if err != nil {
		t.Fatal(err)
	}
	if target.Source != "affine_cloud" || target.Provider != "openai" || target.ModelID != "text-embedding-3-small" {
		t.Fatalf("wrong managed OpenAI embedding target: source=%q provider=%q model=%q", target.Source, target.Provider, target.ModelID)
	}
	target.Credential.Destroy()
	setProfiles(`[{"id":"audio","type":"gemini","models":["gemini-3.8-flash"],"config":{"apiKey":"test-key"}},{"id":"summary","type":"openai","models":["gpt-6-luna"],"config":{"apiKey":"summary-key"}}]`)
	// A broken selected BYOK target cannot silently switch providers.
	refused := copilot.NewTranscriptRoutes(pool, cfg, transcriptUnavailableRoute{byok.ErrTargetUnavailable})
	if _, err := refused.ResolveServerTarget(ctx, fixture.workspaceID, fixture.userID, "transcript"); !errors.Is(err, byok.ErrTargetUnavailable) {
		t.Fatalf("selected target failure fell back: %v", err)
	}
	// Settled transcripts exhaust the same quota as conversation messages.
	if _, err := pool.Exec(ctx, `INSERT INTO ai_transcript_tasks SELECT 'settled-'||n,$1,'settled' FROM generate_series(1,10) n`, fixture.userID); err != nil {
		t.Fatal(err)
	}
	if target, err := routes.ResolveServerTarget(ctx, fixture.workspaceID, fixture.userID, "transcript"); !errors.Is(err, byok.ErrTargetUnavailable) {
		target.Credential.Destroy()
		t.Fatalf("settled transcript quota was ignored: %v", err)
	}
	// Duplicate BYOK selection/token events must exempt each billing unit once.
	if _, err := pool.Exec(ctx, `INSERT INTO ai_usage_events SELECT $1,'byok_server','transcript','settled-'||n FROM generate_series(1,10) n CROSS JOIN generate_series(1,2) duplicates`, fixture.userID); err != nil {
		t.Fatal(err)
	}
	target, err = routes.ResolveServerTarget(ctx, fixture.workspaceID, fixture.userID, "transcript")
	if err != nil {
		t.Fatalf("BYOK transcripts were charged: %v", err)
	}
	target.Credential.Destroy()
	if _, err := pool.Exec(ctx, `INSERT INTO ai_sessions_metadata(workspace_id,user_id,"messageCost") VALUES($1,$2,10)`, fixture.workspaceID, fixture.userID); err != nil {
		t.Fatal(err)
	}
	if _, err := routes.ResolveServerTarget(ctx, fixture.workspaceID, fixture.userID, "transcript"); !errors.Is(err, byok.ErrTargetUnavailable) {
		t.Fatalf("quota exhausted route=%v", err)
	}
	// The public quota view and route gate use the same complete accounting.
	if _, err := pool.Exec(ctx, `INSERT INTO ai_sessions_metadata(id,workspace_id,user_id,prompt_action,"messageCost") VALUES('legacy-action',$2,$1,'summarize',90),('run-session',$2,$1,'summarize',90),('regular',$2,$1,'',2);
`, fixture.userID, fixture.workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ai_action_runs(id,user_id,session_id,action_id,status) VALUES('success',$1,'run-session','summarize','succeeded'),('failed',$1,NULL,'summarize','failed'),('audio-step',$1,NULL,'transcript.audio.segment','succeeded'),('other-user','other',NULL,'summarize','succeeded')`, fixture.userID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ai_sessions_messages VALUES('regular-message','regular','user'),('assistant-message','regular','assistant');`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ai_usage_events VALUES($1,'byok_local','chat','regular-message'),($1,'byok_server','image','regular-message'),($1,'byok_server','action','success'),($1,'byok_local','chat','assistant-message'),('other','byok_local','chat','regular-message')`, fixture.userID); err != nil {
		t.Fatal(err)
	}
	used, err := copilot.NewStore(pool).UserMessageCost(ctx, fixture.userID)
	// 10 initial chat + 2 regular + 1 legacy + 1 succeeded - 1 message - 1 action;
	// all 10 settled transcripts are BYOK-exempt. Assistant/other-user rows do not exempt.
	if err != nil || used != 12 {
		t.Fatalf("complete quota used=%d err=%v", used, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO entitlements(target_type,target_id,source,plan,status) VALUES('user',$1,'admin_grant','ai','active')`, fixture.userID); err != nil {
		t.Fatal(err)
	}
	target, err = routes.ResolveServerTarget(ctx, fixture.workspaceID, fixture.userID, "transcript")
	if err != nil {
		t.Fatal(err)
	}
	target.Credential.Destroy()
	setProfiles(`[{"id":"one","type":"gemini","models":["gemini-3.8-flash"],"config":{"apiKey":"key"}},{"id":"two","type":"gemini","models":["gemini-3.8-flash"],"config":{"apiKey":"key"}}]`)
	if _, err := routes.ResolveServerTarget(ctx, fixture.workspaceID, fixture.userID, "transcript"); err == nil {
		t.Fatal("duplicate managed model accepted")
	}
	setProfiles(`[{"id":"audio","type":"gemini","enabled":false,"models":["gemini-3.8-flash"],"config":{"apiKey":"key"}}]`)
	if _, err := routes.ResolveServerTarget(ctx, fixture.workspaceID, fixture.userID, "transcript"); !errors.Is(err, byok.ErrTargetUnavailable) {
		t.Fatal("disabled profile accepted")
	}
	setProfiles(`[{"id":"audio","type":"gemini","models":["gemini-3.8-flash"],"config":{"apiKey":"key","baseURL":"http://public.example/v1"}}]`)
	if _, err := routes.ResolveServerTarget(ctx, fixture.workspaceID, fixture.userID, "transcript"); err == nil {
		t.Fatal("plaintext managed credential endpoint accepted")
	}
}

func TestTranscriptUsageLedgerScopeAndAggregation(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	fixture := seedGraphQLFixture(t, ctx, pool)
	applyBYOKMigration(t, pool)
	if _, err := pool.Exec(ctx, `INSERT INTO app_configs(id,value) VALUES ('copilot.enabled','true'::jsonb) ON CONFLICT (id) DO UPDATE SET value=EXCLUDED.value`); err != nil {
		t.Fatal(err)
	}
	store, handler := newBYOKGraphQLHandler(t, pool)
	if _, err := pool.Exec(ctx, `INSERT INTO entitlements(target_type,target_id,source,plan,status) VALUES('user',$1,'admin_grant','pro','active')`, fixture.userID); err != nil {
		t.Fatal(err)
	}
	created := byokGraphData(t, createByokProfile(t, handler, fixture.workspaceID, fixture.sessionID, fixture.userID, "Transcript usage", "secret"))["createWorkspaceByokProfile"].(map[string]any)
	target, err := store.ResolveChatTarget(ctx, fixture.workspaceID, fixture.userID, created["profileId"].(string), "gpt-6.1-sol")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Credential.Destroy()
	if _, err := pool.Exec(ctx, `CREATE TABLE ai_usage_events(id text PRIMARY KEY,workspace_id text REFERENCES workspaces(id) ON DELETE CASCADE,local_workspace_id text,user_id text,provider text,provider_source text,feature_kind text,model text,task_id text,action_id text,billing_unit_id text,prompt_tokens integer,completion_tokens integer,total_tokens integer,cached_tokens integer,created_at timestamptz,
 CHECK ((workspace_id IS NOT NULL AND local_workspace_id IS NULL) OR (workspace_id IS NULL AND local_workspace_id IS NOT NULL AND user_id IS NOT NULL)))`); err != nil {
		t.Fatal(err)
	}
	ledger := copilot.NewTranscriptUsageLedger(pool)
	task := copilot.TranscriptExecutionTask{ID: "task", UserID: fixture.userID, WorkspaceID: fixture.workspaceID, InputSnapshot: json.RawMessage(`{"scopeMode":"canonical"}`)}
	if err := ledger.RecordTranscriptUsage(ctx, task, target, nil); err != nil {
		t.Fatal(err)
	}
	usage := copilot.TranscriptTokenUsage{Prompt: 10, Completion: 5, Total: 15, Cached: 4}
	if err := ledger.RecordTranscriptUsage(ctx, task, target, &usage); err != nil {
		t.Fatal(err)
	}
	var rows int
	var total int64
	if err := pool.QueryRow(ctx, `SELECT count(*),sum(total_tokens) FROM ai_usage_events WHERE workspace_id=$1 AND user_id=$2 AND feature_kind='transcript' AND task_id='task' AND billing_unit_id='task' AND action_id='transcript.audio'`, fixture.workspaceID, fixture.userID).Scan(&rows, &total); err != nil || rows != 2 || total != 15 {
		t.Fatalf("usage rows=%d total=%d err=%v", rows, total, err)
	}
	var touched bool
	if err := pool.QueryRow(ctx, `SELECT last_used_at IS NOT NULL FROM ai_workspace_byok_configs WHERE id=$1`, target.ProfileID).Scan(&touched); err != nil || !touched {
		t.Fatal("profile was not marked used")
	}
	points, err := store.AggregateUsageByDay(ctx, fixture.workspaceID, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil || len(points) != 1 || points[0].FeatureKind != "transcript" || points[0].TotalTokens != 15 {
		t.Fatalf("aggregate=%#v err=%v", points, err)
	}
	task.WorkspaceID = "personal-workspace"
	task.InputSnapshot = json.RawMessage(`{"scopeMode":"personal"}`)
	if err := ledger.RecordTranscriptUsage(ctx, task, target, &usage); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ai_usage_events WHERE workspace_id IS NULL AND local_workspace_id='personal-workspace' AND user_id=$1`, fixture.userID).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("personal usage=%d %v", rows, err)
	}
}
