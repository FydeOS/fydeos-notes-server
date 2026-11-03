// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package byok

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	maxEmbeddingTextBytes = 64 << 10
	embeddingTimeout      = 30 * time.Second
)

func embeddingError(reason string) error { return fmt.Errorf("%w: %s", ErrEmbeddingRequest, reason) }

// EmbedText embeds text at EmbeddingDimensions using target's provider.
func EmbedText(ctx context.Context, target ChatTarget, text string) ([]float32, error) {
	if target.Credential == nil || target.Credential.Bytes() == nil {
		return nil, embeddingError("credential missing")
	}
	if strings.TrimSpace(text) == "" || len(text) > maxEmbeddingTextBytes {
		return nil, embeddingError("invalid text")
	}
	base := strings.TrimRight(target.BaseURL, "/")
	var path string
	var body any
	switch target.Provider {
	case "openai":
		path = "/embeddings"
		body = map[string]any{"model": target.ModelID, "input": text, "dimensions": EmbeddingDimensions}
	case "gemini":
		path = "/models/" + url.PathEscape(target.ModelID) + ":embedContent"
		body = map[string]any{"content": map[string]any{"parts": []any{map[string]any{"text": text}}}, "outputDimensionality": EmbeddingDimensions}
	default:
		return nil, embeddingError("unsupported provider")
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, embeddingError("encode request")
	}
	ctx, cancel := context.WithTimeout(ctx, embeddingTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(encoded))
	if err != nil {
		return nil, embeddingError("build request")
	}
	request.Header.Set("Content-Type", "application/json")
	setAuthHeaders(request.Header, target.Provider, target.Credential.Bytes())
	transport := target.Transport
	if transport == nil {
		transport = publicTransport
	}
	response, err := (&http.Client{Transport: transport, CheckRedirect: noRedirect}).Do(request)
	if err != nil {
		return nil, embeddingError("transport failure")
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, embeddingError("read response")
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return nil, embeddingError(fmt.Sprintf("provider status %d", response.StatusCode))
	}
	if len(payload) == 0 || len(payload) > maxResponseBytes {
		return nil, embeddingError("invalid response size")
	}
	var values []float64
	if target.Provider == "openai" {
		var r struct {
			Data []struct {
				Index     int       `json:"index"`
				Embedding []float64 `json:"embedding"`
			} `json:"data"`
		}
		if json.Unmarshal(payload, &r) != nil || len(r.Data) != 1 || r.Data[0].Index != 0 {
			return nil, embeddingError("invalid response")
		}
		values = r.Data[0].Embedding
	} else {
		var r struct {
			Embedding *struct {
				Values []float64 `json:"values"`
			} `json:"embedding"`
		}
		if json.Unmarshal(payload, &r) != nil || r.Embedding == nil {
			return nil, embeddingError("invalid response")
		}
		values = r.Embedding.Values
	}
	return toVector(values)
}

func toVector(values []float64) ([]float32, error) {
	if len(values) != EmbeddingDimensions {
		return nil, embeddingError("wrong vector dimensions")
	}
	vector := make([]float32, len(values))
	var norm float64
	for i, v := range values {
		if math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v) > math.MaxFloat32 {
			return nil, embeddingError("non-finite vector value")
		}
		vector[i] = float32(v)
		norm += float64(vector[i]) * float64(vector[i])
	}
	if norm == 0 {
		return nil, embeddingError("zero vector")
	}
	return vector, nil
}
