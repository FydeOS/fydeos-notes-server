// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package search

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/fydeos/fydeos-notes-server/internal/byok"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	embeddingFeature        = "embedding"
	embeddingContract       = 1
	documentRecipeRevision  = "document-projection-v1"
	maxChunkBytes           = 48 * 1024
	chunkOverlapRunes       = 512
	maxChunksPerSource      = 2048
	maxSourceTextBytes      = 64 << 20
	maxArtifactBytes        = 50 << 20
	maxEmbeddingSnippetRune = 800
)

// Error codes persisted in last_error_code columns.
const (
	codeProviderUnavailable = "provider_unavailable"
	codeRouteChanged        = "route_changed"
	codeUnsupportedSource   = "unsupported_source"
	codeDocumentInvalid     = "document_projection_invalid"
	codeArtifactExtraction  = "artifact_extraction_failed"
	codeArtifactFormat      = "artifact_format_unsupported"
	codeCommitFailed        = "commit_failed"
	codeInvalidEmbedCount   = "invalid_embedding_count"
)

type rowReader interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type execQueryRower interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type txDB interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	execQueryRower
}

type embeddingDB interface {
	txDB
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// TargetResolver resolves the embedding route for a workspace owner.
type TargetResolver interface {
	ResolveServerTarget(ctx context.Context, workspaceID, userID, feature string) (byok.ChatTarget, error)
}

type progressNotifier interface {
	PublishWorkspaceEmbeddingProgressChanged(string)
}

type embedFunc func(ctx context.Context, target byok.ChatTarget, text string) ([]float32, error)

// embeddingTablesExist reports whether the optional pgvector tables exist.
func embeddingTablesExist(ctx context.Context, db rowReader) (bool, error) {
	var ok bool
	err := db.QueryRow(ctx, `SELECT
		to_regclass('embedding_workspace_states') IS NOT NULL AND
		to_regclass('embedding_indexes') IS NOT NULL AND
		to_regclass('embedding_sources') IS NOT NULL AND
		to_regclass('embedding_projections') IS NOT NULL AND
		to_regclass('embedding_chunks') IS NOT NULL`).Scan(&ok)
	return ok, err
}

func routeSource(source string) string {
	if source == "byok" || strings.HasPrefix(source, "byok_") {
		return "byok"
	}
	return "managed"
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func endpointFingerprint(target byok.ChatTarget) string {
	data, _ := json.Marshal(struct {
		BaseURL string `json:"baseUrl"`
		Kind    string `json:"kind"`
		URL     string `json:"url"`
		Dialect string `json:"dialect"`
	}{target.BaseURL, target.Endpoint.Kind, target.Endpoint.URL, target.Endpoint.Dialect})
	return sha256Hex(data)
}

// routeFingerprint identifies the vector space produced by a route.
func routeFingerprint(target byok.ChatTarget) string {
	data, _ := json.Marshal(struct {
		Source     string `json:"source"`
		ProfileID  string `json:"profileId"`
		Provider   string `json:"provider"`
		ModelID    string `json:"modelId"`
		Endpoint   string `json:"endpoint"`
		Dimensions int    `json:"dimensions"`
		Contract   int    `json:"contract"`
	}{routeSource(target.Source), target.ProfileID, target.Provider, target.ModelID,
		endpointFingerprint(target), byok.EmbeddingDimensions, embeddingContract})
	return sha256Hex(data)
}

func vectorLiteral(values []float32) (string, error) {
	if len(values) != byok.EmbeddingDimensions {
		return "", fmt.Errorf("embedding has %d dimensions, want %d", len(values), byok.EmbeddingDimensions)
	}
	var b strings.Builder
	b.Grow(len(values) * 10)
	b.WriteByte('[')
	for i, v := range values {
		f := float64(v)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return "", errors.New("embedding contains a non-finite value")
		}
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(f, 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String(), nil
}

// workspaceOwner returns the earliest active owner of a workspace.
func workspaceOwner(ctx context.Context, db rowReader, workspaceID string) (string, error) {
	var owner string
	err := db.QueryRow(ctx, `SELECT user_id FROM workspace_members
		WHERE workspace_id=$1 AND role='owner' AND state='active'
		ORDER BY created_at, user_id LIMIT 1`, workspaceID).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return owner, err
}

func routeUnavailable(err error) bool {
	return errors.Is(err, byok.ErrTargetUnavailable) || errors.Is(err, byok.ErrNoCompatibleServerTarget)
}

// splitText cuts text into pieces of at most maxChunkBytes, overlapping by
// chunkOverlapRunes runes, never splitting a rune.
func splitText(text string) []string {
	if text == "" {
		return nil
	}
	var pieces []string
	start := 0
	for {
		end := start
		for end < len(text) {
			_, size := utf8.DecodeRuneInString(text[end:])
			if end+size-start > maxChunkBytes {
				break
			}
			end += size
		}
		pieces = append(pieces, text[start:end])
		if end >= len(text) {
			return pieces
		}
		next := end
		for i := 0; i < chunkOverlapRunes && next > start; i++ {
			_, size := utf8.DecodeLastRuneInString(text[:next])
			next -= size
		}
		if next <= start {
			next = end
		}
		start = next
	}
}

func backoffSeconds(attempts, maxExp int, capSeconds float64) float64 {
	return math.Min(capSeconds, 5*math.Pow(2, float64(min(max(attempts, 0), maxExp))))
}

func truncateRunes(text string, n int) string {
	if utf8.RuneCountInString(text) <= n {
		return text
	}
	return string([]rune(text)[:n])
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// runLoop drives a RunOne-style function until ctx is cancelled.
func runLoop(ctx context.Context, idle, minBackoff, maxBackoff time.Duration, step func(context.Context) (bool, error)) error {
	backoff := minBackoff
	for {
		if ctx.Err() != nil {
			return nil
		}
		worked, err := step(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			if !sleepCtx(ctx, backoff) {
				return nil
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}
		backoff = minBackoff
		if worked {
			continue
		}
		if !sleepCtx(ctx, idle) {
			return nil
		}
	}
}
