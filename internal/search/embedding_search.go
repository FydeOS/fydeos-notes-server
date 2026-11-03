// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package search

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/fydeos/fydeos-notes-server/internal/byok"
	"github.com/jackc/pgx/v5"
)

const (
	maxSemanticQueryBytes = 8 * 1024
	maxSemanticDocResults = 500
	maxSemanticDocIDs     = 500
	maxArtifactCandidates = 20
)

// ArtifactCandidate is one artifact chunk close to a semantic query.
type ArtifactCandidate struct {
	ArtifactID string
	Content    string
	Distance   float64
	Chunk      int
	Name       string
	MediaType  string
}

// EmbeddingCandidateSearcher runs semantic search over embedded chunks.
type EmbeddingCandidateSearcher struct {
	db      embeddingDB
	targets TargetResolver
	embed   embedFunc
}

func NewEmbeddingCandidateSearcher(db embeddingDB, targets TargetResolver) *EmbeddingCandidateSearcher {
	return &EmbeddingCandidateSearcher{db: db, targets: targets, embed: byok.EmbedText}
}

func unavailable(reason string, err error) error {
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrEmbeddingUnavailable, reason, err)
	}
	return fmt.Errorf("%w: %s", ErrEmbeddingUnavailable, reason)
}

// queryVector embeds query with the workspace's active ready index route and
// returns the index id and vector literal.
func (s *EmbeddingCandidateSearcher) queryVector(ctx context.Context, workspaceID, query string) (string, string, error) {
	exists, err := embeddingTablesExist(ctx, s.db)
	if err != nil {
		return "", "", err
	}
	if !exists {
		return "", "", unavailable("pgvector tables are absent", nil)
	}
	var indexID, fingerprint string
	err = s.db.QueryRow(ctx, `SELECT i.id::text, i.fingerprint FROM embedding_workspace_states s
		JOIN embedding_indexes i ON i.id=s.active_index_id
		WHERE s.workspace_id=$1 AND s.runtime_state='active' AND i.health_status='ready' AND i.inactive_at IS NULL`,
		workspaceID).Scan(&indexID, &fingerprint)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", unavailable("no ready index", nil)
	}
	if err != nil {
		return "", "", err
	}
	owner, err := workspaceOwner(ctx, s.db, workspaceID)
	if err != nil {
		return "", "", err
	}
	if owner == "" {
		return "", "", unavailable("workspace has no active owner", nil)
	}
	target, err := s.targets.ResolveServerTarget(ctx, workspaceID, owner, embeddingFeature)
	if err != nil {
		return "", "", unavailable("route unavailable", err)
	}
	if routeFingerprint(target) != fingerprint {
		return "", "", unavailable("route changed", nil)
	}
	values, err := s.embed(ctx, target, query)
	if err != nil {
		return "", "", unavailable("query embedding failed", err)
	}
	literal, err := vectorLiteral(values)
	if err != nil {
		return "", "", unavailable("query embedding invalid", err)
	}
	return indexID, literal, nil
}

func validSemanticInput(s *EmbeddingCandidateSearcher, workspaceID, query string, limit, maxLimit int) bool {
	return s != nil && s.db != nil && s.targets != nil && workspaceID != "" &&
		strings.TrimSpace(query) != "" && len(query) <= maxSemanticQueryBytes && limit >= 1 && limit <= maxLimit
}

const semanticDocsSQL = `
WITH candidates AS (
	SELECT ch.doc_id, COALESCE(ch.block_id, '') AS block_id, ch.content, ch.chunk_index,
		ch.embedding OPERATOR(public.<=>) $3::text::public.vector AS distance
	FROM embedding_chunks ch
	JOIN embedding_projections p ON p.source_id=ch.source_id AND p.index_id=ch.index_id
		AND p.active_generation_token=ch.generation_token AND p.status='ready'
	JOIN embedding_sources src ON src.id=ch.source_id AND src.deleted_at IS NULL
	WHERE ch.workspace_id=$1 AND ch.index_id=$2::uuid AND ch.source_kind='document'
		AND (cardinality($4::text[]) = 0 OR ch.doc_id = ANY($4::text[]))
),
best AS (
	SELECT DISTINCT ON (doc_id) * FROM candidates ORDER BY doc_id, distance, chunk_index
)
SELECT b.doc_id, b.block_id, b.content,
	COALESCE((SELECT u.title FROM search_projection_units u
		WHERE u.workspace_id=$1 AND u.doc_id=b.doc_id AND u.block_id=b.block_id LIMIT 1), ''),
	sn.created_at, sn.updated_at, sn.created_by, sn.updated_by
FROM best b
JOIN snapshots sn ON sn.workspace_id=$1 AND sn.guid=b.doc_id
ORDER BY b.distance, b.doc_id
LIMIT $5`

func (s *EmbeddingCandidateSearcher) SearchDocs(ctx context.Context, workspaceID, query string, docIDs []string, limit int) ([]DocResult, error) {
	if !validSemanticInput(s, workspaceID, query, limit, maxSemanticDocResults) || len(docIDs) > maxSemanticDocIDs {
		return nil, ErrInvalidSearchDocsInput
	}
	indexID, vector, err := s.queryVector(ctx, workspaceID, query)
	if err != nil {
		return nil, err
	}
	if docIDs == nil {
		docIDs = []string{}
	}
	rows, err := s.db.Query(ctx, semanticDocsSQL, workspaceID, indexID, vector, docIDs, limit)
	if err != nil {
		return nil, fmt.Errorf("semantic doc search: %w", err)
	}
	defer rows.Close()
	results := []DocResult{}
	for rows.Next() {
		r := DocResult{WorkspaceID: workspaceID}
		var content string
		if err := rows.Scan(&r.DocID, &r.BlockID, &content, &r.Title, &r.CreatedAt, &r.UpdatedAt, &r.CreatedBy, &r.UpdatedBy); err != nil {
			return nil, err
		}
		r.Highlight = truncateRunes(strings.TrimSpace(content), maxEmbeddingSnippetRune)
		results = append(results, r)
	}
	return results, rows.Err()
}

const semanticArtifactsSQL = `
SELECT ch.artifact_id::text, ch.content, ch.embedding OPERATOR(public.<=>) $3::text::public.vector AS distance, ch.chunk_index,
	COALESCE(NULLIF(a.display_name, ''), NULLIF(src.file_name, ''), NULLIF(a.file_name, ''), ''),
	COALESCE(NULLIF(src.mime_type, ''), a.canonical_media_type, '')
FROM embedding_chunks ch
JOIN embedding_projections p ON p.source_id=ch.source_id AND p.index_id=ch.index_id
	AND p.active_generation_token=ch.generation_token AND p.status='ready'
JOIN embedding_sources src ON src.id=ch.source_id AND src.deleted_at IS NULL
JOIN workspace_artifacts a ON a.id=ch.artifact_id AND a.workspace_id=$1 AND a.status='ready' AND a.library_owned
WHERE ch.workspace_id=$1 AND ch.index_id=$2::uuid AND ch.source_kind='artifact'
ORDER BY distance, ch.artifact_id, ch.chunk_index
LIMIT $4`

func (s *EmbeddingCandidateSearcher) SearchArtifacts(ctx context.Context, workspaceID, query string, limit int) ([]ArtifactCandidate, error) {
	if !validSemanticInput(s, workspaceID, query, limit, maxArtifactCandidates) {
		return nil, ErrInvalidSearchDocsInput
	}
	indexID, vector, err := s.queryVector(ctx, workspaceID, query)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, semanticArtifactsSQL, workspaceID, indexID, vector, limit)
	if err != nil {
		return nil, fmt.Errorf("semantic artifact search: %w", err)
	}
	defer rows.Close()
	results := []ArtifactCandidate{}
	for rows.Next() {
		var c ArtifactCandidate
		if err := rows.Scan(&c.ArtifactID, &c.Content, &c.Distance, &c.Chunk, &c.Name, &c.MediaType); err != nil {
			return nil, err
		}
		c.Content = truncateRunes(c.Content, maxEmbeddingSnippetRune)
		results = append(results, c)
	}
	return results, rows.Err()
}

// EmbeddingProgress is the realtime payload for embedding progress.
type EmbeddingProgress struct {
	Total    int64 `json:"total"`
	Embedded int64 `json:"embedded"`
}

// EmbeddingProgressReader counts embedded sources for a workspace.
type EmbeddingProgressReader struct {
	db rowReader
}

func NewEmbeddingProgressReader(db rowReader) *EmbeddingProgressReader {
	return &EmbeddingProgressReader{db: db}
}

func (r *EmbeddingProgressReader) ReadEmbeddingProgress(ctx context.Context, workspaceID string) (int64, int64, error) {
	if r == nil || r.db == nil || workspaceID == "" {
		return 0, 0, nil
	}
	exists, err := embeddingTablesExist(ctx, r.db)
	if err != nil || !exists {
		return 0, 0, err
	}
	var total, embedded int64
	err = r.db.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE p.status='ready' AND p.applied_content_revision=src.content_revision)
		FROM embedding_sources src
		LEFT JOIN embedding_workspace_states s ON s.workspace_id=src.workspace_id
		LEFT JOIN embedding_projections p ON p.source_id=src.id AND p.index_id=s.active_index_id
		WHERE src.workspace_id=$1 AND src.deleted_at IS NULL`, workspaceID).Scan(&total, &embedded)
	return total, embedded, err
}
