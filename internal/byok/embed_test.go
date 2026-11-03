// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package byok

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func vectorJSON(n int, value float64) string {
	values := make([]float64, n)
	for i := range values {
		values[i] = value
	}
	encoded, _ := json.Marshal(values)
	return string(encoded)
}

func TestEmbedText(t *testing.T) {
	var body string
	var status int
	var seen map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&seen)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	target := func(provider string) ChatTarget {
		return ChatTarget{Provider: provider, ModelID: "m", BaseURL: server.URL, Credential: NewSensitiveCredential([]byte("k")), Transport: server.Client().Transport}
	}
	status = 200
	body = `{"data":[{"index":0,"embedding":` + vectorJSON(1024, 0.5) + `}]}`
	vector, err := EmbedText(context.Background(), target("openai"), "hello")
	if err != nil || len(vector) != EmbeddingDimensions || vector[0] != 0.5 || seen["dimensions"] != float64(1024) {
		t.Fatalf("openai = %d, %v, %v", len(vector), err, seen)
	}
	body = `{"embedding":{"values":` + vectorJSON(1024, 0.25) + `}}`
	vector, err = EmbedText(context.Background(), target("gemini"), "hello")
	if err != nil || len(vector) != EmbeddingDimensions || seen["outputDimensionality"] != float64(1024) {
		t.Fatalf("gemini = %d, %v", len(vector), err)
	}
	bad := map[string]string{
		"1023 dims": `{"data":[{"index":0,"embedding":` + vectorJSON(1023, 0.5) + `}]}`,
		"nan":       `{"data":[{"index":0,"embedding":[NaN]}]}`,
		"zero":      `{"data":[{"index":0,"embedding":` + vectorJSON(1024, 0) + `}]}`,
		"two items": `{"data":[{"index":0,"embedding":` + vectorJSON(1024, 0.5) + `},{"index":1,"embedding":` + vectorJSON(1024, 0.5) + `}]}`,
		"overflow":  `{"data":[{"index":0,"embedding":` + strings.Replace(vectorJSON(1024, 0.5), "0.5", "1e300", 1) + `}]}`,
		"empty":     ``,
	}
	for name, b := range bad {
		body = b
		if _, err := EmbedText(context.Background(), target("openai"), "hello"); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	body = `{"data":[{"index":0,"embedding":` + vectorJSON(1024, 0.5) + `}]}`
	status = 500
	if _, err := EmbedText(context.Background(), target("openai"), "hello"); err == nil {
		t.Error("non-2xx accepted")
	}
	status = 200
	if _, err := EmbedText(context.Background(), target("anthropic"), "hello"); err == nil {
		t.Error("anthropic accepted")
	}
	if _, err := EmbedText(context.Background(), target("openai"), "  "); err == nil {
		t.Error("blank text accepted")
	}
	if _, err := EmbedText(context.Background(), target("openai"), strings.Repeat("a", 64<<10+1)); err == nil {
		t.Error("long text accepted")
	}
	noCredential := target("openai")
	noCredential.Credential = nil
	if _, err := EmbedText(context.Background(), noCredential, "hello"); err == nil {
		t.Error("nil credential accepted")
	}
}
