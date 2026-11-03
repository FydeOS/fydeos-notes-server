// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package copilot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/fydeos/fydeos-notes-server/internal/documents"
	"github.com/fydeos/fydeos-notes-server/internal/search"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
	"github.com/google/uuid"
)

const (
	maxChatDocSearchQuery  = 2000
	maxChatDocSearchHits   = 10
	maxChatDocSearchScan   = 500
	maxChatDocReadChars    = 40000
	maxChatToolOutputBytes = 24000
	maxCanvasReadLimit     = 100
	maxCanvasPreviewChars  = 4000
	maxCanvasRelationIDs   = 200
)

type chatDocSearcher interface {
	SearchDocsPage(context.Context, string, string, int, int) ([]search.DocResult, error)
}

type chatSemanticDocSearcher interface {
	SearchDocs(context.Context, string, string, []string, int) ([]search.DocResult, error)
}

type chatDocReader interface {
	ReadDocumentPermissions(context.Context, string, string, string) (map[string]bool, error)
	ReadDocumentPermissionsBatch(context.Context, string, string, []string) (map[string]map[string]bool, error)
	ReadDocumentMarkdown(context.Context, string, string) (string, bool, error)
	ReadDocumentCanvas(context.Context, string, string, string) (documents.CanvasProjection, bool, error)
}

type chatArtifactSearcher interface {
	SearchArtifacts(context.Context, string, string, int) ([]search.ArtifactCandidate, error)
}

type chatArtifactReader interface {
	ReadExtracted(context.Context, string, string, int, string) (WorkspaceArtifactContent, error)
}

type scopedChatArtifactReader interface {
	ReadExtractedScoped(context.Context, string, string, string, int, string, ArtifactRetrievalScope) (WorkspaceArtifactContent, error)
}

type chatWorkspaceReader interface {
	ReadWorkspace(context.Context, string, string) (workspaces.Workspace, error)
}

type ChatToolExecutor interface {
	Execute(context.Context, string, string, string, json.RawMessage) (any, error)
}

type scopedChatToolExecutor interface {
	ExecuteTurn(context.Context, string, string, string, ArtifactRetrievalScope, string, json.RawMessage) (any, error)
}

type ArtifactRetrievalScope struct {
	Mode                string   `json:"mode"`
	RequiredArtifactIDs []string `json:"requiredArtifactIds"`
}

type DocumentChatTools struct {
	searcher         chatDocSearcher
	semanticSearcher chatSemanticDocSearcher
	reader           chatDocReader
	artifactSearcher chatArtifactSearcher
	artifactReader   chatArtifactReader
	workspaceReader  chatWorkspaceReader
}

func NewDocumentChatTools(searcher chatDocSearcher, reader chatDocReader) *DocumentChatTools {
	return &DocumentChatTools{searcher: searcher, reader: reader}
}

func (t *DocumentChatTools) SetSemanticSearcher(searcher chatSemanticDocSearcher) {
	if t != nil {
		t.semanticSearcher = searcher
	}
}

func (t *DocumentChatTools) SetArtifactTools(searcher chatArtifactSearcher, artifacts chatArtifactReader, workspaceReader chatWorkspaceReader) {
	if t != nil {
		t.artifactSearcher, t.artifactReader, t.workspaceReader = searcher, artifacts, workspaceReader
	}
}

func (t *DocumentChatTools) Execute(ctx context.Context, userID, workspaceID, name string, raw json.RawMessage) (any, error) {
	return t.ExecuteTurn(ctx, userID, workspaceID, "", ArtifactRetrievalScope{Mode: "workspace"}, name, raw)
}

func (t *DocumentChatTools) ExecuteTurn(ctx context.Context, userID, workspaceID, messageID string, retrieval ArtifactRetrievalScope, name string, raw json.RawMessage) (any, error) {
	if t == nil || t.reader == nil || userID == "" || workspaceID == "" {
		return nil, errors.New("document tools are unavailable")
	}
	if retrieval.Mode != "required" {
		retrieval = ArtifactRetrievalScope{Mode: "workspace"}
	}
	switch name {
	case "doc_search":
		return t.search(ctx, userID, workspaceID, raw)
	case "doc_read":
		return t.read(ctx, userID, workspaceID, raw)
	case "doc_canvas_read":
		return t.readCanvas(ctx, userID, workspaceID, raw)
	case "artifact_search":
		return t.searchArtifacts(ctx, userID, workspaceID, messageID, retrieval, raw)
	case "artifact_read":
		return t.readArtifact(ctx, userID, workspaceID, messageID, retrieval, raw)
	default:
		return nil, fmt.Errorf("unsupported Copilot tool %q", name)
	}
}

func (t *DocumentChatTools) authorizeArtifactWorkspace(ctx context.Context, userID, workspaceID string) error {
	if t.workspaceReader == nil {
		return errors.New("workspace artifact authorization is unavailable")
	}
	_, err := t.workspaceReader.ReadWorkspace(ctx, userID, workspaceID)
	return err
}

func (t *DocumentChatTools) searchArtifacts(ctx context.Context, userID, workspaceID, messageID string, retrieval ArtifactRetrievalScope, raw json.RawMessage) (any, error) {
	var input struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := decodeStrictToolInput(raw, &input); err != nil {
		return nil, errors.New("invalid artifact search input")
	}
	input.Query = strings.TrimSpace(input.Query)
	if input.Query == "" || len([]rune(input.Query)) > maxChatDocSearchQuery {
		return nil, errors.New("invalid artifact search input")
	}
	if input.Limit == 0 {
		input.Limit = 5
	}
	if input.Limit < 1 || input.Limit > maxChatDocSearchHits || t.artifactReader == nil || (retrieval.Mode != "required" && t.artifactSearcher == nil) {
		return nil, errors.New("artifact search is unavailable")
	}
	if err := t.authorizeArtifactWorkspace(ctx, userID, workspaceID); err != nil {
		return nil, err
	}
	if retrieval.Mode == "required" {
		hits := make([]map[string]any, 0, min(input.Limit, len(retrieval.RequiredArtifactIDs)))
		for _, artifactID := range retrieval.RequiredArtifactIDs {
			if len(hits) >= input.Limit {
				break
			}
			if _, err := uuid.Parse(artifactID); err != nil {
				continue
			}
			content, err := t.readArtifactContent(ctx, workspaceID, messageID, artifactID, 4000, "", retrieval)
			if err != nil {
				continue
			}
			hits = append(hits, map[string]any{"artifact_id": artifactID, "excerpt": content.Content, "distance": 0, "chunk": 0, "source": map[string]any{"type": "artifact", "workspace_id": workspaceID, "artifact_id": artifactID, "name": content.Name, "mime_type": content.MediaType}})
		}
		return map[string]any{"degraded": true, "hits": hits}, nil
	}
	candidates, err := t.artifactSearcher.SearchArtifacts(ctx, workspaceID, input.Query, input.Limit)
	if errors.Is(err, search.ErrEmbeddingUnavailable) {
		return map[string]any{"degraded": true, "hits": []any{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("search workspace artifacts: %w", err)
	}
	hits := make([]map[string]any, 0, len(candidates))
	for _, candidate := range candidates {
		if _, err := uuid.Parse(candidate.ArtifactID); err != nil {
			continue
		}
		hits = append(hits, map[string]any{
			"artifact_id": candidate.ArtifactID,
			"excerpt":     candidate.Content,
			"distance":    candidate.Distance,
			"chunk":       candidate.Chunk,
			"source":      map[string]any{"type": "artifact", "workspace_id": workspaceID, "artifact_id": candidate.ArtifactID, "name": candidate.Name, "mime_type": candidate.MediaType},
		})
	}
	return map[string]any{"degraded": false, "hits": hits}, nil
}

func (t *DocumentChatTools) readArtifact(ctx context.Context, userID, workspaceID, messageID string, retrieval ArtifactRetrievalScope, raw json.RawMessage) (any, error) {
	var input struct {
		ArtifactID string `json:"artifact_id"`
		MaxChars   int    `json:"max_chars"`
		Cursor     string `json:"cursor"`
	}
	if err := decodeStrictToolInput(raw, &input); err != nil || len(input.Cursor) > 128 {
		return nil, errors.New("invalid artifact read input")
	}
	if _, err := uuid.Parse(input.ArtifactID); err != nil {
		return nil, errors.New("invalid artifact read input")
	}
	if input.MaxChars == 0 {
		input.MaxChars = 20_000
	}
	if input.MaxChars < 1 || input.MaxChars > 100_000 || t.artifactReader == nil {
		return nil, errors.New("artifact read limit is out of range")
	}
	if err := t.authorizeArtifactWorkspace(ctx, userID, workspaceID); err != nil {
		return nil, err
	}
	content, err := t.readArtifactContent(ctx, workspaceID, messageID, input.ArtifactID, input.MaxChars, input.Cursor, retrieval)
	if err != nil {
		return nil, fmt.Errorf("read workspace artifact: %w", err)
	}
	result := map[string]any{
		"artifact_id": input.ArtifactID, "content": content.Content, "revision": content.Revision,
		"name": content.Name, "mime_type": content.MediaType, "truncated": content.Truncated,
		"source": map[string]any{"type": "artifact", "workspace_id": workspaceID, "artifact_id": input.ArtifactID, "name": content.Name, "mime_type": content.MediaType, "revision": content.Revision},
	}
	if content.NextCursor != "" {
		result["next_cursor"] = content.NextCursor
	}
	return result, nil
}

func (t *DocumentChatTools) readArtifactContent(ctx context.Context, workspaceID, messageID, artifactID string, maxChars int, cursor string, retrieval ArtifactRetrievalScope) (WorkspaceArtifactContent, error) {
	if scoped, ok := t.artifactReader.(scopedChatArtifactReader); ok {
		return scoped.ReadExtractedScoped(ctx, workspaceID, messageID, artifactID, maxChars, cursor, retrieval)
	}
	if retrieval.Mode == "required" {
		return WorkspaceArtifactContent{}, errors.New("required artifact scope is unavailable")
	}
	return t.artifactReader.ReadExtracted(ctx, workspaceID, artifactID, maxChars, cursor)
}

type canvasReadTarget struct {
	Kind       string                  `json:"kind"`
	FrameID    string                  `json:"frame_id,omitempty"`
	ElementIDs []string                `json:"element_ids,omitempty"`
	Bounds     *documents.CanvasBounds `json:"bounds,omitempty"`
}

func (target *canvasReadTarget) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	var header struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return err
	}
	allowed := map[string]struct{}{"kind": {}}
	switch header.Kind {
	case "overview":
	case "frame":
		allowed["frame_id"] = struct{}{}
	case "elements":
		allowed["element_ids"] = struct{}{}
	case "region":
		allowed["bounds"] = struct{}{}
	default:
		return errors.New("invalid canvas target kind")
	}
	for key := range fields {
		if _, ok := allowed[key]; !ok {
			return fmt.Errorf("unexpected canvas target field %q", key)
		}
	}
	type plainCanvasReadTarget canvasReadTarget
	var decoded plainCanvasReadTarget
	if err := decodeStrictToolInput(data, &decoded); err != nil {
		return err
	}
	*target = canvasReadTarget(decoded)
	return nil
}

type canvasReadCursor struct {
	Version           int    `json:"version"`
	ProjectionVersion int    `json:"projectionVersion"`
	Revision          string `json:"revision"`
	TargetHash        string `json:"targetHash"`
	Offset            int    `json:"offset"`
}

func (t *DocumentChatTools) readCanvas(ctx context.Context, userID, workspaceID string, raw json.RawMessage) (any, error) {
	var input struct {
		DocID  string           `json:"doc_id"`
		Target canvasReadTarget `json:"target"`
		Cursor string           `json:"cursor"`
		Limit  int              `json:"limit"`
	}
	if err := decodeStrictToolInput(raw, &input); err != nil || input.DocID == "" || len(input.DocID) > 128 || len(input.Cursor) > 2048 {
		return nil, errors.New("invalid canvas read input")
	}
	if input.Limit == 0 {
		input.Limit = 50
	}
	if input.Limit < 1 || input.Limit > maxCanvasReadLimit || !validCanvasReadTarget(input.Target) {
		return nil, errors.New("invalid canvas read target or limit")
	}
	projection, found, err := t.reader.ReadDocumentCanvas(ctx, userID, workspaceID, input.DocID)
	if err != nil {
		return nil, fmt.Errorf("read document canvas: %w", err)
	}
	if !found {
		return nil, workspaces.ErrDocumentNotFound
	}
	targetJSON, err := json.Marshal(input.Target)
	if err != nil {
		return nil, err
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(targetJSON))
	offset := 0
	if input.Cursor != "" {
		cursor, err := decodeCanvasCursor(input.Cursor)
		if err != nil || cursor.Version != 1 || cursor.ProjectionVersion != projection.Version || cursor.Revision != projection.Revision || cursor.TargetHash != hash {
			return nil, errors.New("canvas cursor is invalid or stale")
		}
		offset = cursor.Offset
	}
	blocks, elements := selectCanvasTarget(projection, input.Target)
	items := make([]canvasReadItem, 0, len(blocks)+len(elements))
	for i := range blocks {
		items = append(items, canvasReadItem{id: blocks[i].ID, block: &blocks[i]})
	}
	for i := range elements {
		items = append(items, canvasReadItem{id: elements[i].ID, element: &elements[i]})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].id < items[j].id })
	if offset > len(items) {
		return nil, errors.New("canvas cursor offset is invalid")
	}
	limit := min(input.Limit, len(items)-offset)
	for limit > 0 {
		result := buildCanvasReadResult(projection, workspaceID, input.Target, items[offset:offset+limit], offset+limit < len(items), hash, offset+limit)
		encoded, err := json.Marshal(result)
		if err != nil {
			return nil, errors.New("canvas result could not be encoded")
		}
		if len(encoded) <= maxChatToolOutputBytes || limit == 1 {
			if len(encoded) > maxChatToolOutputBytes {
				return nil, errors.New("canvas result exceeds the output limit")
			}
			return result, nil
		}
		limit--
	}
	return buildCanvasReadResult(projection, workspaceID, input.Target, nil, false, hash, offset), nil
}

func validCanvasReadTarget(target canvasReadTarget) bool {
	switch target.Kind {
	case "overview":
		return target.FrameID == "" && len(target.ElementIDs) == 0 && target.Bounds == nil
	case "frame":
		return target.FrameID != "" && len(target.FrameID) <= 128 && len(target.ElementIDs) == 0 && target.Bounds == nil
	case "elements":
		if target.FrameID != "" || target.Bounds != nil || len(target.ElementIDs) == 0 || len(target.ElementIDs) > maxCanvasReadLimit {
			return false
		}
		for _, id := range target.ElementIDs {
			if id == "" || len(id) > 128 {
				return false
			}
		}
		return true
	case "region":
		if target.FrameID != "" || len(target.ElementIDs) != 0 || target.Bounds == nil {
			return false
		}
		bounds := target.Bounds
		return finiteInRange(bounds.X, -10_000_000, 10_000_000) && finiteInRange(bounds.Y, -10_000_000, 10_000_000) && finiteInRange(bounds.Width, math.SmallestNonzeroFloat64, 10_000_000) && finiteInRange(bounds.Height, math.SmallestNonzeroFloat64, 10_000_000)
	default:
		return false
	}
}

func finiteInRange(value, minimum, maximum float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= minimum && value <= maximum
}

func decodeCanvasCursor(value string) (canvasReadCursor, error) {
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return canvasReadCursor{}, err
	}
	var cursor canvasReadCursor
	if err := decodeStrictToolInput(data, &cursor); err != nil || cursor.Offset < 0 || cursor.Revision == "" || cursor.TargetHash == "" {
		return canvasReadCursor{}, errors.New("invalid canvas cursor")
	}
	return cursor, nil
}

type canvasReadItem struct {
	id      string
	block   *documents.CanvasBlock
	element *documents.CanvasElement
}

func selectCanvasTarget(projection documents.CanvasProjection, target canvasReadTarget) ([]documents.CanvasBlock, []documents.CanvasElement) {
	blocks := make([]documents.CanvasBlock, 0, len(projection.Blocks))
	for _, block := range projection.Blocks {
		if block.Visibility != "page" {
			blocks = append(blocks, block)
		}
	}
	switch target.Kind {
	case "overview":
		owned := map[string]struct{}{}
		for _, block := range blocks {
			if block.Type == "frame" {
				for _, child := range block.ChildIDs {
					owned[child] = struct{}{}
				}
			}
		}
		selectedBlocks := make([]documents.CanvasBlock, 0, len(blocks))
		for _, block := range blocks {
			_, child := owned[block.ID]
			if block.Type == "frame" || !child {
				selectedBlocks = append(selectedBlocks, block)
			}
		}
		selectedElements := make([]documents.CanvasElement, 0, len(projection.Elements))
		for _, element := range projection.Elements {
			if element.FrameID == "" {
				selectedElements = append(selectedElements, element)
			}
		}
		return selectedBlocks, selectedElements
	case "frame":
		childIDs := map[string]struct{}{}
		for _, block := range blocks {
			if block.ID == target.FrameID && block.Type == "frame" {
				for _, id := range block.ChildIDs {
					childIDs[id] = struct{}{}
				}
			}
		}
		selectedBlocks := make([]documents.CanvasBlock, 0)
		for _, block := range blocks {
			_, child := childIDs[block.ID]
			if block.ID == target.FrameID || child {
				selectedBlocks = append(selectedBlocks, block)
			}
		}
		selectedElements := make([]documents.CanvasElement, 0)
		for _, element := range projection.Elements {
			_, child := childIDs[element.ID]
			if element.FrameID == target.FrameID || child {
				selectedElements = append(selectedElements, element)
			}
		}
		return selectedBlocks, selectedElements
	case "elements":
		selected := stringSet(target.ElementIDs)
		selectedBlocks := make([]documents.CanvasBlock, 0)
		for _, block := range blocks {
			if _, ok := selected[block.ID]; ok {
				selectedBlocks = append(selectedBlocks, block)
			}
		}
		selectedElements := make([]documents.CanvasElement, 0)
		for _, element := range projection.Elements {
			if _, ok := selected[element.ID]; ok {
				selectedElements = append(selectedElements, element)
			}
		}
		return selectedBlocks, selectedElements
	case "region":
		selectedBlocks := make([]documents.CanvasBlock, 0)
		for _, block := range blocks {
			if canvasIntersects(block.Bounds, target.Bounds) {
				selectedBlocks = append(selectedBlocks, block)
			}
		}
		selectedElements := make([]documents.CanvasElement, 0)
		for _, element := range projection.Elements {
			if canvasIntersects(element.Bounds, target.Bounds) {
				selectedElements = append(selectedElements, element)
			}
		}
		return selectedBlocks, selectedElements
	default:
		return nil, nil
	}
}

func canvasIntersects(left, right *documents.CanvasBounds) bool {
	return left != nil && left.X < right.X+right.Width && left.X+left.Width > right.X && left.Y < right.Y+right.Height && left.Y+left.Height > right.Y
}

func stringSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

func buildCanvasReadResult(projection documents.CanvasProjection, workspaceID string, target canvasReadTarget, items []canvasReadItem, truncated bool, targetHash string, nextOffset int) map[string]any {
	blocks := make([]map[string]any, 0)
	elements := make([]map[string]any, 0)
	for _, item := range items {
		if item.block != nil {
			blocks = append(blocks, boundedCanvasBlock(*item.block))
		} else if item.element != nil {
			elements = append(elements, boundedCanvasElement(*item.element))
		}
	}
	warnings := projection.Warnings
	warningsTruncated := len(warnings) > maxCanvasReadLimit
	if warningsTruncated {
		warnings = warnings[:maxCanvasReadLimit]
	}
	result := map[string]any{
		"doc_id": projection.DocID, "revision": projection.Revision, "target": target,
		"counts": projection.Counts, "blocks": blocks, "elements": elements,
		"truncated": truncated, "warnings": warnings, "warnings_truncated": warningsTruncated,
		"source": map[string]any{"type": "document", "workspace_id": workspaceID, "doc_id": projection.DocID, "title": projection.Title, "revision": projection.Revision, "visibility": "edgeless"},
	}
	if projection.Bounds != nil {
		result["bounds"] = projection.Bounds
	}
	if truncated {
		cursor, _ := json.Marshal(canvasReadCursor{Version: 1, ProjectionVersion: projection.Version, Revision: projection.Revision, TargetHash: targetHash, Offset: nextOffset})
		result["next_cursor"] = base64.RawURLEncoding.EncodeToString(cursor)
	}
	return result
}

func boundedCanvasBlock(value documents.CanvasBlock) map[string]any {
	result := map[string]any{"id": value.ID, "type": value.Type, "visibility": value.Visibility, "child_ids": boundedRelationIDs(value.ChildIDs), "child_ids_truncated": len(value.ChildIDs) > maxCanvasRelationIDs}
	if value.Bounds != nil {
		result["bounds"] = value.Bounds
	}
	if value.Text != "" {
		result["text"] = boundedCanvasString(value.Text, result)
	}
	if value.Title != "" {
		result["title"] = boundedCanvasString(value.Title, result)
	}
	return result
}

func boundedCanvasElement(value documents.CanvasElement) map[string]any {
	result := map[string]any{"id": value.ID, "type": value.Type, "child_ids": boundedRelationIDs(value.ChildIDs), "child_ids_truncated": len(value.ChildIDs) > maxCanvasRelationIDs}
	if value.Bounds != nil {
		result["bounds"] = value.Bounds
	}
	if value.Text != "" {
		result["text"] = boundedCanvasString(value.Text, result)
	}
	if value.Title != "" {
		result["title"] = boundedCanvasString(value.Title, result)
	}
	for key, item := range map[string]string{"frame_id": value.FrameID, "source_id": value.SourceID, "target_id": value.TargetID, "parent_id": value.ParentID, "index": value.Index, "color": value.Color} {
		if item != "" {
			result[key] = item
		}
	}
	if value.PointCount != nil {
		result["point_count"] = *value.PointCount
	}
	if value.LineWidth != nil {
		result["line_width"] = *value.LineWidth
	}
	return result
}

func boundedCanvasString(value string, result map[string]any) string {
	runes := []rune(value)
	if len(runes) <= maxCanvasPreviewChars {
		return value
	}
	result["content_truncated"] = true
	return string(runes[:maxCanvasPreviewChars])
}

func boundedRelationIDs(values []string) []string {
	if len(values) > maxCanvasRelationIDs {
		return values[:maxCanvasRelationIDs]
	}
	return values
}

func (t *DocumentChatTools) search(ctx context.Context, userID, workspaceID string, raw json.RawMessage) (any, error) {
	var input struct {
		Query  string   `json:"query"`
		DocIDs []string `json:"doc_ids"`
		Limit  int      `json:"limit"`
	}
	if err := decodeStrictToolInput(raw, &input); err != nil {
		return nil, errors.New("invalid document search input")
	}
	input.Query = strings.TrimSpace(input.Query)
	if input.Query == "" || len([]rune(input.Query)) > maxChatDocSearchQuery || len(input.DocIDs) > 50 {
		return nil, errors.New("invalid document search input")
	}
	if t.searcher == nil {
		return nil, errors.New("document search is unavailable")
	}
	if input.Limit == 0 {
		input.Limit = maxChatDocSearchHits
	}
	if input.Limit < 1 || input.Limit > maxChatDocSearchHits {
		return nil, errors.New("document search limit is out of range")
	}
	requested := make(map[string]struct{}, len(input.DocIDs))
	for _, id := range input.DocIDs {
		if id == "" || len(id) > 128 {
			return nil, errors.New("invalid document search scope")
		}
		requested[id] = struct{}{}
	}
	if len(input.DocIDs) > 0 && len(requested) == 0 {
		return map[string]any{"hits": []any{}}, nil
	}

	const pageSize = 100
	results := make([]map[string]any, 0, input.Limit)
	for offset := 0; offset < maxChatDocSearchScan && len(results) < input.Limit; offset += pageSize {
		candidates, err := t.searcher.SearchDocsPage(ctx, workspaceID, input.Query, pageSize, offset)
		if err != nil {
			return nil, fmt.Errorf("search workspace documents: %w", err)
		}
		ids := make([]string, 0, len(candidates))
		for _, candidate := range candidates {
			if len(requested) == 0 {
				ids = append(ids, candidate.DocID)
			} else if _, ok := requested[candidate.DocID]; ok {
				ids = append(ids, candidate.DocID)
			}
		}
		permissions, err := t.reader.ReadDocumentPermissionsBatch(ctx, userID, workspaceID, ids)
		if err != nil && !errors.Is(err, workspaces.ErrDocRoleDenied) && !errors.Is(err, workspaces.ErrDocumentNotFound) {
			return nil, fmt.Errorf("authorize workspace search results: %w", err)
		}
		for _, candidate := range candidates {
			if len(requested) > 0 {
				if _, ok := requested[candidate.DocID]; !ok {
					continue
				}
			}
			if !permissions[candidate.DocID]["Doc.Read"] {
				continue
			}
			results = append(results, map[string]any{
				"doc_id": candidate.DocID, "title": candidate.Title,
				"block_id": candidate.BlockID, "excerpt": candidate.Highlight,
				"updated_at": candidate.UpdatedAt,
			})
			if len(results) == input.Limit {
				break
			}
		}
		if len(candidates) < pageSize {
			break
		}
	}
	if t.semanticSearcher != nil {
		candidates, err := t.semanticSearcher.SearchDocs(ctx, workspaceID, input.Query, input.DocIDs, maxChatDocSearchScan)
		if err != nil && !errors.Is(err, search.ErrEmbeddingUnavailable) {
			return nil, fmt.Errorf("search semantic workspace documents: %w", err)
		}
		if err == nil {
			ids := make([]string, 0, len(candidates))
			for _, candidate := range candidates {
				ids = append(ids, candidate.DocID)
			}
			permissions, permissionErr := t.reader.ReadDocumentPermissionsBatch(ctx, userID, workspaceID, ids)
			if permissionErr != nil && !errors.Is(permissionErr, workspaces.ErrDocRoleDenied) && !errors.Is(permissionErr, workspaces.ErrDocumentNotFound) {
				return nil, fmt.Errorf("authorize semantic workspace search results: %w", permissionErr)
			}
			semanticResults := make([]map[string]any, 0, input.Limit)
			seen := make(map[string]struct{}, len(results)+len(candidates))
			for _, candidate := range candidates {
				if _, found := seen[candidate.DocID]; found || !permissions[candidate.DocID]["Doc.Read"] {
					continue
				}
				if len(requested) > 0 {
					if _, allowed := requested[candidate.DocID]; !allowed {
						continue
					}
				}
				semanticResults = append(semanticResults, map[string]any{
					"doc_id": candidate.DocID, "title": candidate.Title,
					"block_id": candidate.BlockID, "excerpt": candidate.Highlight,
					"updated_at": candidate.UpdatedAt,
				})
				seen[candidate.DocID] = struct{}{}
				if len(semanticResults) == input.Limit {
					break
				}
			}
			combined := semanticResults
			for _, lexicalResult := range results {
				if len(combined) == input.Limit {
					break
				}
				docID, _ := lexicalResult["doc_id"].(string)
				if _, found := seen[docID]; found {
					continue
				}
				combined = append(combined, lexicalResult)
				seen[docID] = struct{}{}
				if len(combined) == input.Limit {
					break
				}
			}
			results = combined
		}
	}
	return map[string]any{"hits": results}, nil
}

func (t *DocumentChatTools) read(ctx context.Context, userID, workspaceID string, raw json.RawMessage) (any, error) {
	var input struct {
		DocID    string `json:"doc_id"`
		MaxChars int    `json:"max_chars"`
	}
	if err := decodeStrictToolInput(raw, &input); err != nil || input.DocID == "" || len(input.DocID) > 128 {
		return nil, errors.New("invalid document read input")
	}
	if input.MaxChars == 0 {
		input.MaxChars = maxChatDocReadChars
	}
	if input.MaxChars < 1 || input.MaxChars > maxChatDocReadChars {
		return nil, errors.New("document read limit is out of range")
	}
	permissions, err := t.reader.ReadDocumentPermissions(ctx, userID, workspaceID, input.DocID)
	if err != nil {
		return nil, fmt.Errorf("authorize document read: %w", err)
	}
	if !permissions["Doc.Read"] {
		return nil, workspaces.ErrDocRoleDenied
	}
	markdown, found, err := t.reader.ReadDocumentMarkdown(ctx, workspaceID, input.DocID)
	if err != nil {
		return nil, fmt.Errorf("read document markdown: %w", err)
	}
	if !found {
		return nil, workspaces.ErrDocumentNotFound
	}
	runes := []rune(markdown)
	truncated := len(runes) > input.MaxChars
	if truncated {
		markdown = string(runes[:input.MaxChars])
	}
	return map[string]any{"doc_id": input.DocID, "markdown": markdown, "truncated": truncated}, nil
}

func decodeStrictToolInput(raw json.RawMessage, target any) error {
	if !utf8.Valid(raw) || len(raw) == 0 || len(raw) > 16<<10 {
		return errors.New("invalid tool input size")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("unexpected trailing tool input")
	}
	return nil
}
