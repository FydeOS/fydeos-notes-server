// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package copilot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/byok"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/storage"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type chatTestAuthenticator struct{ principal *auth.Principal }

func (a chatTestAuthenticator) FromRequest(context.Context, *http.Request) (*auth.Principal, error) {
	return a.principal, nil
}

type chatTestFeature struct{ enabled bool }

func (f chatTestFeature) CopilotEnabled(context.Context) (bool, error) { return f.enabled, nil }

type chatTestBYOKResolver struct{ target byok.ChatTarget }

func (r chatTestBYOKResolver) ResolveChatTarget(context.Context, string, string, string, string) (byok.ChatTarget, error) {
	return r.target, nil
}

type chatQuotaPublisherRecorder struct{ users []string }

func (r *chatQuotaPublisherRecorder) PublishUserQuotaStateChanged(userID, _ string) {
	r.users = append(r.users, userID)
}

type chatToolExecutorRecorder struct {
	userID      string
	workspaceID string
	name        string
	input       json.RawMessage
}

func (r *chatToolExecutorRecorder) Execute(_ context.Context, userID, workspaceID, name string, input json.RawMessage) (any, error) {
	r.userID, r.workspaceID, r.name = userID, workspaceID, name
	r.input = append(r.input[:0], input...)
	return map[string]any{"hits": []any{map[string]string{"doc_id": "readable"}}}, nil
}

func TestManagedCopilotStreamPersistsAssistantReply(t *testing.T) {
	databaseURL := os.Getenv("NOTES_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set NOTES_TEST_DATABASE_URL to a disposable local notes_test database")
	}
	parsed, err := url.Parse(databaseURL)
	if err != nil || (parsed.Hostname() != "localhost" && parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "::1") {
		t.Fatal("Copilot integration test requires a loopback PostgreSQL URL")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect to integration PostgreSQL: %v", err)
	}
	schema := "copilot_chat_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
		`CREATE TABLE ai_prompts_metadata(id serial PRIMARY KEY,name varchar(32) NOT NULL,config jsonb)`,
		`CREATE TABLE ai_prompts_messages(prompt_id integer NOT NULL,idx integer NOT NULL,role "AiPromptRole" NOT NULL,content text NOT NULL)`,
		`CREATE TABLE ai_action_runs(id text,user_id text,session_id text,user_message_id text,status text,action_id text,created_at timestamptz)`,
		`CREATE TABLE ai_usage_events(id text PRIMARY KEY,workspace_id text,local_workspace_id text,user_id text,provider text,provider_source text,billing_unit_id text,task_id text,feature_kind text,model text,session_id text,created_at timestamptz)`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("set up Copilot schema: %v", err)
		}
	}
	for _, statement := range []string{
		`INSERT INTO workspaces(id) VALUES('workspace-1')`,
		`INSERT INTO ai_prompts_metadata(id,name) VALUES(1,'ask')`,
		`INSERT INTO ai_prompts_messages(prompt_id,idx,role,content) VALUES(1,0,'system','Use concise answers.')`,
		`INSERT INTO ai_sessions_metadata(id,user_id,workspace_id,doc_id,prompt_name) VALUES('session-1','user-1','workspace-1','doc-1','ask')`,
		`INSERT INTO ai_sessions_messages(id,session_id,role,content,attachments) VALUES('message-1','session-1','user','Explain the page','[]')`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	imageBody := []byte{1, 2, 3, 4}
	imageHash := sha256.Sum256(imageBody)
	imageKey := base64.RawURLEncoding.EncodeToString(imageHash[:])
	attachmentURL := "/api/copilot/chat/session-1/attachments/" + imageKey + "?workspaceId=workspace-1&fileName=diagram.png"
	if _, err := pool.Exec(ctx, `UPDATE ai_sessions_messages SET attachments=$1 WHERE id='message-1'`, []byte(`["`+attachmentURL+`"]`)); err != nil {
		t.Fatal(err)
	}
	var providerRequest struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
		Tools  []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
		Messages []struct {
			Role       string            `json:"role"`
			Content    json.RawMessage   `json:"content"`
			Name       string            `json:"name"`
			ToolCallID string            `json:"tool_call_id"`
			ToolCalls  []json.RawMessage `json:"tool_calls"`
		} `json:"messages"`
	}
	failNextProviderRequest := false
	toolCallMode := false
	toolRound := 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer test-secret" {
			t.Errorf("provider request method=%s path=%s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&providerRequest); err != nil {
			t.Errorf("decode provider request: %v", err)
		}
		if failNextProviderRequest {
			failNextProviderRequest = false
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if toolCallMode && toolRound == 0 {
			toolRound++
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"type\":\"function\",\"function\":{\"name\":\"doc_search\",\"arguments\":\"{\\\"query\\\":\\\"needle\\\"}\"}}]}}]}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"The page\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\" is clear.\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer provider.Close()
	store := NewStore(pool)
	objects := &attachmentObjects{values: map[string]attachmentObject{
		"user-1/workspace-1/" + imageKey: {
			body: imageBody, metadata: storage.ObjectMetadata{ContentType: "image/png", ContentLength: int64(len(imageBody))},
		},
	}}
	handler := NewChatHTTPHandler(pool, config.Config{
		FydeOSManagedCopilot: true, FydeOSManagedCopilotAPIKey: "test-secret",
		FydeOSManagedCopilotBaseURL: provider.URL + "/v1", FydeOSManagedCopilotModel: "test-model",
	}, chatTestAuthenticator{principal: &auth.Principal{User: auth.User{ID: "user-1"}}}, store, func(context.Context, string, string, *string) error { return nil }, chatTestFeature{enabled: true})
	quotaPublisher := &chatQuotaPublisherRecorder{}
	handler.SetQuotaStatePublisher(quotaPublisher)
	handler.SetSessionAttachments(NewSessionAttachments(objects, store, func(context.Context, string, string, *string) error { return nil }, nil, nil))
	toolExecutor := &chatToolExecutorRecorder{}
	handler.SetToolExecutor(toolExecutor)
	request := httptest.NewRequest(http.MethodGet, "/api/copilot/chat/session-1/stream-object?messageId=message-1&toolsConfig=%7B%22searchWorkspace%22%3Afalse%2C%22readingDocs%22%3Afalse%7D", nil)
	request.SetPathValue("sessionId", "session-1")
	request.SetPathValue("endpoint", "stream-object")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"textDelta":"The page"`) || !strings.Contains(recorder.Body.String(), `"textDelta":" is clear."`) {
		t.Fatalf("Copilot stream status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	if !providerRequest.Stream || providerRequest.Model != "test-model" || len(providerRequest.Messages) != 2 || providerRequest.Messages[0].Role != "system" || !bytes.Contains(providerRequest.Messages[1].Content, []byte(`"image_url"`)) || !bytes.Contains(providerRequest.Messages[1].Content, []byte("data:image/png;base64,AQIDBA==")) {
		t.Fatalf("provider prompt = %#v", providerRequest)
	}
	var assistantContent string
	var streamObjects []byte
	if err := pool.QueryRow(ctx, `SELECT content,"streamObjects" FROM ai_sessions_messages WHERE session_id='session-1' AND role='assistant'`).Scan(&assistantContent, &streamObjects); err != nil {
		t.Fatal(err)
	}
	if assistantContent != "The page is clear." || !strings.Contains(string(streamObjects), "text-delta") {
		t.Fatalf("persisted assistant message content=%q streamObjects=%s", assistantContent, streamObjects)
	}
	if used, err := store.CountFydeosManagedRequests(ctx, "user-1", time.Now()); err != nil || used != 1 {
		t.Fatalf("managed quota after saved chat = %d, %v; want one", used, err)
	}
	if len(quotaPublisher.users) != 1 || quotaPublisher.users[0] != "user-1" {
		t.Fatalf("quota invalidations after reservation = %v; want [user-1]", quotaPublisher.users)
	}
	if _, err := pool.Exec(ctx, `UPDATE ai_prompts_metadata SET config='{"tools":["docSearch","docRead","docCanvasRead"]}' WHERE name='ask'`); err != nil {
		t.Fatal(err)
	}
	toolCallMode = true
	enabledToolsRequest := httptest.NewRequest(http.MethodGet, "/api/copilot/chat/session-1/stream-object?messageId=message-1&toolsConfig=%7B%22searchWorkspace%22%3Atrue%2C%22readingDocs%22%3Atrue%7D", nil)
	enabledToolsRequest.SetPathValue("sessionId", "session-1")
	enabledToolsRequest.SetPathValue("endpoint", "stream-object")
	enabledToolsResponse := httptest.NewRecorder()
	handler.ServeHTTP(enabledToolsResponse, enabledToolsRequest)
	toolCallMode = false
	if enabledToolsResponse.Code != http.StatusOK || !strings.Contains(enabledToolsResponse.Body.String(), `"textDelta":"The page"`) {
		t.Fatalf("enabled tools response status=%d body=%q", enabledToolsResponse.Code, enabledToolsResponse.Body.String())
	}
	if toolExecutor.userID != "user-1" || toolExecutor.workspaceID != "workspace-1" || toolExecutor.name != "doc_search" || string(toolExecutor.input) != `{"query":"needle"}` || len(providerRequest.Messages) != 5 || providerRequest.Messages[3].Role != "assistant" || len(providerRequest.Messages[3].ToolCalls) != 1 || providerRequest.Messages[4].Role != "tool" || providerRequest.Messages[4].ToolCallID != "call-1" {
		t.Fatalf("tool execution=%+v provider messages=%+v", toolExecutor, providerRequest.Messages)
	}
	toolSchemas := map[string]bool{}
	for _, tool := range providerRequest.Tools {
		toolSchemas[tool.Function.Name] = true
	}
	if !toolSchemas["doc_search"] || !toolSchemas["doc_read"] || !toolSchemas["doc_canvas_read"] {
		t.Fatalf("provider document tool schemas = %#v", providerRequest.Tools)
	}
	if _, err := pool.Exec(ctx, `UPDATE ai_prompts_metadata SET config=NULL WHERE name='ask'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ai_sessions_messages(id,session_id,role,content,attachments) VALUES('message-2','session-1','user','Fail this provider request','[]')`); err != nil {
		t.Fatal(err)
	}
	failNextProviderRequest = true
	failedRequest := httptest.NewRequest(http.MethodGet, "/api/copilot/chat/session-1/stream-object?messageId=message-2", nil)
	failedRequest.SetPathValue("sessionId", "session-1")
	failedRequest.SetPathValue("endpoint", "stream-object")
	failedResponse := httptest.NewRecorder()
	handler.ServeHTTP(failedResponse, failedRequest)
	if !strings.Contains(failedResponse.Body.String(), `"status":502`) {
		t.Fatalf("failed provider response = %q", failedResponse.Body.String())
	}
	if used, err := store.CountFydeosManagedRequests(ctx, "user-1", time.Now()); err != nil || used != 1 {
		t.Fatalf("managed quota after failed provider request = %d, %v; want one", used, err)
	}
	if len(quotaPublisher.users) != 4 || quotaPublisher.users[2] != "user-1" || quotaPublisher.users[3] != "user-1" {
		t.Fatalf("quota invalidations after failed request/release = %v; want four user-1 events", quotaPublisher.users)
	}

	byokCases := []struct {
		provider string
		path     string
		event    string
		text     string
	}{
		{provider: "openai", path: "/responses", event: "data: {\"type\":\"response.output_text.delta\",\"delta\":\"BYOK openai\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n", text: "BYOK openai"},
		{provider: "anthropic", path: "/v1/messages", event: "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"BYOK anthropic\"}}\n\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\ndata: {\"type\":\"message_stop\"}\n\n", text: "BYOK anthropic"},
		{provider: "gemini", path: "/models/model-1:streamGenerateContent", event: "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"BYOK gemini\"}]},\"finishReason\":\"STOP\"}]}\n\n", text: "BYOK gemini"},
	}
	for _, test := range byokCases {
		t.Run("BYOK "+test.provider, func(t *testing.T) {
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != test.path {
					t.Errorf("provider path = %q, want %q", r.URL.Path, test.path)
				}
				if test.provider == "anthropic" && r.Header.Get("x-api-key") != "byok-secret" {
					t.Errorf("Anthropic credential header = %q", r.Header.Get("x-api-key"))
				}
				if test.provider == "gemini" && r.Header.Get("x-goog-api-key") != "byok-secret" {
					t.Errorf("Gemini credential header = %q", r.Header.Get("x-goog-api-key"))
				}
				if test.provider == "openai" && r.Header.Get("Authorization") != "Bearer byok-secret" {
					t.Errorf("OpenAI credential header = %q", r.Header.Get("Authorization"))
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode provider body: %v", err)
				}
				if test.provider != "gemini" && body["model"] != "model-1" {
					t.Errorf("provider model = %#v", body["model"])
				}
				if test.provider == "gemini" && r.URL.Query().Get("alt") != "sse" {
					t.Errorf("Gemini stream format = %q", r.URL.Query().Get("alt"))
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(test.event))
				if test.provider == "openai" {
					_, _ = w.Write([]byte("data: [DONE]\n\n"))
				}
			}))
			defer provider.Close()
			target := byok.ChatTarget{
				Source: "byok_server", Provider: test.provider, ModelID: "model-1", Endpoint: byok.Endpoint{Kind: "provider_default"},
				BaseURL: provider.URL, Credential: byok.NewSensitiveCredential([]byte("byok-secret")), Transport: http.DefaultTransport,
			}
			handler := NewChatHTTPHandler(pool, config.Config{}, chatTestAuthenticator{principal: &auth.Principal{User: auth.User{ID: "user-1"}}}, store, func(context.Context, string, string, *string) error { return nil }, chatTestFeature{enabled: true})
			handler.SetSessionAttachments(NewSessionAttachments(objects, store, func(context.Context, string, string, *string) error { return nil }, nil, nil))
			handler.SetBYOKProfileStore(chatTestBYOKResolver{target: target})
			request := httptest.NewRequest(http.MethodGet, "/api/copilot/chat/session-1/stream-object?messageId=message-1&routeTargetId=fydeos-byok%3Aprofile-1%3Amodel-1", nil)
			request.SetPathValue("sessionId", "session-1")
			request.SetPathValue("endpoint", "stream-object")
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"textDelta":"`+test.text+`"`) || strings.Contains(recorder.Body.String(), "byok-secret") {
				t.Fatalf("BYOK response status=%d body=%q", recorder.Code, recorder.Body.String())
			}
			var usageSource, billingUnit string
			if err := pool.QueryRow(ctx, `SELECT provider_source,billing_unit_id FROM ai_usage_events WHERE user_id='user-1' AND feature_kind='chat' ORDER BY created_at DESC LIMIT 1`).Scan(&usageSource, &billingUnit); err != nil || usageSource != "byok_server" || billingUnit != "message-1" {
				t.Fatalf("server BYOK usage source=%q billing=%q err=%v", usageSource, billingUnit, err)
			}

			localTarget := target
			localTarget.Source = "byok_local"
			localTarget.Credential = byok.NewSensitiveCredential([]byte("byok-secret"))
			localResolver := &localChatResolverStub{target: localTarget}
			handler.SetBYOKProfileStore(localResolver)
			request = httptest.NewRequest(http.MethodGet, "/api/copilot/chat/session-1/stream-object?messageId=message-1&byokLeaseId=lease-1&routeTargetId=fydeos-byok%3Alease-1%3A0%3Amodel-1", nil)
			request.SetPathValue("sessionId", "session-1")
			request.SetPathValue("endpoint", "stream-object")
			recorder = httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"textDelta":"`+test.text+`"`) || localResolver.leaseID != "lease-1" || localResolver.profileID != "lease-1:0" {
				t.Fatalf("local BYOK response status=%d route=%+v body=%q", recorder.Code, localResolver, recorder.Body.String())
			}
			if err := pool.QueryRow(ctx, `SELECT provider_source,billing_unit_id FROM ai_usage_events WHERE user_id='user-1' AND feature_kind='chat' AND provider_source='byok_local' ORDER BY created_at DESC LIMIT 1`).Scan(&usageSource, &billingUnit); err != nil || usageSource != "byok_local" || billingUnit != "message-1" {
				t.Fatalf("local BYOK usage source=%q billing=%q err=%v", usageSource, billingUnit, err)
			}
		})
	}
	testBYOKDocumentToolLoops(t, pool, store)
}
