// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package copilot

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/fydeos/fydeos-notes-server/internal/byok"
	"github.com/fydeos/fydeos-notes-server/internal/storage"
)

type transcriptTargetFunc func(context.Context, string, string, string) (byok.ChatTarget, error)

func (f transcriptTargetFunc) ResolveServerTarget(ctx context.Context, w, u, operation string) (byok.ChatTarget, error) {
	return f(ctx, w, u, operation)
}

func (s *transcriptTaskObjectStore) GetObject(_ context.Context, scope, key string) (io.ReadCloser, storage.ObjectMetadata, error) {
	if scope != storage.CopilotScope {
		return nil, storage.ObjectMetadata{}, errors.New("wrong scope")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.objects[key]
	if !ok {
		return nil, storage.ObjectMetadata{}, errors.New("missing object")
	}
	return io.NopCloser(bytes.NewReader(value)), storage.ObjectMetadata{}, nil
}

const transcriptTestSummary = `{"title":"Meeting","durationMinutes":1,"attendees":["A"],"keyPoints":["Hello"],"actionItems":[{"description":"Follow up","owner":null,"deadline":null}],"decisions":[],"openQuestions":[],"blockers":[]}`

func transcriptHTTPFixture(t *testing.T, afterAudio func()) (TranscriptTargetResolver, *atomic.Int32) {
	t.Helper()
	calls := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]json.RawMessage
		if r.Method != "POST" || json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("bad provider request")
			w.WriteHeader(400)
			return
		}
		switch r.URL.Path {
		case "/models/audio-model:generateContent":
			if r.Header.Get("x-goog-api-key") != "test-audio-key" {
				t.Error("missing Gemini credential")
			}
			var contents []struct {
				Parts []struct {
					Text   string `json:"text"`
					Inline *struct {
						MIME string `json:"mimeType"`
						Data string `json:"data"`
					} `json:"inlineData"`
				} `json:"parts"`
			}
			if json.Unmarshal(body["contents"], &contents) != nil || len(contents) != 1 || len(contents[0].Parts) != 2 || contents[0].Parts[1].Inline == nil {
				t.Error("missing audio request")
				w.WriteHeader(400)
				return
			}
			data, err := base64.StdEncoding.DecodeString(contents[0].Parts[1].Inline.Data)
			if err != nil || len(data) == 0 {
				t.Error("missing audio bytes")
			}
			if len(body["generationConfig"]) == 0 {
				t.Error("missing structured output contract")
			}
			if afterAudio != nil {
				afterAudio()
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"usageMetadata": map[string]int{"promptTokenCount": 10, "candidatesTokenCount": 5, "totalTokenCount": 15}, "candidates": []any{map[string]any{"finishReason": "STOP", "content": map[string]any{"parts": []any{map[string]string{"text": `[{"a":" A ","s":0,"e":2,"t":" Hello "},{"a":"A","s":1,"e":3,"t":"World"}]`}}}}}})
		case "/responses":
			if r.Header.Get("Authorization") != "Bearer test-summary-key" {
				t.Error("missing OpenAI credential")
			}
			if !bytes.Contains(body["input"], []byte("A: Hello")) || len(body["text"]) == 0 {
				t.Error("summary missing normalized transcript/schema")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"usage": map[string]int{"input_tokens": 20, "output_tokens": 10, "total_tokens": 30}, "status": "completed", "output": []any{map[string]any{"type": "message", "content": []any{map[string]string{"type": "output_text", "text": transcriptTestSummary}}}}})
		default:
			t.Errorf("unexpected provider path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(server.Close)
	resolver := transcriptTargetFunc(func(_ context.Context, w, u, operation string) (byok.ChatTarget, error) {
		if w != "workspace-1" || u != "user-1" {
			return byok.ChatTarget{}, byok.ErrTargetUnavailable
		}
		target := byok.ChatTarget{BaseURL: server.URL, Transport: http.DefaultTransport, Endpoint: byok.Endpoint{Kind: "provider_default"}}
		if operation == "transcript" {
			target.Provider = "gemini"
			target.ModelID = "audio-model"
			target.Credential = byok.NewSensitiveCredential([]byte("test-audio-key"))
		} else if operation == "structured" {
			target.Provider = "openai"
			target.ModelID = "summary-model"
			target.Credential = byok.NewSensitiveCredential([]byte("test-summary-key"))
		} else {
			return byok.ChatTarget{}, byok.ErrTargetUnavailable
		}
		return target, nil
	})
	return resolver, calls
}

func TestTranscriptExecutorAudioAndSummaryHTTP(t *testing.T) {
	targets, calls := transcriptHTTPFixture(t, nil)
	key := "user-1/workspace-1/transcripts/task-1/0"
	objects := &transcriptTaskObjectStore{objects: map[string][]byte{key: []byte("test audio bytes")}}
	checks := 0
	executor := NewTranscriptExecutor(objects, targets, func(_ context.Context, u, w string, personal bool) error {
		checks++
		if u != "user-1" || w != "workspace-1" || personal {
			return errors.New("wrong scope")
		}
		return nil
	})
	input := `{"scopeMode":"canonical","infos":[{"key":"` + key + `","mimeType":"audio/wav","index":0}],"sourceAudio":{"blobId":"blob-1"},"quality":{"degraded":false},"sliceManifest":[{"index":0,"startSec":60,"durationSec":10}]}`
	value, err := executor.ExecuteTranscript(context.Background(), TranscriptExecutionTask{ID: "task-1", UserID: "user-1", WorkspaceID: "workspace-1", InputSnapshot: []byte(input)})
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Text     string                   `json:"normalizedTranscript"`
		Segments []normalizedAudioSegment `json:"normalizedSegments"`
		Summary  json.RawMessage          `json:"summaryJson"`
		Version  string                   `json:"version"`
		Source   map[string]string        `json:"sourceAudio"`
	}
	if json.Unmarshal(value, &result) != nil || result.Text != "00:01:00 A: Hello\n00:01:02 A: World" || len(result.Segments) != 2 || result.Segments[1].StartSec != 62 || result.Version != "transcript-result-v1" || result.Source["blobId"] != "blob-1" || !bytes.Contains(result.Summary, []byte("Meeting")) {
		t.Fatalf("unexpected result %s", value)
	}
	if calls.Load() != 2 || checks != 4 {
		t.Fatalf("provider calls=%d authorization checks=%d", calls.Load(), checks)
	}
}

func TestTranscriptExecutorRecoversProviderBlockedAudio(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			t.Error("invalid provider request")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/models/audio-model:generateContent":
			calls.Add(1)
			var contents []struct {
				Parts []struct {
					Inline *struct {
						Data string `json:"data"`
					} `json:"inlineData"`
				} `json:"parts"`
			}
			if json.Unmarshal(request["contents"], &contents) != nil || len(contents) != 1 || len(contents[0].Parts) != 2 || contents[0].Parts[1].Inline == nil {
				t.Error("provider request did not contain audio")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			audio, err := base64.StdEncoding.DecodeString(contents[0].Parts[1].Inline.Data)
			if err != nil {
				t.Error("invalid inline audio")
			}
			if string(audio) == "blocked audio" || string(audio) == "blocked part" {
				_ = json.NewEncoder(w).Encode(map[string]any{"promptFeedback": map[string]string{"blockReason": "SAFETY"}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"candidates": []any{map[string]any{"finishReason": "STOP", "content": map[string]any{"parts": []any{map[string]string{"text": `[{"a":"A","s":1,"e":2,"t":"Known speech"}]`}}}}}})
		case "/responses":
			var prompt string
			if json.Unmarshal(request["input"], &prompt) != nil || !strings.Contains(prompt, "do not infer missing content") {
				t.Error("summary prompt did not guard missing audio")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "output": []any{map[string]any{"type": "message", "content": []any{map[string]string{"type": "output_text", "text": transcriptTestSummary}}}}})
		default:
			t.Errorf("unexpected provider path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	objects := &transcriptTaskObjectStore{objects: map[string][]byte{"user-1/workspace-1/transcripts/task-1/0": []byte("blocked audio")}}
	targets := transcriptTargetFunc(func(_ context.Context, _, _ string, operation string) (byok.ChatTarget, error) {
		target := byok.ChatTarget{BaseURL: server.URL, Transport: http.DefaultTransport, Endpoint: byok.Endpoint{Kind: "provider_default"}}
		if operation == "transcript" {
			target.Provider, target.ModelID = "gemini", "audio-model"
			target.Credential = byok.NewSensitiveCredential([]byte("audio-key"))
		} else if operation == "structured" {
			target.Provider, target.ModelID = "openai", "summary-model"
			target.Endpoint.Dialect = "responses"
			target.Credential = byok.NewSensitiveCredential([]byte("summary-key"))
		} else {
			return byok.ChatTarget{}, byok.ErrTargetUnavailable
		}
		return target, nil
	})
	executor := NewTranscriptExecutor(objects, targets, func(context.Context, string, string, bool) error { return nil })
	executor.splitAudio = func(_ context.Context, audio []byte, mime string) ([]transcriptAudioPart, error) {
		if string(audio) != "blocked audio" || mime != "audio/mp4" {
			t.Fatalf("split input = %q/%q", audio, mime)
		}
		return []transcriptAudioPart{{Data: []byte("speech part"), StartSec: 0, DurationSec: 30}, {Data: []byte("blocked part"), StartSec: 30, DurationSec: 1}}, nil
	}
	input := `{"scopeMode":"canonical","infos":[{"key":"user-1/workspace-1/transcripts/task-1/0","mimeType":"audio/mp4","index":0}],"sliceManifest":[{"index":0,"startSec":0,"durationSec":31}]}`
	value, err := executor.ExecuteTranscript(context.Background(), TranscriptExecutionTask{ID: "task-1", UserID: "user-1", WorkspaceID: "workspace-1", InputSnapshot: []byte(input)})
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Segments []normalizedAudioSegment `json:"normalizedSegments"`
	}
	if json.Unmarshal(value, &result) != nil || len(result.Segments) != 2 || result.Segments[0].Text != "Known speech" || result.Segments[0].StartSec != 1 || result.Segments[1].Text != "[Transcription unavailable: this audio interval was blocked by the provider.]" || result.Segments[1].StartSec != 30 || result.Segments[1].EndSec != 31 {
		t.Fatalf("recovered transcript = %s", value)
	}
	if calls.Load() != 3 {
		t.Fatalf("audio provider calls = %d, want original plus two recovery parts", calls.Load())
	}
}

func TestTranscriptExecutorRejectsObjectScopeAndRevocation(t *testing.T) {
	for _, scenario := range []string{"wrong-key", "revoked", "denied", "changed-scope"} {
		t.Run(scenario, func(t *testing.T) {
			var revoked atomic.Bool
			targets, calls := transcriptHTTPFixture(t, func() { revoked.Store(true) })
			key := "user-1/workspace-1/transcripts/task-1/0"
			objects := &transcriptTaskObjectStore{objects: map[string][]byte{key: []byte("audio")}}
			executor := NewTranscriptExecutor(objects, targets, func(context.Context, string, string, bool) error {
				if revoked.Load() || scenario == "denied" {
					return errors.New("revoked")
				}
				return nil
			})
			inputKey := key
			if scenario == "wrong-key" {
				inputKey = "other-user/workspace-1/private"
			}
			scope := "canonical"
			if scenario == "changed-scope" {
				scope = "unexpected"
			}
			snapshot, _ := json.Marshal(map[string]any{"scopeMode": scope, "infos": []any{map[string]any{"key": inputKey, "mimeType": "audio/wav"}}})
			value, err := executor.ExecuteTranscript(context.Background(), TranscriptExecutionTask{ID: "task-1", UserID: "user-1", WorkspaceID: "workspace-1", InputSnapshot: snapshot})
			if err == nil || value != nil {
				t.Fatalf("expected rejection, got %s/%v", value, err)
			}
			expected := int32(0)
			if scenario == "revoked" {
				expected = 1
			}
			if calls.Load() != expected {
				t.Fatalf("calls=%d expected %d", calls.Load(), expected)
			}
		})
	}
}

func TestTranscriptTimestampNormalization(t *testing.T) {
	for _, tc := range []struct {
		name                                     string
		start, end, duration, wantStart, wantEnd float64
	}{
		{"mmss", 130, 140, 100, 90, 100}, {"milliseconds", 10000, 20000, 30, 10, 20}, {"scaled", 65, 75, 60, 52, 60}, {"clamped", -1, 900, 20, 0, 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			segments := []audioSegment{{Start: tc.start, End: tc.end}}
			normalizeSliceTimes(segments, &tc.duration)
			if segments[0].Start != tc.wantStart || segments[0].End != tc.wantEnd {
				t.Fatalf("got %#v", segments)
			}
		})
	}
}

func TestTranscriptProviderRejectsTruncationAndMalformedSchema(t *testing.T) {
	for _, value := range []string{`null`, `{}`, `[{"a":"A","s":0,"e":1}]`, `[{"a":"A","s":"0","e":1,"t":"text"}]`} {
		var decoded any
		_ = json.Unmarshal([]byte(value), &decoded)
		if validateTranscriptValue(decoded, transcriptSegmentsSchema()) {
			t.Fatalf("accepted %s", value)
		}
	}
	for _, format := range []string{"gemini", "openai-chat", "openai-responses", "anthropic"} {
		if _, err := transcriptProviderValue([]byte(`{"status":"incomplete","stop_reason":"max_tokens","choices":[{"finish_reason":"length","message":{"content":"[]"}}],"candidates":[{"finishReason":"MAX_TOKENS","content":{"parts":[{"text":"[]"}]}}]}`), format); err == nil {
			t.Fatalf("accepted truncated %s response", format)
		}
	}
	for _, raw := range []string{transcriptTestSummary, `[]`} {
		var decoded any
		_ = json.Unmarshal([]byte(raw), &decoded)
		schema := transcriptSummarySchema()
		if raw == "[]" {
			schema = transcriptSegmentsSchema()
		}
		if !validateTranscriptValue(decoded, schema) {
			t.Fatal("rejected valid structured output")
		}
	}
}

func TestOpenAIChatCompletionTranscriptRequestCarriesAudio(t *testing.T) {
	target := byok.ChatTarget{
		Provider: "openai", ModelID: "audio-model", BaseURL: "https://api.example.test/v1",
		Endpoint:   byok.Endpoint{Kind: "openai_compatible", Dialect: "chat_completions"},
		Credential: byok.NewSensitiveCredential([]byte("audio-key")),
	}
	defer target.Credential.Destroy()
	endpoint, headers, body, format, err := transcriptProviderRequest(target, "Transcribe", []byte("audio bytes"), "audio/wav", transcriptSegmentsSchema())
	if err != nil || format != "openai-chat" || endpoint != "https://api.example.test/v1/chat/completions" || headers["Authorization"] != "Bearer audio-key" {
		t.Fatalf("request route = %q/%q/%#v/%v", endpoint, format, headers, err)
	}
	var request struct {
		Messages []struct {
			Content []struct {
				Type  string `json:"type"`
				Audio struct {
					Data   string `json:"data"`
					Format string `json:"format"`
				} `json:"input_audio"`
			} `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &request) != nil || len(request.Messages) != 1 || len(request.Messages[0].Content) != 2 || request.Messages[0].Content[1].Type != "input_audio" || request.Messages[0].Content[1].Audio.Data != base64.StdEncoding.EncodeToString([]byte("audio bytes")) || request.Messages[0].Content[1].Audio.Format != "wav" {
		t.Fatalf("OpenAI audio request = %s", body)
	}
}

func TestOpenAIResponsesTranscriptRequestCarriesAudio(t *testing.T) {
	target := byok.ChatTarget{
		Provider: "openai", ModelID: "audio-model", BaseURL: "https://api.example.test/v1",
		Endpoint:   byok.Endpoint{Kind: "openai_compatible", Dialect: "responses"},
		Credential: byok.NewSensitiveCredential([]byte("audio-key")),
	}
	defer target.Credential.Destroy()

	endpoint, headers, body, format, err := transcriptProviderRequest(target, "Transcribe", []byte("audio bytes"), "audio/wav", transcriptSegmentsSchema())
	if err != nil || format != "openai-responses" || endpoint != "https://api.example.test/v1/responses" || headers["Authorization"] != "Bearer audio-key" {
		t.Fatalf("request route = %q/%q/%#v/%v", endpoint, format, headers, err)
	}
	var request struct {
		Input []struct {
			Role    string `json:"role"`
			Content []struct {
				Type  string `json:"type"`
				Text  string `json:"text"`
				Audio struct {
					Data   string `json:"data"`
					Format string `json:"format"`
				} `json:"input_audio"`
			} `json:"content"`
		} `json:"input"`
		Text struct {
			Format map[string]any `json:"format"`
		} `json:"text"`
	}
	if json.Unmarshal(body, &request) != nil || len(request.Input) != 1 || request.Input[0].Role != "user" || len(request.Input[0].Content) != 2 || request.Input[0].Content[0].Type != "input_text" || request.Input[0].Content[0].Text != "Transcribe" || request.Input[0].Content[1].Type != "input_audio" || request.Input[0].Content[1].Audio.Data != base64.StdEncoding.EncodeToString([]byte("audio bytes")) || request.Input[0].Content[1].Audio.Format != "wav" || request.Text.Format["type"] != "json_schema" {
		t.Fatalf("OpenAI Responses audio request = %s", body)
	}
	for _, mime := range []string{"audio/mp4", "audio/ogg", "audio/webm", "audio/flac", "audio/aac"} {
		if _, _, _, _, err := transcriptProviderRequest(target, "Transcribe", []byte("audio bytes"), mime, transcriptSegmentsSchema()); err == nil {
			t.Errorf("OpenAI Responses accepted unsupported audio MIME %q", mime)
		}
	}
}

func TestTranscriptExecutorPreflightProviderDialects(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider string
		endpoint byok.Endpoint
		wantErr  bool
	}{
		{name: "Gemini default", provider: "gemini", endpoint: byok.Endpoint{Kind: "provider_default"}},
		{name: "OpenAI Responses", provider: "openai", endpoint: byok.Endpoint{Kind: "provider_default", Dialect: "responses"}},
		{name: "OpenAI provider default", provider: "openai", endpoint: byok.Endpoint{Kind: "provider_default"}},
		{name: "OpenAI Chat Completions", provider: "openai", endpoint: byok.Endpoint{Kind: "openai_compatible", Dialect: "chat_completions"}},
		{name: "Anthropic", provider: "anthropic", endpoint: byok.Endpoint{Kind: "provider_default"}, wantErr: true},
		{name: "OpenAI unknown dialect", provider: "openai", endpoint: byok.Endpoint{Kind: "openai_compatible", Dialect: "unknown"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			targets := transcriptTargetFunc(func(context.Context, string, string, string) (byok.ChatTarget, error) {
				return byok.ChatTarget{Provider: tc.provider, Endpoint: tc.endpoint, Credential: byok.NewSensitiveCredential([]byte("secret"))}, nil
			})
			executor := NewTranscriptExecutor(&transcriptTaskObjectStore{objects: map[string][]byte{}}, targets, func(context.Context, string, string, bool) error { return nil })
			err := executor.Preflight(context.Background(), "user", "workspace", false)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Preflight() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestTranscriptProviderFailureAndCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = io.WriteString(w, "private provider credential detail")
	}))
	defer server.Close()
	target := byok.ChatTarget{Provider: "gemini", ModelID: "audio", BaseURL: server.URL, Credential: byok.NewSensitiveCredential([]byte("secret")), Transport: http.DefaultTransport}
	defer target.Credential.Destroy()
	_, err := generateTranscriptValue(context.Background(), target, "transcribe", []byte("audio"), "audio/wav", transcriptSegmentsSchema())
	if err == nil || strings.Contains(err.Error(), "private") || err.Error() != TranscriptProviderErrorCode {
		t.Fatalf("unexpected error %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := generateTranscriptValue(ctx, target, "transcribe", []byte("audio"), "audio/wav", transcriptSegmentsSchema()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
}

func TestTranscriptProviderRetriesEmptyGeminiCandidates(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			_, _ = io.WriteString(w, `{"candidates":[]}`)
			return
		}
		_, _ = io.WriteString(w, `{"candidates":[{"finishReason":"STOP","content":{"parts":[{"text":"[]"}]}}]}`)
	}))
	defer server.Close()
	target := byok.ChatTarget{Provider: "gemini", ModelID: "audio", BaseURL: server.URL, Credential: byok.NewSensitiveCredential([]byte("secret")), Transport: http.DefaultTransport}
	defer target.Credential.Destroy()
	value, err := generateTranscriptValue(context.Background(), target, "transcribe", []byte("audio"), "audio/wav", transcriptSegmentsSchema())
	if err != nil || string(value) != "[]" || calls.Load() != 2 {
		t.Fatalf("empty-candidate retry value=%s err=%v calls=%d", value, err, calls.Load())
	}
}

func TestTranscriptExecutorLoadsFrozenLegacyAudioLocators(t *testing.T) {
	for _, kind := range []string{"relative-key", "scoped-url", "inline-data"} {
		t.Run(kind, func(t *testing.T) {
			targets, _ := transcriptHTTPFixture(t, nil)
			objects := &transcriptTaskObjectStore{objects: map[string][]byte{"user-1/workspace-1/legacy-blob-0": []byte("old audio")}}
			info := map[string]any{"mimeType": "audio/wav", "index": 0}
			switch kind {
			case "relative-key":
				info["key"] = "legacy-blob-0"
				info["url"] = "https://expired-signature.invalid/audio"
			case "scoped-url":
				info["url"] = "https://old-storage.invalid/api/copilot/blob/user-1/workspace-1/legacy-blob-0?expired=true"
			case "inline-data":
				info["url"] = "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("inline audio"))
				objects.objects = nil
			}
			snapshot, _ := json.Marshal(map[string]any{"scopeMode": "canonical", "infos": []any{info}})
			executor := NewTranscriptExecutor(objects, targets, func(context.Context, string, string, bool) error { return nil })
			result, err := executor.ExecuteTranscript(context.Background(), TranscriptExecutionTask{ID: "old-task", UserID: "user-1", WorkspaceID: "workspace-1", BlobID: "legacy-blob", InputSnapshot: snapshot})
			if err != nil || !bytes.Contains(result, []byte("Hello")) {
				t.Fatalf("legacy %s result=%s err=%v", kind, result, err)
			}
		})
	}
}

type transcriptUsageFunc func(context.Context, TranscriptExecutionTask, byok.ChatTarget, *TranscriptTokenUsage) error

func (f transcriptUsageFunc) RecordTranscriptUsage(ctx context.Context, task TranscriptExecutionTask, target byok.ChatTarget, usage *TranscriptTokenUsage) error {
	return f(ctx, task, target, usage)
}

func TestTranscriptExecutorRecordsSelectionAndActualUsageWithoutResendingOnLedgerFailure(t *testing.T) {
	targets, calls := transcriptHTTPFixture(t, nil)
	key := "user-1/workspace-1/transcripts/task-1/0"
	objects := &transcriptTaskObjectStore{objects: map[string][]byte{key: []byte("audio")}}
	executor := NewTranscriptExecutor(objects, targets, func(context.Context, string, string, bool) error { return nil })
	selections, events := 0, 0
	var tokens int64
	executor.SetUsageRecorder(transcriptUsageFunc(func(ctx context.Context, task TranscriptExecutionTask, target byok.ChatTarget, usage *TranscriptTokenUsage) error {
		if task.ID != "task-1" {
			t.Fatal("wrong task attribution")
		}
		if usage == nil {
			selections++
		} else {
			events++
			tokens += usage.Total
		}
		return errors.New("ledger unavailable")
	}))
	input := `{"scopeMode":"canonical","infos":[{"key":"` + key + `","mimeType":"audio/wav"}]}`
	_, err := executor.ExecuteTranscript(context.Background(), TranscriptExecutionTask{ID: "task-1", UserID: "user-1", WorkspaceID: "workspace-1", InputSnapshot: []byte(input)})
	if err != nil || selections != 2 || events != 2 || tokens != 45 || calls.Load() != 2 {
		t.Fatalf("selections=%d events=%d tokens=%d calls=%d err=%v", selections, events, tokens, calls.Load(), err)
	}
}

func TestTranscriptExecutorRejectsUnscopedLegacyLocatorsBeforeProviderCall(t *testing.T) {
	for _, info := range []transcriptAudioInfo{
		{Key: "../legacy-blob-0"},
		{Key: "someone-else/workspace-1/legacy-blob-0"},
		{Key: "legacy-blob-1"},
		{URL: "https://host/api/copilot/blob/someone-else/workspace-1/legacy-blob-0"},
		{URL: "https://host/api/copilot/blob/user-1/workspace-1/%2e%2e/legacy-blob-0"},
		{URL: "http://127.0.0.1/secret"},
		{URL: "data:audio/wav;base64,invalid!"},
	} {
		targets, calls := transcriptHTTPFixture(t, nil)
		executor := NewTranscriptExecutor(&transcriptTaskObjectStore{}, targets, func(context.Context, string, string, bool) error { return nil })
		snapshot, _ := json.Marshal(map[string]any{"scopeMode": "canonical", "infos": []transcriptAudioInfo{info}})
		_, err := executor.ExecuteTranscript(context.Background(), TranscriptExecutionTask{ID: "task-1", UserID: "user-1", WorkspaceID: "workspace-1", BlobID: "legacy-blob", InputSnapshot: snapshot})
		if err == nil || calls.Load() != 0 {
			t.Fatalf("unsafe locator was accepted: %#v err=%v calls=%d", info, err, calls.Load())
		}
	}
}
