// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package byok

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	probeRequestTimeout = 15 * time.Second
	maxResponseBytes    = 2 << 20
)

var defaultBaseURLs = map[string]string{
	"openai":    "https://api.openai.com/v1",
	"anthropic": "https://api.anthropic.com",
	"gemini":    "https://generativelanguage.googleapis.com/v1beta",
	"fal":       "https://fal.run",
}

// HTTPProbeRunner probes providers with one small real request per check.
// The zero value is ready to use.
type HTTPProbeRunner struct {
	// baseURLs and transport override defaults in tests.
	baseURLs  map[string]string
	transport http.RoundTripper
}

type probeRequest struct {
	path     string
	body     any
	validate func([]byte) bool
}

var errUnsupportedProbe = errors.New("unsupported probe")

// Probe implements ProbeRunner.
func (r HTTPProbeRunner) Probe(ctx context.Context, target ProbeTarget) ProbeStatus {
	base := defaultBaseURLs[target.Provider]
	if override, ok := r.baseURLs[target.Provider]; ok {
		base = override
	}
	if target.Endpoint.Kind == endpointOpenAICompatible {
		base = target.Endpoint.URL
	}
	request, err := buildProbeRequest(target)
	if err != nil || base == "" {
		return failedStatus("unsupported_operation")
	}
	body, err := json.Marshal(request.body)
	if err != nil {
		return failedStatus("unsupported_operation")
	}
	ctx, cancel := context.WithTimeout(ctx, probeRequestTimeout)
	defer cancel()
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+request.path, bytes.NewReader(body))
	if err != nil {
		return failedStatus("transport")
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	setAuthHeaders(httpRequest.Header, target.Provider, target.Credential)

	transport := r.transport
	if transport == nil {
		if target.Endpoint.Kind == endpointOpenAICompatible {
			parsed, _ := url.Parse(base)
			transport = newGuardedTransport(parsed.Scheme, target.AllowPrivateEndpoint, target.EndpointResolver)
		} else {
			transport = publicTransport
		}
	}
	client := &http.Client{Transport: transport, CheckRedirect: noRedirect}
	response, err := client.Do(httpRequest)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			return failedStatus("timeout")
		}
		return failedStatus("transport")
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return failedStatus("timeout")
		}
		return failedStatus("transport")
	}
	switch code := response.StatusCode; {
	case code == http.StatusUnauthorized:
		return failedStatus("authentication")
	case code == http.StatusForbidden:
		return failedStatus("permission")
	case code == http.StatusNotFound:
		return failedStatus("not_found")
	case code == http.StatusTooManyRequests:
		return failedStatus("rate_limited")
	case code >= 500:
		return failedStatus("unavailable")
	case code < 200 || code > 299:
		return failedStatus("rejected")
	}
	if len(payload) > maxResponseBytes || !json.Valid(payload) || !request.validate(payload) {
		status := failedStatus("invalid_response")
		status.TestedAtMS = nowMS()
		return status
	}
	return ProbeStatus{Kind: probeVerified, TestedAtMS: nowMS()}
}

func setAuthHeaders(header http.Header, provider string, credential []byte) {
	key := string(credential)
	switch provider {
	case "openai":
		header.Set("Authorization", "Bearer "+key)
	case "anthropic":
		header.Set("x-api-key", key)
		header.Set("anthropic-version", "2023-06-01")
	case "gemini":
		header.Set("x-goog-api-key", key)
	case "fal":
		header.Set("Authorization", "Key "+key)
	}
}

const (
	probePrompt     = "Reply with the single word OK."
	structuredInstr = `Return the JSON object {"ok":true}.`
)

var okSchema = map[string]any{
	"type":                 "object",
	"properties":           map[string]any{"ok": map[string]any{"type": "boolean"}},
	"required":             []string{"ok"},
	"additionalProperties": false,
}

func buildProbeRequest(target ProbeTarget) (probeRequest, error) {
	model := target.ModelID
	custom := target.Endpoint.Kind == endpointOpenAICompatible
	switch target.Provider {
	case "openai":
		chatCompletions := custom && target.Endpoint.Dialect == "chat_completions"
		switch target.Operation {
		case "chat", "structured", "tool_calling":
			if chatCompletions {
				return openAIChatCompletionsProbe(model, target.Operation), nil
			}
			return openAIResponsesProbe(model, target.Operation), nil
		case "rerank":
			return probeRequest{path: "/chat/completions", body: map[string]any{
				"model": model, "max_tokens": 1, "logprobs": true, "top_logprobs": 2,
				"messages": []any{map[string]any{"role": "user", "content": "Is 'ok' relevant to 'ok'? Answer yes or no."}},
			}, validate: validateLogprob}, nil
		case "embedding":
			return probeRequest{path: "/embeddings", body: map[string]any{"model": model, "input": "probe"},
				validate: validateOpenAIEmbedding}, nil
		case "image":
			return probeRequest{path: "/images/generations", body: map[string]any{"model": model, "prompt": "A plain blue square.", "n": 1},
				validate: validateOpenAIImage}, nil
		}
	case "anthropic":
		if custom || target.Operation != "chat" {
			break
		}
		return probeRequest{path: "/v1/messages", body: map[string]any{
			"model": model, "max_tokens": 16, "messages": []any{map[string]any{"role": "user", "content": probePrompt}},
		}, validate: validateAnthropicText}, nil
	case "gemini":
		if custom {
			break
		}
		generate := "/models/" + url.PathEscape(model) + ":generateContent"
		userText := func(text string) []any {
			return []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": text}}}}
		}
		switch target.Operation {
		case "chat":
			return probeRequest{path: generate, body: map[string]any{"contents": userText(probePrompt)}, validate: validateGeminiText}, nil
		case "structured":
			return probeRequest{path: generate, body: map[string]any{
				"contents":         userText(structuredInstr),
				"generationConfig": map[string]any{"responseMimeType": "application/json", "responseSchema": map[string]any{"type": "OBJECT", "properties": map[string]any{"ok": map[string]any{"type": "BOOLEAN"}}, "required": []string{"ok"}}},
			}, validate: func(b []byte) bool { return isOKObject(geminiText(b)) }}, nil
		case "embedding":
			return probeRequest{path: "/models/" + url.PathEscape(model) + ":embedContent", body: map[string]any{
				"content": map[string]any{"parts": []any{map[string]any{"text": "probe"}}},
			}, validate: validateGeminiEmbedding}, nil
		case "image":
			return probeRequest{path: generate, body: map[string]any{
				"contents":         userText("Generate an image of a plain blue square."),
				"generationConfig": map[string]any{"responseModalities": []string{"TEXT", "IMAGE"}},
			}, validate: validateGeminiImage}, nil
		case "transcript":
			return probeRequest{path: generate, body: map[string]any{
				"contents": []any{map[string]any{"role": "user", "parts": []any{
					map[string]any{"text": `Transcribe the audio. Return a JSON array of segments {"a":speaker,"s":startSeconds,"e":endSeconds,"t":text}; return [] when silent.`},
					map[string]any{"inlineData": map[string]any{"mimeType": "audio/wav", "data": base64.StdEncoding.EncodeToString(silentWAV())}},
				}}},
				"generationConfig": map[string]any{"responseMimeType": "application/json"},
			}, validate: func(b []byte) bool { return isSegmentArray(geminiText(b)) }}, nil
		}
	case "fal":
		if custom || target.Operation != "image" || !validFalModelPath(model) {
			break
		}
		return probeRequest{path: "/" + model, body: map[string]any{"prompt": "A plain blue square.", "num_images": 1},
			validate: validateFalImage}, nil
	}
	return probeRequest{}, errUnsupportedProbe
}

func validFalModelPath(model string) bool {
	if model == "" || strings.HasPrefix(model, "/") || strings.HasPrefix(model, "workflows/") {
		return false
	}
	for _, segment := range strings.Split(model, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

var probeTool = map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string"}}, "required": []string{"value"}}

func openAIResponsesProbe(model, operation string) probeRequest {
	body := map[string]any{"model": model, "input": probePrompt}
	validate := func(b []byte) bool { return responsesText(b) != "" }
	switch operation {
	case "structured":
		body["input"] = structuredInstr
		body["text"] = map[string]any{"format": map[string]any{"type": "json_schema", "name": "probe", "schema": okSchema, "strict": true}}
		validate = func(b []byte) bool { return isOKObject(responsesText(b)) }
	case "tool_calling":
		body["input"] = "Call the probe tool with value OK."
		body["tools"] = []any{map[string]any{"type": "function", "name": "probe", "parameters": probeTool}}
		body["tool_choice"] = "required"
		validate = func(b []byte) bool {
			var r struct {
				Output []struct {
					Type string `json:"type"`
				} `json:"output"`
			}
			if json.Unmarshal(b, &r) != nil {
				return false
			}
			for _, item := range r.Output {
				if item.Type == "function_call" {
					return true
				}
			}
			return false
		}
	}
	return probeRequest{path: "/responses", body: body, validate: validate}
}

func openAIChatCompletionsProbe(model, operation string) probeRequest {
	message := func(text string) []any { return []any{map[string]any{"role": "user", "content": text}} }
	body := map[string]any{"model": model, "messages": message(probePrompt)}
	validate := func(b []byte) bool { return chatCompletionText(b) != "" }
	switch operation {
	case "structured":
		body["messages"] = message(structuredInstr)
		body["response_format"] = map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "probe", "schema": okSchema, "strict": true}}
		validate = func(b []byte) bool { return isOKObject(chatCompletionText(b)) }
	case "tool_calling":
		body["messages"] = message("Call the probe tool with value OK.")
		body["tools"] = []any{map[string]any{"type": "function", "function": map[string]any{"name": "probe", "parameters": probeTool}}}
		body["tool_choice"] = "required"
		validate = func(b []byte) bool {
			var r struct {
				Choices []struct {
					Message struct {
						ToolCalls []json.RawMessage `json:"tool_calls"`
					} `json:"message"`
				} `json:"choices"`
			}
			return json.Unmarshal(b, &r) == nil && len(r.Choices) > 0 && len(r.Choices[0].Message.ToolCalls) > 0
		}
	}
	return probeRequest{path: "/chat/completions", body: body, validate: validate}
}

func responsesText(b []byte) string {
	var r struct {
		OutputText string `json:"output_text"`
		Output     []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if json.Unmarshal(b, &r) != nil {
		return ""
	}
	if strings.TrimSpace(r.OutputText) != "" {
		return r.OutputText
	}
	var text strings.Builder
	for _, item := range r.Output {
		for _, part := range item.Content {
			text.WriteString(part.Text)
		}
	}
	if strings.TrimSpace(text.String()) == "" {
		return ""
	}
	return text.String()
}

func chatCompletionText(b []byte) string {
	var r struct {
		Choices []struct {
			Message struct {
				Content *string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(b, &r) != nil || len(r.Choices) == 0 || r.Choices[0].Message.Content == nil || strings.TrimSpace(*r.Choices[0].Message.Content) == "" {
		return ""
	}
	return *r.Choices[0].Message.Content
}

func validateLogprob(b []byte) bool {
	var r struct {
		Choices []struct {
			Logprobs struct {
				Content []struct {
					Logprob *float64 `json:"logprob"`
				} `json:"content"`
			} `json:"logprobs"`
		} `json:"choices"`
	}
	return json.Unmarshal(b, &r) == nil && len(r.Choices) > 0 && len(r.Choices[0].Logprobs.Content) > 0 && r.Choices[0].Logprobs.Content[0].Logprob != nil
}

func numericVector(values []float64) bool { return len(values) > 0 }

func validateOpenAIEmbedding(b []byte) bool {
	var r struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	return json.Unmarshal(b, &r) == nil && len(r.Data) > 0 && numericVector(r.Data[0].Embedding)
}

func validateOpenAIImage(b []byte) bool {
	var r struct {
		Data []struct {
			B64 string `json:"b64_json"`
			URL string `json:"url"`
		} `json:"data"`
	}
	return json.Unmarshal(b, &r) == nil && len(r.Data) > 0 && (r.Data[0].B64 != "" || r.Data[0].URL != "")
}

func validateAnthropicText(b []byte) bool {
	var r struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(b, &r) != nil {
		return false
	}
	for _, part := range r.Content {
		if part.Type == "text" && strings.TrimSpace(part.Text) != "" {
			return true
		}
	}
	return false
}

type geminiResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text       string `json:"text"`
				InlineData *struct {
					Data string `json:"data"`
				} `json:"inlineData"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
}

func geminiText(b []byte) string {
	var r geminiResponse
	if json.Unmarshal(b, &r) != nil || len(r.Candidates) == 0 {
		return ""
	}
	var text strings.Builder
	for _, part := range r.Candidates[0].Content.Parts {
		text.WriteString(part.Text)
	}
	return text.String()
}

func validateGeminiText(b []byte) bool { return strings.TrimSpace(geminiText(b)) != "" }

func validateGeminiImage(b []byte) bool {
	var r geminiResponse
	if json.Unmarshal(b, &r) != nil || len(r.Candidates) == 0 {
		return false
	}
	for _, part := range r.Candidates[0].Content.Parts {
		if part.InlineData != nil && part.InlineData.Data != "" {
			return true
		}
	}
	return false
}

func validateGeminiEmbedding(b []byte) bool {
	var r struct {
		Embedding struct {
			Values []float64 `json:"values"`
		} `json:"embedding"`
	}
	return json.Unmarshal(b, &r) == nil && numericVector(r.Embedding.Values)
}

func validateFalImage(b []byte) bool {
	var r struct {
		Images []struct {
			URL string `json:"url"`
		} `json:"images"`
	}
	return json.Unmarshal(b, &r) == nil && len(r.Images) > 0 && r.Images[0].URL != ""
}

func isOKObject(text string) bool {
	var v struct {
		OK *bool `json:"ok"`
	}
	return json.Unmarshal([]byte(strings.TrimSpace(text)), &v) == nil && v.OK != nil && *v.OK
}

func isSegmentArray(text string) bool {
	var segments []map[string]json.RawMessage
	if json.Unmarshal([]byte(strings.TrimSpace(text)), &segments) != nil || segments == nil {
		return false
	}
	for _, segment := range segments {
		var speaker, words string
		var start, end float64
		if json.Unmarshal(segment["a"], &speaker) != nil || json.Unmarshal(segment["s"], &start) != nil ||
			json.Unmarshal(segment["e"], &end) != nil || json.Unmarshal(segment["t"], &words) != nil {
			return false
		}
	}
	return true
}

// silentWAV returns 100 ms of 16 kHz mono 16-bit PCM silence.
func silentWAV() []byte {
	const sampleRate, samples = 16000, 1600
	dataSize := samples * 2
	var b bytes.Buffer
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(36+dataSize))
	b.WriteString("WAVEfmt ")
	for _, field := range []any{uint32(16), uint16(1), uint16(1), uint32(sampleRate), uint32(sampleRate * 2), uint16(2), uint16(16)} {
		_ = binary.Write(&b, binary.LittleEndian, field)
	}
	b.WriteString("data")
	_ = binary.Write(&b, binary.LittleEndian, uint32(dataSize))
	b.Write(make([]byte, dataSize))
	return b.Bytes()
}
