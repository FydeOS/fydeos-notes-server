// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

// Package search implements workspace keyword, structured and semantic search
// over the Postgres search projection.
package search

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	// ErrInvalidQuery reports invalid structured search or aggregate input.
	ErrInvalidQuery = errors.New("invalid search query")
	// ErrInvalidSearchDocsInput reports invalid keyword or semantic search input.
	ErrInvalidSearchDocsInput = errors.New("invalid search docs input")
	// ErrEmbeddingUnavailable reports that semantic search cannot be used.
	ErrEmbeddingUnavailable = errors.New("semantic search is unavailable")
)

const (
	maxKeywordBytes  = 4096
	maxSearchResults = 10000
)

// rowQuerier is the minimal read interface shared by the search services.
type rowQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// DocResult is one document returned by keyword or semantic search.
type DocResult struct {
	WorkspaceID string
	DocID       string
	BlockID     string
	Title       string
	Highlight   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	CreatedBy   *string
	UpdatedBy   *string
}

// DocsService runs keyword search over the projection.
type DocsService struct {
	db rowQuerier
}

func NewDocsService(db rowQuerier) *DocsService {
	return &DocsService{db: db}
}

func (s *DocsService) SearchDocs(ctx context.Context, workspaceID, keyword string, limit int) ([]DocResult, error) {
	return s.SearchDocsPage(ctx, workspaceID, keyword, limit, 0)
}

const keywordSearchSQL = `
WITH q AS (SELECT websearch_to_tsquery('simple', $2) AS tsq),
ranked AS (
	SELECT u.doc_id, u.block_id, u.title, u.content,
		ts_rank(u.content_vector, q.tsq) AS rank,
		row_number() OVER (PARTITION BY u.doc_id ORDER BY ts_rank(u.content_vector, q.tsq) DESC, u.block_id ASC) AS pos
	FROM search_projection_units u CROSS JOIN q
	WHERE u.workspace_id = $1 AND u.content_vector @@ q.tsq
)
SELECT r.doc_id, r.block_id, r.title,
	ts_headline('simple',
		replace(replace(replace(r.content, '&', '&amp;'), '<', '&lt;'), '>', '&gt;'),
		q.tsq, 'StartSel=<b>, StopSel=</b>, MaxFragments=2, MinWords=8, MaxWords=28'),
	sn.created_at, sn.updated_at, sn.created_by, sn.updated_by
FROM ranked r
CROSS JOIN q
JOIN snapshots sn ON sn.workspace_id = $1 AND sn.guid = r.doc_id
WHERE r.pos = 1
ORDER BY r.rank DESC, r.doc_id ASC
LIMIT $3 OFFSET $4`

func (s *DocsService) SearchDocsPage(ctx context.Context, workspaceID, keyword string, limit, offset int) ([]DocResult, error) {
	keyword = strings.TrimSpace(keyword)
	if s == nil || s.db == nil || workspaceID == "" || len(keyword) > maxKeywordBytes ||
		limit < 1 || limit > maxSearchResults || offset < 0 {
		return nil, ErrInvalidSearchDocsInput
	}
	results := []DocResult{}
	if keyword == "" {
		return results, nil
	}
	rows, err := s.db.Query(ctx, keywordSearchSQL, workspaceID, keyword, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("keyword search: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		r := DocResult{WorkspaceID: workspaceID}
		if err := rows.Scan(&r.DocID, &r.BlockID, &r.Title, &r.Highlight, &r.CreatedAt, &r.UpdatedAt, &r.CreatedBy, &r.UpdatedBy); err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	return results, rows.Err()
}
