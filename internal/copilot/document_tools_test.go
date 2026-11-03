// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/fydeos/fydeos-notes-server/internal/documents"
	"github.com/fydeos/fydeos-notes-server/internal/search"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

type chatToolSearchStub struct {
	results []search.DocResult
}

func (s chatToolSearchStub) SearchDocsPage(context.Context, string, string, int, int) ([]search.DocResult, error) {
	return s.results, nil
}

type chatToolSemanticStub struct {
	results []search.DocResult
	err     error
}

type chatArtifactSearchStub struct {
	results []search.ArtifactCandidate
	calls   int
}

func (s *chatArtifactSearchStub) SearchArtifacts(context.Context, string, string, int) ([]search.ArtifactCandidate, error) {
	s.calls++
	return s.results, nil
}

type chatArtifactReaderStub struct {
	content    WorkspaceArtifactContent
	artifactID string
	maxChars   int
	cursor     string
	messageID  string
	scope      ArtifactRetrievalScope
}

func (s *chatArtifactReaderStub) ReadExtracted(_ context.Context, _, artifactID string, maxChars int, cursor string) (WorkspaceArtifactContent, error) {
	s.artifactID, s.maxChars, s.cursor = artifactID, maxChars, cursor
	return s.content, nil
}

func (s *chatArtifactReaderStub) ReadExtractedScoped(_ context.Context, _, messageID, artifactID string, maxChars int, cursor string, scope ArtifactRetrievalScope) (WorkspaceArtifactContent, error) {
	s.artifactID, s.maxChars, s.cursor, s.messageID, s.scope = artifactID, maxChars, cursor, messageID, scope
	return s.content, nil
}

type chatWorkspaceReaderStub struct {
	err error
}

func (s chatWorkspaceReaderStub) ReadWorkspace(context.Context, string, string) (workspaces.Workspace, error) {
	return workspaces.Workspace{}, s.err
}

func (s chatToolSemanticStub) SearchDocs(context.Context, string, string, []string, int) ([]search.DocResult, error) {
	return s.results, s.err
}

type chatToolReaderStub struct {
	permissions      map[string]map[string]bool
	markdown         string
	markdownFound    bool
	markdownReadCall int
	canvas           documents.CanvasProjection
	canvasFound      bool
}

func (s *chatToolReaderStub) ReadDocumentPermissions(_ context.Context, _, _, docID string) (map[string]bool, error) {
	permission, ok := s.permissions[docID]
	if !ok {
		return nil, workspaces.ErrDocumentNotFound
	}
	return permission, nil
}

func (s *chatToolReaderStub) ReadDocumentPermissionsBatch(_ context.Context, _, _ string, docIDs []string) (map[string]map[string]bool, error) {
	result := make(map[string]map[string]bool, len(docIDs))
	for _, docID := range docIDs {
		if permission, ok := s.permissions[docID]; ok {
			result[docID] = permission
		}
	}
	return result, nil
}

func (s *chatToolReaderStub) ReadDocumentMarkdown(context.Context, string, string) (string, bool, error) {
	s.markdownReadCall++
	return s.markdown, s.markdownFound, nil
}

func (s *chatToolReaderStub) ReadDocumentCanvas(context.Context, string, string, string) (documents.CanvasProjection, bool, error) {
	return s.canvas, s.canvasFound, nil
}

func TestDocumentChatToolsSearchFiltersUnreadableDocuments(t *testing.T) {
	reader := &chatToolReaderStub{permissions: map[string]map[string]bool{
		"readable": {"Doc.Read": true},
		"private":  {"Doc.Read": false},
	}}
	tools := NewDocumentChatTools(chatToolSearchStub{results: []search.DocResult{
		{DocID: "private", Title: "Private"},
		{DocID: "readable", Title: "Readable"},
	}}, reader)

	result, err := tools.Execute(context.Background(), "user-1", "workspace-1", "doc_search", json.RawMessage(`{"query":"needle"}`))
	if err != nil {
		t.Fatalf("search documents: %v", err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "private") || !strings.Contains(string(encoded), "readable") {
		t.Fatalf("search result includes wrong documents: %s", encoded)
	}
}

func TestDocumentChatToolsAddsSemanticCandidatesWithCanonicalACL(t *testing.T) {
	reader := &chatToolReaderStub{permissions: map[string]map[string]bool{
		"lexical":           {"Doc.Read": true},
		"semantic-readable": {"Doc.Read": true},
		"semantic-private":  {"Doc.Read": false},
	}}
	tools := NewDocumentChatTools(chatToolSearchStub{results: []search.DocResult{{DocID: "lexical", Title: "Lexical"}}}, reader)
	tools.SetSemanticSearcher(chatToolSemanticStub{results: []search.DocResult{
		{DocID: "lexical", Title: "Duplicate"},
		{DocID: "semantic-private", Title: "Private semantic result"},
		{DocID: "semantic-readable", Title: "Semantic"},
	}})

	result, err := tools.Execute(context.Background(), "user-1", "workspace-1", "doc_search", json.RawMessage(`{"query":"related idea"}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "semantic-private") || strings.Contains(string(encoded), "Private semantic result") || strings.Count(string(encoded), `"doc_id":"lexical"`) != 1 || !strings.Contains(string(encoded), "semantic-readable") {
		t.Fatalf("semantic search result violated ACL/deduplication: %s", encoded)
	}
}

func TestDocumentChatToolsFallsBackWhenEmbeddingIsUnavailable(t *testing.T) {
	tools := NewDocumentChatTools(chatToolSearchStub{results: []search.DocResult{{DocID: "lexical", Title: "Lexical"}}}, &chatToolReaderStub{
		permissions: map[string]map[string]bool{"lexical": {"Doc.Read": true}},
	})
	tools.SetSemanticSearcher(chatToolSemanticStub{err: search.ErrEmbeddingUnavailable})
	if _, err := tools.Execute(context.Background(), "user-1", "workspace-1", "doc_search", json.RawMessage(`{"query":"needle"}`)); err != nil {
		t.Fatalf("lexical fallback returned error: %v", err)
	}
}

func TestDocumentChatToolsReadRequiresPermissionAndBoundsContent(t *testing.T) {
	reader := &chatToolReaderStub{
		permissions: map[string]map[string]bool{
			"private": {"Doc.Read": false},
			"allowed": {"Doc.Read": true},
		},
		markdown:      "0123456789",
		markdownFound: true,
	}
	tools := NewDocumentChatTools(nil, reader)
	if _, err := tools.Execute(context.Background(), "user-1", "workspace-1", "doc_read", json.RawMessage(`{"doc_id":"private"}`)); !errors.Is(err, workspaces.ErrDocRoleDenied) {
		t.Fatalf("read private document error = %v, want permission denial", err)
	}
	if reader.markdownReadCall != 0 {
		t.Fatal("private document markdown was read before authorization")
	}

	result, err := tools.Execute(context.Background(), "user-1", "workspace-1", "doc_read", json.RawMessage(`{"doc_id":"allowed","max_chars":4}`))
	if err != nil {
		t.Fatalf("read allowed document: %v", err)
	}
	value, ok := result.(map[string]any)
	if !ok || value["markdown"] != "0123" || value["truncated"] != true {
		t.Fatalf("bounded read result = %#v", result)
	}
}

func TestDocumentChatToolsRejectMalformedAndOversizedInput(t *testing.T) {
	tools := NewDocumentChatTools(chatToolSearchStub{}, &chatToolReaderStub{})
	for _, input := range []json.RawMessage{
		json.RawMessage(`{"query":"needle","unexpected":true}`),
		json.RawMessage(`{"query":"needle"} {}`),
		json.RawMessage(`{"query":"` + strings.Repeat("x", maxChatDocSearchQuery+1) + `"}`),
		json.RawMessage(`{"query":"` + strings.Repeat("x", 16<<10) + `"}`),
	} {
		if _, err := tools.Execute(context.Background(), "user-1", "workspace-1", "doc_search", input); err == nil {
			t.Fatalf("expected invalid input to be rejected: %s", input[:min(len(input), 80)])
		}
	}
}

func TestDocumentChatToolsArtifactSearchRequiresWorkspaceRead(t *testing.T) {
	searcher := &chatArtifactSearchStub{results: []search.ArtifactCandidate{{ArtifactID: "3e5e66cb-8022-4d95-982a-1670c87aef01", Content: "excerpt"}}}
	reader := &chatArtifactReaderStub{}
	tools := NewDocumentChatTools(chatToolSearchStub{}, &chatToolReaderStub{})
	tools.SetArtifactTools(searcher, reader, chatWorkspaceReaderStub{err: workspaces.ErrAccessDenied})
	if _, err := tools.Execute(context.Background(), "user-1", "workspace-1", "artifact_search", json.RawMessage(`{"query":"needle"}`)); !errors.Is(err, workspaces.ErrAccessDenied) {
		t.Fatalf("artifact search error = %v, want workspace read denial", err)
	}
	if searcher.calls != 0 {
		t.Fatal("artifact search ran before workspace read authorization")
	}
}

func TestDocumentChatToolsArtifactReadCarriesBoundedCursorAndSource(t *testing.T) {
	artifactID := "3e5e66cb-8022-4d95-982a-1670c87aef01"
	reader := &chatArtifactReaderStub{content: WorkspaceArtifactContent{Content: "continued", Revision: "rev-1", Name: "notes.txt", MediaType: "text/plain", Truncated: true, NextCursor: "9"}}
	tools := NewDocumentChatTools(chatToolSearchStub{}, &chatToolReaderStub{})
	tools.SetArtifactTools(&chatArtifactSearchStub{}, reader, chatWorkspaceReaderStub{})
	result, err := tools.Execute(context.Background(), "user-1", "workspace-1", "artifact_read", json.RawMessage(`{"artifact_id":"`+artifactID+`","max_chars":12,"cursor":"4"}`))
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if reader.artifactID != artifactID || reader.maxChars != 12 || reader.cursor != "4" || !strings.Contains(string(encoded), `"next_cursor":"9"`) || !strings.Contains(string(encoded), `"mime_type":"text/plain"`) {
		t.Fatalf("artifact read result or forwarded bounds are wrong: reader=%+v result=%s", reader, encoded)
	}
}

func TestDocumentChatToolsArtifactSearchUsesRequiredMessageScope(t *testing.T) {
	reader := &chatArtifactReaderStub{content: WorkspaceArtifactContent{Name: "attached.txt", MediaType: "text/plain", Content: "attached content"}}
	tools := NewDocumentChatTools(nil, &chatToolReaderStub{})
	tools.SetArtifactTools(nil, reader, chatWorkspaceReaderStub{})
	scope := ArtifactRetrievalScope{Mode: "required", RequiredArtifactIDs: []string{"6ba7b810-9dad-11d1-80b4-00c04fd430c8"}}
	result, err := tools.ExecuteTurn(context.Background(), "user-1", "workspace-1", "message-1", scope, "artifact_search", json.RawMessage(`{"query":"question"}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(result)
	if reader.messageID != "message-1" || reader.scope.Mode != "required" || len(reader.scope.RequiredArtifactIDs) != 1 || !strings.Contains(string(encoded), "attached content") {
		t.Fatalf("required artifact result=%s scope=%#v message=%q", encoded, reader.scope, reader.messageID)
	}
}

func TestDocumentChatToolsCanvasOverviewPaginationAndRevisionFence(t *testing.T) {
	reader := &chatToolReaderStub{canvasFound: true, canvas: documents.CanvasProjection{
		Version: 1, DocID: "doc-1", Revision: "rev-1", Title: "Board",
		Blocks: []documents.CanvasBlock{
			{ID: "a-child", Type: "edgeless-text", Visibility: "edgeless", Text: "inside frame"},
			{ID: "m-frame", Type: "frame", Visibility: "edgeless", ChildIDs: []string{"a-child"}},
			{ID: "z-text", Type: "edgeless-text", Visibility: "edgeless", Text: "top-level"},
			{ID: "page", Type: "paragraph", Visibility: "page", Text: "page only"},
		},
		Elements: []documents.CanvasElement{
			{ID: "x-child-shape", Type: "shape", FrameID: "m-frame"},
			{ID: "y-shape", Type: "shape"},
		},
		Counts: map[string]int{"shape": 2},
	}}
	tools := NewDocumentChatTools(nil, reader)
	input := json.RawMessage(`{"doc_id":"doc-1","target":{"kind":"overview"},"limit":1}`)
	first, err := tools.Execute(context.Background(), "user-1", "workspace-1", "doc_canvas_read", input)
	if err != nil {
		t.Fatalf("read canvas overview: %v", err)
	}
	firstResult := first.(map[string]any)
	if firstResult["truncated"] != true || firstResult["next_cursor"] == nil || !strings.Contains(string(mustJSON(t, firstResult["source"])), `"workspace_id":"workspace-1"`) {
		t.Fatalf("first canvas page = %#v", firstResult)
	}
	blocks := firstResult["blocks"].([]map[string]any)
	if len(blocks) != 1 || blocks[0]["id"] != "m-frame" {
		t.Fatalf("first canvas page blocks = %#v", blocks)
	}
	cursor, err := json.Marshal(firstResult["next_cursor"])
	if err != nil {
		t.Fatal(err)
	}
	var cursorValue string
	if err := json.Unmarshal(cursor, &cursorValue); err != nil {
		t.Fatal(err)
	}
	secondInput, _ := json.Marshal(map[string]any{"doc_id": "doc-1", "target": map[string]any{"kind": "overview"}, "cursor": cursorValue, "limit": 1})
	second, err := tools.Execute(context.Background(), "user-1", "workspace-1", "doc_canvas_read", secondInput)
	if err != nil {
		t.Fatalf("read next canvas page: %v", err)
	}
	secondResult := second.(map[string]any)
	if secondResult["truncated"] != true || len(secondResult["elements"].([]map[string]any)) != 1 || secondResult["elements"].([]map[string]any)[0]["id"] != "y-shape" {
		t.Fatalf("second canvas page = %#v", secondResult)
	}
	reader.canvas.Revision = "rev-2"
	if _, err := tools.Execute(context.Background(), "user-1", "workspace-1", "doc_canvas_read", secondInput); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale revision cursor error = %v", err)
	}
	if _, err := tools.Execute(context.Background(), "user-1", "workspace-1", "doc_canvas_read", json.RawMessage(`{"doc_id":"doc-1","target":{"kind":"region","bounds":{"x":0,"y":0,"width":0,"height":1}}}`)); err == nil {
		t.Fatal("zero-width canvas region was accepted")
	}
	for _, input := range []json.RawMessage{
		json.RawMessage(`{"doc_id":"doc-1","target":{"kind":"overview","frame_id":""}}`),
		json.RawMessage(`{"doc_id":"doc-1","target":{"kind":"region","bounds":{"x":0,"y":0,"width":1,"height":1,"extra":true}}}`),
	} {
		if _, err := tools.Execute(context.Background(), "user-1", "workspace-1", "doc_canvas_read", input); err == nil {
			t.Fatalf("unexpected canvas target fields accepted: %s", input)
		}
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
