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
	"math"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/byok"
)

var errTranscriptNoCandidates = errors.New("transcript_provider_no_candidates")

func transcriptSegmentsSchema() map[string]any {
	return map[string]any{"type": "array", "items": transcriptObjectSchema(map[string]any{"a": map[string]any{"type": "string"}, "s": map[string]any{"type": "number"}, "e": map[string]any{"type": "number"}, "t": map[string]any{"type": "string"}}, "a", "s", "e", "t")}
}
func transcriptObjectSchema(properties map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
}
func transcriptSummarySchema() map[string]any {
	str := map[string]any{"type": "string"}
	strs := map[string]any{"type": "array", "items": str}
	nullable := map[string]any{"type": []string{"string", "null"}}
	return transcriptObjectSchema(map[string]any{"title": str, "durationMinutes": map[string]any{"type": "number"}, "attendees": strs, "keyPoints": strs, "actionItems": map[string]any{"type": "array", "items": transcriptObjectSchema(map[string]any{"description": str, "owner": nullable, "deadline": nullable}, "description", "owner", "deadline")}, "decisions": strs, "openQuestions": strs, "blockers": strs}, "title", "durationMinutes", "attendees", "keyPoints", "actionItems", "decisions", "openQuestions", "blockers")
}

func generateTranscriptValue(ctx context.Context, target byok.ChatTarget, prompt string, audio []byte, mime string, schema map[string]any) (json.RawMessage, error) {
	return generateTranscriptValueObserved(ctx, target, prompt, audio, mime, schema, nil)
}

func generateTranscriptValueObserved(ctx context.Context, target byok.ChatTarget, prompt string, audio []byte, mime string, schema map[string]any, observe func(context.Context, *TranscriptTokenUsage)) (json.RawMessage, error) {
	endpoint, headers, body, format, err := transcriptProviderRequest(target, prompt, audio, mime, schema)
	if err != nil {
		return nil, err
	}
	client := byok.NewPublicHTTPSClient(5 * time.Minute)
	if target.Transport != nil {
		client.Transport = target.Transport
	}
	defer client.CloseIdleConnections()
	// Model calls have no side-effectful tools. Only retry transient failures, and
	// never include a response body, endpoint or credential in a persisted error.
	delays := []time.Duration{0, 5 * time.Second, 15 * time.Second}
	for attempt, delay := range delays {
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if observe != nil {
			observe(ctx, nil)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, errTranscriptRoute
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		response, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() && attempt < len(delays)-1 {
				continue
			}
			return nil, transcriptExecutionError(TranscriptProviderErrorCode)
		}
		data, readErr := io.ReadAll(io.LimitReader(response.Body, (16<<20)+1))
		_ = response.Body.Close()
		if readErr != nil || len(data) > 16<<20 {
			return nil, transcriptExecutionError(TranscriptProviderErrorCode)
		}
		if response.StatusCode == 429 || response.StatusCode >= 500 {
			if attempt < len(delays)-1 {
				continue
			}
			return nil, transcriptExecutionError(TranscriptProviderErrorCode)
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return nil, transcriptExecutionError(TranscriptProviderErrorCode)
		}
		if usage, usageErr := transcriptProviderUsage(data, format); usageErr == nil && usage != nil && observe != nil {
			observe(ctx, usage)
		}
		value, err := transcriptProviderValue(data, format)
		if errors.Is(err, errTranscriptNoCandidates) && attempt < len(delays)-1 {
			continue
		}
		if err != nil {
			return nil, err
		}
		var decoded any
		if json.Unmarshal(value, &decoded) != nil || !validateTranscriptValue(decoded, schema) {
			return nil, errTranscriptResult
		}
		return value, nil
	}
	return nil, transcriptExecutionError(TranscriptProviderErrorCode)
}

func transcriptProviderRequest(target byok.ChatTarget, prompt string, audio []byte, mime string, schema map[string]any) (string, map[string]string, []byte, string, error) {
	if target.Credential == nil || len(target.Credential.Bytes()) == 0 || target.ModelID == "" {
		return "", nil, nil, "", errTranscriptRoute
	}
	parsed, err := url.Parse(target.BaseURL)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return "", nil, nil, "", errTranscriptRoute
	}
	headers := map[string]string{"Content-Type": "application/json", "Accept": "application/json"}
	key := string(target.Credential.Bytes())
	path, format := "", ""
	var body any
	switch target.Provider {
	case "gemini":
		parts := []any{map[string]any{"text": prompt}}
		if len(audio) > 0 {
			parts = append(parts, map[string]any{"inlineData": map[string]string{"mimeType": mime, "data": base64.StdEncoding.EncodeToString(audio)}})
		}
		body = map[string]any{"contents": []any{map[string]any{"role": "user", "parts": parts}}, "generationConfig": map[string]any{"responseMimeType": "application/json", "responseJsonSchema": schema}}
		headers["x-goog-api-key"] = key
		path = "/models/" + url.PathEscape(target.ModelID) + ":generateContent"
		format = "gemini"
	case "openai":
		headers["Authorization"] = "Bearer " + key
		if target.Endpoint.Dialect == "chat_completions" {
			var content any = prompt
			if len(audio) > 0 {
				audioFormat, ok := openAITranscriptAudioFormat(mime)
				if !ok {
					return "", nil, nil, "", errTranscriptInput
				}
				content = []any{map[string]any{"type": "text", "text": prompt}, map[string]any{"type": "input_audio", "input_audio": map[string]string{"data": base64.StdEncoding.EncodeToString(audio), "format": audioFormat}}}
			}
			body = map[string]any{"model": target.ModelID, "messages": []any{map[string]any{"role": "user", "content": content}}, "response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "transcript_result", "strict": true, "schema": schema}}}
			path, format = "/chat/completions", "openai-chat"
		} else {
			input := any(prompt)
			if len(audio) > 0 {
				audioFormat, ok := openAIResponsesTranscriptAudioFormat(mime)
				if !ok {
					return "", nil, nil, "", errTranscriptInput
				}
				input = []any{map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "input_text", "text": prompt},
					map[string]any{"type": "input_audio", "input_audio": map[string]string{"data": base64.StdEncoding.EncodeToString(audio), "format": audioFormat}},
				}}}
			}
			body = map[string]any{"model": target.ModelID, "input": input, "text": map[string]any{"format": map[string]any{"type": "json_schema", "name": "transcript_result", "strict": true, "schema": schema}}}
			path, format = "/responses", "openai-responses"
		}
	case "anthropic":
		if len(audio) > 0 {
			return "", nil, nil, "", errTranscriptRoute
		}
		headers["x-api-key"] = key
		headers["anthropic-version"] = "2023-06-01"
		body = map[string]any{"model": target.ModelID, "max_tokens": 8192, "messages": []any{map[string]any{"role": "user", "content": prompt}}, "output_config": map[string]any{"format": map[string]any{"type": "json_schema", "schema": schema}}}
		path, format = "/v1/messages", "anthropic"
	default:
		return "", nil, nil, "", errTranscriptRoute
	}
	data, err := json.Marshal(body)
	if err != nil {
		return "", nil, nil, "", errTranscriptInput
	}
	return strings.TrimRight(target.BaseURL, "/") + path, headers, data, format, nil
}

func openAIResponsesTranscriptAudioFormat(mime string) (string, bool) {
	switch strings.ToLower(strings.Split(mime, ";")[0]) {
	case "audio/wav", "audio/x-wav", "audio/wave", "audio/vnd.wave":
		return "wav", true
	case "audio/mp3", "audio/mpeg":
		return "mp3", true
	default:
		return "", false
	}
}

func openAITranscriptAudioFormat(mime string) (string, bool) {
	switch strings.ToLower(strings.Split(mime, ";")[0]) {
	case "audio/wav", "audio/x-wav", "audio/wave", "audio/vnd.wave":
		return "wav", true
	case "audio/mp3", "audio/mpeg":
		return "mp3", true
	case "audio/m4a", "audio/x-m4a", "audio/mp4", "video/mp4":
		return "m4a", true
	case "audio/ogg", "application/ogg", "audio/opus":
		return "ogg", true
	case "audio/webm", "video/webm":
		return "webm", true
	case "audio/flac", "audio/x-flac":
		return "flac", true
	case "audio/aac":
		return "aac", true
	default:
		return "", false
	}
}

func transcriptProviderValue(body []byte, format string) (json.RawMessage, error) {
	// Only accepted text output is consumed: refusals, thinking blocks, tools and
	// truncated responses must not accidentally become a successful transcript.
	var response struct {
		Status         string `json:"status"`
		PromptFeedback struct {
			BlockReason string `json:"blockReason"`
		} `json:"promptFeedback"`
		Candidates []struct {
			Finish  string `json:"finishReason"`
			Content struct {
				Parts []struct {
					Text    string `json:"text"`
					Thought bool   `json:"thought"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
		Choices []struct {
			Finish  string `json:"finish_reason"`
			Message struct {
				Content string  `json:"content"`
				Refusal *string `json:"refusal"`
			} `json:"message"`
		} `json:"choices"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(body, &response) != nil {
		return nil, errTranscriptResult
	}
	if format == "gemini" && response.PromptFeedback.BlockReason != "" && response.PromptFeedback.BlockReason != "BLOCK_REASON_UNSPECIFIED" {
		return nil, errTranscriptBlocked
	}
	var text strings.Builder
	switch format {
	case "gemini":
		if len(response.Candidates) == 0 {
			return nil, errTranscriptNoCandidates
		}
		if response.Candidates[0].Finish != "STOP" {
			return nil, errTranscriptResult
		}
		for _, part := range response.Candidates[0].Content.Parts {
			if !part.Thought {
				text.WriteString(part.Text)
			}
		}
	case "openai-chat":
		if len(response.Choices) == 0 || response.Choices[0].Finish != "stop" || response.Choices[0].Message.Refusal != nil {
			return nil, errTranscriptResult
		}
		text.WriteString(response.Choices[0].Message.Content)
	case "openai-responses":
		if response.Status != "completed" {
			return nil, errTranscriptResult
		}
		for _, output := range response.Output {
			if output.Type == "message" {
				for _, part := range output.Content {
					if part.Type == "output_text" {
						text.WriteString(part.Text)
					}
				}
			}
		}
	case "anthropic":
		if response.StopReason != "end_turn" {
			return nil, errTranscriptResult
		}
		for _, part := range response.Content {
			if part.Type == "text" {
				text.WriteString(part.Text)
			}
		}
	default:
		return nil, errTranscriptResult
	}
	value := json.RawMessage(strings.TrimSpace(text.String()))
	if !json.Valid(value) {
		return nil, errTranscriptResult
	}
	return value, nil
}

// Validates the small, fixed schemas above after provider decoding. This is not
// a general JSON Schema implementation and never accepts user-supplied schemas.
func validateTranscriptValue(value any, schema map[string]any) bool {
	if types, ok := schema["type"].([]string); ok {
		for _, t := range types {
			if validateTranscriptValue(value, map[string]any{"type": t}) {
				return true
			}
		}
		return false
	}
	switch schema["type"] {
	case "null":
		return value == nil
	case "string":
		_, ok := value.(string)
		return ok
	case "number":
		n, ok := value.(float64)
		return ok && !math.IsNaN(n) && !math.IsInf(n, 0) && math.Abs(n) < 1e12
	case "array":
		items, ok := value.([]any)
		if !ok {
			return false
		}
		itemSchema, ok := schema["items"].(map[string]any)
		if !ok {
			return false
		}
		for _, item := range items {
			if !validateTranscriptValue(item, itemSchema) {
				return false
			}
		}
		return true
	case "object":
		object, ok := value.(map[string]any)
		if !ok {
			return false
		}
		properties := schema["properties"].(map[string]any)
		for _, key := range schema["required"].([]string) {
			if _, ok := object[key]; !ok {
				return false
			}
		}
		for key, item := range object {
			property, ok := properties[key].(map[string]any)
			if !ok || !validateTranscriptValue(item, property) {
				return false
			}
		}
		return true
	}
	return false
}
