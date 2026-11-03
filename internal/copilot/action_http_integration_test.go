// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestStructuredCopilotActionStreamAndRetry(t *testing.T) {
	databaseURL := os.Getenv("NOTES_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set NOTES_TEST_DATABASE_URL to a disposable local notes_test database")
	}
	parsed, err := url.Parse(databaseURL)
	if err != nil || (parsed.Hostname() != "localhost" && parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "::1") {
		t.Fatal("Copilot action integration test requires a loopback PostgreSQL URL")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect to integration PostgreSQL: %v", err)
	}
	schema := "copilot_action_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	poolConfig.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, `SET search_path TO `+schema)
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		if _, err := admin.Exec(ctx, `DROP SCHEMA `+schema+` CASCADE`); err != nil {
			t.Errorf("drop test schema: %v", err)
		}
		admin.Close()
	})
	for _, statement := range []string{
		`CREATE TYPE "AiPromptRole" AS ENUM ('system','assistant','user')`,
		`CREATE TABLE workspaces(id text PRIMARY KEY)`,
		`CREATE TABLE ai_sessions_metadata(id varchar(36) PRIMARY KEY,user_id varchar(36) NOT NULL,workspace_id varchar(36) NOT NULL,doc_id varchar(36),parent_session_id varchar(36),prompt_name varchar(32) NOT NULL,prompt_action varchar(32) DEFAULT '',pinned boolean NOT NULL DEFAULT false,title varchar,created_at timestamptz NOT NULL DEFAULT now(),updated_at timestamptz NOT NULL DEFAULT now(),"messageCost" integer NOT NULL DEFAULT 0,deleted_at timestamptz,focus jsonb)`,
		`CREATE TABLE ai_sessions_messages(id varchar(36) PRIMARY KEY,session_id varchar(36) NOT NULL,compat_submission_id varchar,role "AiPromptRole" NOT NULL,content text NOT NULL,attachments json,params json,"streamObjects" json,scope_snapshot jsonb,created_at timestamptz NOT NULL DEFAULT now(),updated_at timestamptz NOT NULL DEFAULT now())`,
		`CREATE TABLE ai_prompts_metadata(id serial PRIMARY KEY,name varchar(32) NOT NULL)`,
		`CREATE TABLE ai_prompts_messages(prompt_id integer NOT NULL,idx integer NOT NULL,role "AiPromptRole" NOT NULL,content text NOT NULL)`,
		`CREATE TABLE ai_action_runs(id varchar PRIMARY KEY,user_id varchar NOT NULL,workspace_id varchar NOT NULL,doc_id varchar,session_id varchar,user_message_id varchar,compat_submission_id varchar,assistant_message_id varchar,action_id varchar NOT NULL,action_version varchar NOT NULL,status varchar NOT NULL,attempt integer NOT NULL DEFAULT 1,retry_of varchar,input_snapshot json,result json,artifacts json,result_summary text,error_code varchar,trace json,created_at timestamptz NOT NULL DEFAULT now(),updated_at timestamptz NOT NULL)`,
		`CREATE TABLE ai_usage_events(id text PRIMARY KEY,workspace_id text,local_workspace_id text,user_id text,provider text,provider_source text,billing_unit_id text,task_id text,feature_kind text,model text,session_id text,created_at timestamptz NOT NULL)`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("set up Copilot action schema: %v", err)
		}
	}
	for _, statement := range []string{
		`INSERT INTO workspaces(id) VALUES('workspace-1')`,
		`INSERT INTO ai_prompts_metadata(id,name) VALUES(1,'slides.outline')`,
		`INSERT INTO ai_prompts_messages(prompt_id,idx,role,content) VALUES(1,0,'system','Create a slide outline.'),(1,1,'assistant','Output Language: {{language}}. Except keywords.'),(1,2,'user','{{content}}')`,
		`INSERT INTO ai_sessions_metadata(id,user_id,workspace_id,doc_id,prompt_name) VALUES('session-1','user-1','workspace-1','doc-1','Chat With AFFiNE AI')`,
		`INSERT INTO ai_sessions_messages(id,session_id,role,content,attachments,params) VALUES('message-1','session-1','user','Plan a launch deck','[]','{"language":"Chinese"}')`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}

	providerFails := false
	var providerRequest struct {
		ResponseFormat struct {
			Type       string `json:"type"`
			JSONSchema struct {
				Strict bool `json:"strict"`
			} `json:"json_schema"`
		} `json:"response_format"`
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&providerRequest); err != nil {
			t.Errorf("decode provider request: %v", err)
		}
		if providerFails {
			providerFails = false
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		resultJSON, _ := json.Marshal(map[string]string{"result": "# Launch\n## Plan"})
		chunkJSON, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]string{"content": string(resultJSON)}}}})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", chunkJSON)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer provider.Close()

	store := NewStore(pool)
	denyAccess := false
	handler := NewChatHTTPHandler(pool, config.Config{
		FydeOSManagedCopilot: true, FydeOSManagedCopilotAPIKey: "test-secret",
		FydeOSManagedCopilotBaseURL: provider.URL + "/v1", FydeOSManagedCopilotModel: "test-model",
	}, chatTestAuthenticator{principal: &auth.Principal{User: auth.User{ID: "user-1"}}}, store, func(context.Context, string, string, *string) error {
		if denyAccess {
			return errors.New("document access denied")
		}
		return nil
	}, chatTestFeature{enabled: true})
	quotaPublisher := &chatQuotaPublisherRecorder{}
	handler.SetQuotaStatePublisher(quotaPublisher)
	serve := func(query string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, "/api/copilot/actions/session-1/stream?"+query, nil)
		request.SetPathValue("sessionId", "session-1")
		response := httptest.NewRecorder()
		handler.ActionHandler().ServeHTTP(response, request)
		return response
	}
	first := serve("messageId=message-1&actionId=slides.outline&actionVersion=v1")
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), "data: # Launch\ndata: ## Plan") || !strings.Contains(first.Body.String(), `"type":"action_start"`) {
		t.Fatalf("first action status=%d body=%q", first.Code, first.Body.String())
	}
	if providerRequest.ResponseFormat.Type != "json_schema" || !providerRequest.ResponseFormat.JSONSchema.Strict || len(providerRequest.Messages) != 3 || !strings.Contains(providerRequest.Messages[1].Content, "Chinese") || providerRequest.Messages[2].Content != "Plan a launch deck" {
		t.Fatalf("provider request=%+v", providerRequest)
	}
	var firstRunID string
	if err := pool.QueryRow(ctx, `SELECT id FROM ai_action_runs WHERE status='succeeded'`).Scan(&firstRunID); err != nil {
		t.Fatal(err)
	}
	var assistantContent, firstStatus string
	if err := pool.QueryRow(ctx, `SELECT message.content,run.status FROM ai_sessions_messages message JOIN ai_action_runs run ON run.assistant_message_id=message.id WHERE run.id=$1`, firstRunID).Scan(&assistantContent, &firstStatus); err != nil || assistantContent != "# Launch\n## Plan" || firstStatus != "succeeded" {
		t.Fatalf("assistant result=%q run status=%q err=%v", assistantContent, firstStatus, err)
	}
	var featureKind string
	if err := pool.QueryRow(ctx, `SELECT feature_kind FROM ai_usage_events WHERE provider_source='fydeos_managed_quota'`).Scan(&featureKind); err != nil || featureKind != "action" {
		t.Fatalf("managed usage feature=%q err=%v", featureKind, err)
	}
	denyAccess = true
	denied := serve("messageId=message-1&actionId=slides.outline&actionVersion=v1")
	denyAccess = false
	if !strings.Contains(denied.Body.String(), "Copilot access is denied") {
		t.Fatalf("denied action response=%q", denied.Body.String())
	}
	var runCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ai_action_runs`).Scan(&runCount); err != nil || runCount != 1 {
		t.Fatalf("denied action created run count=%d err=%v", runCount, err)
	}

	providerFails = true
	failed := serve("messageId=message-1&actionId=slides.outline&actionVersion=v1")
	if !strings.Contains(failed.Body.String(), "The Copilot provider request failed") {
		t.Fatalf("failed action response=%q", failed.Body.String())
	}
	var failedRunID, failedStatus, failedCode string
	if err := pool.QueryRow(ctx, `SELECT id,status,error_code FROM ai_action_runs WHERE status='failed' ORDER BY created_at DESC LIMIT 1`).Scan(&failedRunID, &failedStatus, &failedCode); err != nil || failedStatus != "failed" || failedCode != "action_provider_error" {
		t.Fatalf("failed run=%q status=%q code=%q err=%v", failedRunID, failedStatus, failedCode, err)
	}
	retried := serve("messageId=message-1&actionId=slides.outline&actionVersion=v1&retry=true&runId=" + url.QueryEscape(failedRunID))
	if retried.Code != http.StatusOK || !strings.Contains(retried.Body.String(), "data: # Launch\ndata: ## Plan") {
		t.Fatalf("retried action status=%d body=%q", retried.Code, retried.Body.String())
	}
	var retryOf string
	var retryAttempt int
	if err := pool.QueryRow(ctx, `SELECT retry_of,attempt FROM ai_action_runs WHERE status='succeeded' AND id<>$1 ORDER BY created_at DESC LIMIT 1`, firstRunID).Scan(&retryOf, &retryAttempt); err != nil || retryOf != failedRunID || retryAttempt != 2 {
		t.Fatalf("retry parent=%q attempt=%d err=%v", retryOf, retryAttempt, err)
	}
	if used, err := store.CountFydeosManagedRequests(ctx, "user-1", time.Now()); err != nil || used != 2 {
		t.Fatalf("managed usage count after action success, failed release, and retry=%d err=%v", used, err)
	}
	if len(quotaPublisher.users) != 4 {
		t.Fatalf("quota state invalidations=%v, want reserve/release for each attempt", quotaPublisher.users)
	}
	testImageFilterActions(t, pool, store)

}
