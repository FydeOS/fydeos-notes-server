// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package byok

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type probeFunc func(context.Context, ProbeTarget) ProbeStatus

func (f probeFunc) Probe(ctx context.Context, target ProbeTarget) ProbeStatus { return f(ctx, target) }

func TestProbeCheckRules(t *testing.T) {
	definition, err := ValidateDefinition("openai", customDef(
		model("chat", textCap()),
		ModelDeclaration{ModelID: "off", Capabilities: []Capability{textCap()}},
		model("vision", Capability{Input: []string{"text", "image"}, Output: []string{"text"}, AttachmentKinds: []string{"image"}, AttachmentSource: []string{"data"}}),
	))
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	run := probeRun{provider: "openai", definition: definition, runner: probeFunc(func(context.Context, ProbeTarget) ProbeStatus {
		calls++
		return failedStatus("authentication")
	})}
	checks, err := validateChecks(definition, []ProbeCheckInput{
		{"vision", "vision"}, {"off", "chat"}, {"chat", "embedding"}, {"chat", "chat"},
	})
	if err != nil {
		t.Fatal(err)
	}
	connection, models := run.run(context.Background(), checks)
	if calls != 1 || connection.Kind != "failed" || *connection.ErrorKind != "authentication" {
		t.Fatalf("connection = %+v calls=%d", connection, calls)
	}
	if len(models) != 3 || models[0].ModelID != "chat" || models[1].ModelID != "off" || models[2].ModelID != "vision" {
		t.Fatalf("models = %+v", models)
	}
	if k := *models[0].Checks[0].Status.ErrorKind; k != "capability_not_declared" {
		t.Fatalf("undeclared = %s", k)
	}
	if k := *models[1].Checks[0].Status.ErrorKind; k != "model_disabled" {
		t.Fatalf("disabled = %s", k)
	}
	if models[2].Checks[0].Status.Kind != "not_tested" {
		t.Fatal("vision was tested")
	}
	many := make([]ProbeCheckInput, 33)
	for i := range many {
		many[i] = ProbeCheckInput{fmt.Sprint(i), "chat"}
	}
	for name, bad := range map[string][]ProbeCheckInput{
		"too many":  many,
		"unknown":   {{"missing", "chat"}},
		"duplicate": {{"chat", "chat"}, {"chat", "chat"}},
		"operation": {{"chat", "dance"}},
	} {
		if _, err := validateChecks(definition, bad); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s accepted", name)
		}
	}
	verified := probeRun{provider: "openai", definition: definition, runner: probeFunc(func(context.Context, ProbeTarget) ProbeStatus {
		return ProbeStatus{Kind: "verified", TestedAtMS: nowMS()}
	})}
	if connection, _ := verified.run(context.Background(), checks); connection.Kind != "verified" || connection.TestedAtMS == nil {
		t.Fatalf("verified connection = %+v", connection)
	}
}

func runnerFor(server *httptest.Server) HTTPProbeRunner {
	return HTTPProbeRunner{
		baseURLs:  map[string]string{"openai": server.URL + "/v1", "anthropic": server.URL, "gemini": server.URL + "/v1beta", "fal": server.URL},
		transport: server.Client().Transport,
	}
}

func probeKind(status ProbeStatus) string {
	if status.ErrorKind != nil {
		return status.Kind + ":" + *status.ErrorKind
	}
	return status.Kind
}

func TestHTTPProbeRunnerStatusMapping(t *testing.T) {
	status := 200
	body := `{"output_text":"OK"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer key" {
			w.WriteHeader(401)
			return
		}
		if status == 302 {
			http.Redirect(w, r, "https://elsewhere.example/", 302)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	runner := runnerFor(server)
	target := ProbeTarget{Provider: "openai", Endpoint: Endpoint{Kind: "provider_default"}, ModelID: "m", Credential: []byte("key"), Operation: "chat"}
	expect := map[int]string{200: "verified", 401: "failed:authentication", 403: "failed:permission", 404: "failed:not_found",
		429: "failed:rate_limited", 503: "failed:unavailable", 400: "failed:rejected", 302: "failed:rejected"}
	for code, want := range expect {
		status = code
		if got := probeKind(runner.Probe(context.Background(), target)); got != want {
			t.Errorf("%d => %s, want %s", code, got, want)
		}
	}
	status = 200
	for _, bad := range []string{`{"output_text":""}`, `not json`, `{"ok":` + strings.Repeat(" ", 2<<20) + `true}`} {
		body = bad
		if got := probeKind(runner.Probe(context.Background(), target)); got != "failed:invalid_response" {
			t.Errorf("body %.20q => %s", bad, got)
		}
	}
	server.Close()
	if got := probeKind(runner.Probe(context.Background(), target)); got != "failed:transport" {
		t.Errorf("closed server => %s", got)
	}
}

func TestHTTPProbeRunnerShapes(t *testing.T) {
	responses := map[string]string{
		"/v1/responses":                    `{"output":[{"type":"function_call","name":"probe"},{"content":[{"text":"{\"ok\":true}"}]}]}`,
		"/v1/embeddings":                   `{"data":[{"embedding":[0.1,0.2]}]}`,
		"/v1/images/generations":           `{"data":[{"b64_json":"aGk="}]}`,
		"/v1/chat/completions":             `{"choices":[{"message":{"content":"OK"},"logprobs":{"content":[{"logprob":-0.1}]}}]}`,
		"/v1/messages":                     `{"content":[{"type":"text","text":"OK"}]}`,
		"/v1beta/models/g:generateContent": `{"candidates":[{"content":{"parts":[{"text":"[]"}]}}]}`,
		"/v1beta/models/g:embedContent":    `{"embedding":{"values":[1]}}`,
		"/flux-1/schnell":                  `{"images":[{"url":"https://x/img.png"}]}`,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := responses[r.URL.Path]
		if !ok {
			w.WriteHeader(404)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	runner := runnerFor(server)
	def := Endpoint{Kind: "provider_default"}
	cases := []struct {
		provider, model, op string
		endpoint            Endpoint
		want                string
	}{
		{"openai", "m", "tool_calling", def, "verified"},
		{"openai", "m", "structured", def, "verified"},
		{"openai", "m", "embedding", def, "verified"},
		{"openai", "m", "image", def, "verified"},
		{"openai", "m", "rerank", def, "verified"},
		{"openai", "m", "transcript", def, "failed:unsupported_operation"},
		{"anthropic", "m", "chat", def, "verified"},
		{"anthropic", "m", "structured", def, "failed:unsupported_operation"},
		{"gemini", "g", "transcript", def, "verified"},
		{"gemini", "g", "structured", def, "failed:invalid_response"},
		{"gemini", "g", "embedding", def, "verified"},
		{"fal", "flux-1/schnell", "image", def, "verified"},
		{"fal", "workflows/x", "image", def, "failed:unsupported_operation"},
		{"fal", "a/../b", "image", def, "failed:unsupported_operation"},
		{"openai", "m", "chat", Endpoint{Kind: "openai_compatible", URL: server.URL + "/v1", Dialect: "chat_completions"}, "verified"},
	}
	for _, tc := range cases {
		target := ProbeTarget{Provider: tc.provider, Endpoint: tc.endpoint, ModelID: tc.model, Credential: []byte("k"), Operation: tc.op}
		if got := probeKind(runner.Probe(context.Background(), target)); got != tc.want {
			t.Errorf("%s/%s => %s, want %s", tc.provider, tc.op, got, tc.want)
		}
	}
	// A custom endpoint on loopback over http only passes with private endpoints enabled.
	guarded := HTTPProbeRunner{}
	custom := ProbeTarget{Provider: "openai", Endpoint: Endpoint{Kind: "openai_compatible", URL: server.URL + "/v1", Dialect: "chat_completions"}, ModelID: "m", Credential: []byte("k"), Operation: "chat"}
	if got := probeKind(guarded.Probe(context.Background(), custom)); got != "failed:transport" {
		t.Errorf("private custom endpoint => %s", got)
	}
	custom.AllowPrivateEndpoint = true
	if got := probeKind(guarded.Probe(context.Background(), custom)); got != "verified" {
		t.Errorf("allowed private custom endpoint => %s", got)
	}
}
