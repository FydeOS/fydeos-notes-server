// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package copilot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/byok"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/jackc/pgx/v5/pgxpool"
)

func providerToolEvents(format string) []string {
	switch format {
	case "openai-responses":
		return []string{
			`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call-search","name":"doc_search","arguments":""}}`,
			`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"query\":"}`,
			`{"type":"response.completed","response":{"status":"completed","output":[{"type":"reasoning","id":"rs-1","summary":[],"encrypted_content":"opaque-reasoning"},{"type":"function_call","id":"fc-1","call_id":"call-search","name":"doc_search","arguments":"{\"query\":\"needle\"}"},{"type":"function_call","id":"fc-2","call_id":"call-read","name":"doc_read","arguments":"{\"doc_id\":\"readable\"}"}]}}`,
		}
	case "anthropic":
		return []string{
			`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"private reasoning"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"opaque-reasoning"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call-search","name":"doc_search","input":{}}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"query\":"}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"needle\"}"}}`,
			`{"type":"content_block_stop","index":1}`,
			`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"call-read","name":"doc_read","input":{}}}`,
			`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"doc_id\":\"readable\"}"}}`,
			`{"type":"content_block_stop","index":2}`,
			`{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
			`{"type":"message_stop"}`,
		}
	case "gemini":
		return []string{
			`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"thought":true,"text":"private reasoning","thoughtSignature":"opaque-reasoning"},{"functionCall":{"id":"call-search","name":"doc_search","args":{"query":"needle"}},"thoughtSignature":"opaque-call"},{"functionCall":{"name":"doc_read","args":{"doc_id":"readable"}}}]},"finishReason":"STOP"}]}`,
		}
	default:
		return nil
	}
}

type providerToolRecorder struct{ calls []string }

func (r *providerToolRecorder) Execute(_ context.Context, user, workspace, name string, input json.RawMessage) (any, error) {
	r.calls = append(r.calls, user+"/"+workspace+"/"+name+"/"+string(input))
	return map[string]any{"result": "authorized document result"}, nil
}

// Invoked by the existing PostgreSQL + authenticated HTTP fixture, so this tests
// production request routing, provider continuation and assistant persistence.
func testBYOKDocumentToolLoops(t *testing.T, pool *pgxpool.Pool, store *Store) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE ai_prompts_metadata SET config='{"tools":["docSearch","docRead","docCanvasRead"]}' WHERE name='ask'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE ai_sessions_messages SET attachments='[]' WHERE id='message-1'`); err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"openai-responses", "anthropic", "gemini"} {
		for _, truncated := range []bool{false, true} {
			t.Run(fmt.Sprintf("document tools %s/truncated=%t", format, truncated), func(t *testing.T) {
				providerName := format
				if format == "openai-responses" {
					providerName = "openai"
				}
				var rounds atomic.Int32
				provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var request map[string]any
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					encoded, _ := json.Marshal(request)
					for _, name := range []string{"doc_search", "doc_read", "doc_canvas_read"} {
						if !strings.Contains(string(encoded), name) {
							t.Errorf("missing %s schema: %s", name, encoded)
						}
					}
					w.Header().Set("Content-Type", "text/event-stream")
					if rounds.Add(1) == 1 {
						for _, event := range providerToolEvents(format) {
							_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
						}
						return
					}
					if !strings.Contains(string(encoded), "opaque-reasoning") || strings.Count(string(encoded), "authorized document result") != 2 {
						t.Errorf("tool replay/result missing: %s", encoded)
					}
					if truncated {
						switch format {
						case "openai-responses":
							fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial answer\"}\n\n")
						case "anthropic":
							fmt.Fprint(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial answer\"}}\n\n")
						case "gemini":
							fmt.Fprint(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"partial answer\"}]}}]}\n\n")
						}
						return
					}
					switch format {
					case "openai-responses":
						input := request["input"].([]any)
						last := input[len(input)-1].(map[string]any)
						if last["type"] != "function_call_output" || last["call_id"] != "call-read" || request["store"] != false {
							t.Errorf("Responses continuation: %s", encoded)
						}
						_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"Tool answer\"}\n\n")
						_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n")
					case "anthropic":
						messages := request["messages"].([]any)
						last := messages[len(messages)-1].(map[string]any)
						blocks := last["content"].([]any)
						if last["role"] != "user" || len(blocks) != 2 || blocks[0].(map[string]any)["tool_use_id"] != "call-search" {
							t.Errorf("Anthropic parallel results: %s", encoded)
						}
						_, _ = fmt.Fprint(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Tool answer\"}}\n\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
						_, _ = fmt.Fprint(w, "data: {\"type\":\"message_stop\"}\n\n")
					case "gemini":
						contents := request["contents"].([]any)
						last := contents[len(contents)-1].(map[string]any)
						parts := last["parts"].([]any)
						first := parts[0].(map[string]any)["functionResponse"].(map[string]any)
						second := parts[1].(map[string]any)["functionResponse"].(map[string]any)
						if last["role"] != "user" || len(parts) != 2 || first["id"] != "call-search" || second["id"] != nil || !strings.Contains(string(encoded), "opaque-call") {
							t.Errorf("Gemini continuation: %s", encoded)
						}
						_, _ = fmt.Fprint(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"thought\":true,\"text\":\"private reasoning\"},{\"text\":\"Tool answer\"}]},\"finishReason\":\"STOP\"}]}\n\n")
					}
				}))
				defer provider.Close()
				executor := &providerToolRecorder{}
				handler := NewChatHTTPHandler(pool, config.Config{}, chatTestAuthenticator{principal: &auth.Principal{User: auth.User{ID: "user-1"}}}, store, func(context.Context, string, string, *string) error { return nil }, chatTestFeature{enabled: true})
				handler.SetToolExecutor(executor)
				handler.SetBYOKProfileStore(chatTestBYOKResolver{target: byok.ChatTarget{Provider: providerName, ModelID: "model-1", Endpoint: byok.Endpoint{Kind: "provider_default"}, BaseURL: provider.URL, Credential: byok.NewSensitiveCredential([]byte("byok-secret")), Transport: http.DefaultTransport}})
				request := httptest.NewRequest(http.MethodGet, "/api/copilot/chat/session-1/stream-object?messageId=message-1&routeTargetId=fydeos-byok%3Aprofile-1%3Amodel-1", nil)
				request.SetPathValue("sessionId", "session-1")
				request.SetPathValue("endpoint", "stream-object")
				response := httptest.NewRecorder()
				var before, after int
				if err := pool.QueryRow(ctx, `SELECT count(*) FROM ai_sessions_messages WHERE role='assistant' AND content='Tool answer'`).Scan(&before); err != nil {
					t.Fatal(err)
				}
				handler.ServeHTTP(response, request)
				if err := pool.QueryRow(ctx, `SELECT count(*) FROM ai_sessions_messages WHERE role='assistant' AND content='Tool answer'`).Scan(&after); err != nil {
					t.Fatal(err)
				}
				if truncated {
					var partial int
					if err := pool.QueryRow(ctx, `SELECT count(*) FROM ai_sessions_messages WHERE role='assistant' AND content='partial answer'`).Scan(&partial); err != nil {
						t.Fatal(err)
					}
					if rounds.Load() != 2 || len(executor.calls) != 2 || after != before || partial != 0 || !strings.Contains(response.Body.String(), `"status":502`) {
						t.Fatalf("truncated answer incorrectly accepted: rounds=%d calls=%v partial=%d response=%s", rounds.Load(), executor.calls, partial, response.Body.String())
					}
					return
				}
				if rounds.Load() != 2 || len(executor.calls) != 2 || after != before+1 || !strings.Contains(response.Body.String(), `"textDelta":"Tool answer"`) || strings.Contains(response.Body.String(), "opaque-") || strings.Contains(response.Body.String(), "private reasoning") {
					t.Fatalf("rounds=%d calls=%v persisted=%d response=%s", rounds.Load(), executor.calls, after-before, response.Body.String())
				}
				if executor.calls[0] != `user-1/workspace-1/doc_search/{"query":"needle"}` || executor.calls[1] != `user-1/workspace-1/doc_read/{"doc_id":"readable"}` {
					t.Fatalf("incorrect tool identity/arguments: %v", executor.calls)
				}
			})
		}
	}
}

func TestProviderToolTurnsRejectIncompleteAndMalformedInput(t *testing.T) {
	for _, format := range []string{"openai-responses", "anthropic", "gemini"} {
		t.Run(format, func(t *testing.T) {
			var turn providerToolTurn
			events := providerToolEvents(format)
			for _, event := range events {
				if err := turn.consume(format, event); err != nil {
					t.Fatal(err)
				}
			}
			if err := turn.finish(format); err != nil || len(turn.calls) != 2 {
				t.Fatalf("complete turn calls=%d err=%v", len(turn.calls), err)
			}
			turn.complete = false
			if err := turn.finish(format); err == nil {
				t.Fatal("truncated tool turn accepted")
			}
			if err := turn.consume(format, `{"error":{"message":"private provider error"}}`); err == nil {
				t.Fatal("provider error accepted")
			}
		})
	}
	for _, args := range []string{`null`, `[]`, `{`, `{"q":"` + strings.Repeat("x", 16<<10) + `"}`} {
		var turn providerToolTurn
		if err := turn.addCall(newProviderToolCall("id", "doc_search", args)); err == nil {
			t.Fatalf("invalid arguments accepted: %d bytes", len(args))
		}
	}
	var turn providerToolTurn
	for i := 0; i < 8; i++ {
		if err := turn.addCall(newProviderToolCall(fmt.Sprint(i), "doc_search", `{}`)); err != nil {
			t.Fatal(err)
		}
	}
	if err := turn.addCall(newProviderToolCall("9", "doc_search", `{}`)); err == nil {
		t.Fatal("tool budget exceeded")
	}
}

func TestProviderAnswerRequiresTerminalEvent(t *testing.T) {
	for _, test := range []struct{ format, event string }{
		{"openai-responses", `{"type":"response.output_text.delta","delta":"partial answer"}`},
		{"anthropic", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"partial answer"}}`},
		{"gemini", `{"candidates":[{"content":{"parts":[{"text":"partial answer"}]}}]}`},
	} {
		t.Run(test.format, func(t *testing.T) {
			var turn providerToolTurn
			if err := turn.consume(test.format, test.event); err != nil {
				t.Fatal(err)
			}
			if err := turn.finish(test.format); err == nil {
				t.Fatal("truncated answer accepted as success")
			}
		})
	}
}

func TestProviderToolsNeverExecuteDisabledUnknownOrTruncatedCalls(t *testing.T) {
	for _, format := range []string{"openai-responses", "anthropic", "gemini"} {
		for _, mode := range []string{"disabled", "unknown", "truncated", "error"} {
			t.Run(format+"/"+mode, func(t *testing.T) {
				events := providerToolEvents(format)
				if mode == "truncated" {
					if format == "gemini" {
						events[0] = strings.ReplaceAll(events[0], `,"finishReason":"STOP"`, "")
					} else {
						events = events[:len(events)-1]
					}
				}
				if mode == "error" {
					events = []string{`{"error":{"message":"private upstream detail"}}`}
				}
				provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					for _, event := range events {
						if mode == "unknown" {
							event = strings.ReplaceAll(event, "doc_read", "delete_everything")
						}
						_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
					}
				}))
				defer provider.Close()
				executor := &providerToolRecorder{}
				handler := &ChatHTTPHandler{client: provider.Client(), toolExecutor: executor}
				providerName := format
				if format == "openai-responses" {
					providerName = "openai"
				}
				route := chatRoute{provider: providerName, baseURL: provider.URL, model: "model", endpoint: byok.Endpoint{Kind: "provider_default"}}
				names := []string{"doc_search", "doc_read"}
				if mode == "disabled" {
					names = nil
				}
				err := handler.streamChat(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil), "message", ArtifactRetrievalScope{Mode: "workspace"}, []providerMessage{{Role: "user", Content: "question"}}, route, names, "user", "workspace")
				if err == nil || len(executor.calls) != 0 {
					t.Fatalf("unsafe execution: err=%v calls=%v", err, executor.calls)
				}
				if strings.Contains(err.Error(), "private upstream detail") {
					t.Fatal("provider error detail leaked")
				}
			})
		}
	}
}
