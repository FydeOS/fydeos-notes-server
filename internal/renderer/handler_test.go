// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package renderer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type previewReaderStub struct {
	doc           PreviewResult
	workspace     PreviewResult
	markdown      string
	markdownOK    bool
	markdownErr   error
	markdownCalls int
	docCalls      int
	spaceCalls    int
}

func (s *previewReaderStub) ReadDocumentMarkdown(_ context.Context, _, _ string) (string, bool, error) {
	s.markdownCalls++
	return s.markdown, s.markdownOK, s.markdownErr
}

func (s *previewReaderStub) ReadDocumentPreview(context.Context, string, string) (PreviewResult, error) {
	s.docCalls++
	return s.doc, nil
}

func (s *previewReaderStub) ReadWorkspacePreview(context.Context, string) (PreviewResult, error) {
	s.spaceCalls++
	return s.workspace, nil
}

type viewRecord struct {
	workspace string
	doc       string
	visitor   string
	guest     bool
	user      string
	viewedAt  time.Time
}

type viewRecorderStub struct{ calls chan viewRecord }

func (s viewRecorderStub) RecordDocView(_ context.Context, workspace, doc, visitor string, guest bool, user string, viewedAt time.Time) error {
	s.calls <- viewRecord{workspace: workspace, doc: doc, visitor: visitor, guest: guest, user: user, viewedAt: viewedAt}
	return nil
}

func TestPublicDocumentPreviewRendersIndexableMetadataAndRecordsView(t *testing.T) {
	recorder := viewRecorderStub{calls: make(chan viewRecord, 1)}
	handler := NewHandler(&previewReaderStub{doc: PreviewResult{
		Exposure: ExposurePublicIndexable,
		Present:  true,
		Title:    "A <safe> title",
		Summary:  "This & that",
	}}, recorder, Config{})
	request := httptest.NewRequest(http.MethodGet, "/workspace/space/doc", nil)
	request.RemoteAddr = "192.0.2.9:4567"
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if !strings.HasPrefix(response.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("content type = %q", response.Header().Get("Content-Type"))
	}
	if got := response.Header().Get("X-Robots-Tag"); got != "" {
		t.Fatalf("X-Robots-Tag = %q, want absent for indexable page", got)
	}
	for _, want := range []string{
		"<title>A &lt;safe&gt; title | FydeOS Notes</title>",
		`<meta property="og:description" content="This &amp; that" />`,
	} {
		if !strings.Contains(response.Body.String(), want) {
			t.Fatalf("rendered HTML missing %q", want)
		}
	}
	select {
	case call := <-recorder.calls:
		if call.workspace != "space" || call.doc != "doc" || !call.guest || call.user != "" || call.visitor == "" {
			t.Fatalf("view record = %+v", call)
		}
	case <-time.After(time.Second):
		t.Fatal("public page view was not recorded")
	}
}

func TestPrivateDocumentPreviewUsesNoIndexAndRecordsGuestView(t *testing.T) {
	recorder := viewRecorderStub{calls: make(chan viewRecord, 1)}
	handler := NewHandler(&previewReaderStub{doc: PreviewResult{
		Exposure: ExposureMetadataNoIndex,
		Present:  true,
		Title:    "Private preview",
	}}, recorder, Config{})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/workspace/space/doc", nil))
	if response.Code != http.StatusOK || response.Header().Get("X-Robots-Tag") != "noindex" || !strings.Contains(response.Body.String(), "Private preview | FydeOS Notes") {
		t.Fatalf("private preview response = %d, %q, %s", response.Code, response.Header().Get("X-Robots-Tag"), response.Body.String())
	}
	select {
	case call := <-recorder.calls:
		if !call.guest || call.doc != "doc" {
			t.Fatalf("view record = %+v", call)
		}
	case <-time.After(time.Second):
		t.Fatal("private preview view was not recorded")
	}
}

func TestWorkspaceRootAndStaticPagesDoNotRecordDocumentViews(t *testing.T) {
	recorder := viewRecorderStub{calls: make(chan viewRecord, 1)}
	reader := &previewReaderStub{
		workspace: PreviewResult{Exposure: ExposureMetadataNoIndex, Present: true, Title: "Workspace"},
	}
	handler := NewHandler(reader, recorder, Config{})
	rootResponse := httptest.NewRecorder()
	handler.ServeHTTP(rootResponse, httptest.NewRequest(http.MethodGet, "/workspace/space/space", nil))
	if rootResponse.Code != http.StatusOK || rootResponse.Header().Get("X-Robots-Tag") != "noindex" || reader.spaceCalls != 1 {
		t.Fatalf("workspace root response = %d/%q, workspace calls=%d", rootResponse.Code, rootResponse.Header().Get("X-Robots-Tag"), reader.spaceCalls)
	}
	staticResponse := httptest.NewRecorder()
	handler.ServeHTTP(staticResponse, httptest.NewRequest(http.MethodGet, "/workspace/space/search", nil))
	if staticResponse.Code != http.StatusOK || reader.docCalls != 0 || reader.spaceCalls != 1 {
		t.Fatalf("static route response = %d, reader calls=%d/%d", staticResponse.Code, reader.docCalls, reader.spaceCalls)
	}
	select {
	case call := <-recorder.calls:
		t.Fatalf("unexpected document view record: %+v", call)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestMarkdownAcceptReturnsMarkdownWithoutPreviewOrAnalytics(t *testing.T) {
	reader := &previewReaderStub{markdown: "# markdown-doc", markdownOK: true}
	recorder := viewRecorderStub{calls: make(chan viewRecord, 1)}
	handler := NewHandler(reader, recorder, Config{})
	request := httptest.NewRequest(http.MethodGet, "/workspace/space/doc", nil)
	request.Header.Set("Accept", "text/markdown; charset=utf-8")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || response.Body.String() != "# markdown-doc" {
		t.Fatalf("markdown response = %d, %q", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); got != "text/markdown; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
	if reader.markdownCalls != 1 || reader.docCalls != 0 || reader.spaceCalls != 0 {
		t.Fatalf("reader calls = markdown:%d document:%d workspace:%d", reader.markdownCalls, reader.docCalls, reader.spaceCalls)
	}
	select {
	case call := <-recorder.calls:
		t.Fatalf("markdown request recorded a document view: %+v", call)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestMarkdownAcceptAcrossHeaderLinesReturnsMarkdown(t *testing.T) {
	reader := &previewReaderStub{markdown: "content", markdownOK: true}
	handler := NewHandler(reader, nil, Config{})
	request := httptest.NewRequest(http.MethodGet, "/workspace/space/doc", nil)
	request.Header.Add("Accept", "text/html")
	request.Header.Add("Accept", "text/markdown;q=0.5")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "content" || reader.markdownCalls != 1 {
		t.Fatalf("multiple Accept headers response = %d, %q, markdown calls=%d", response.Code, response.Body.String(), reader.markdownCalls)
	}
}

func TestMarkdownAcceptReturnsNotFoundWhenUnavailable(t *testing.T) {
	for name, reader := range map[string]*previewReaderStub{
		"denied":  {markdownOK: false},
		"invalid": {markdownErr: context.DeadlineExceeded},
	} {
		t.Run(name, func(t *testing.T) {
			handler := NewHandler(reader, nil, Config{})
			request := httptest.NewRequest(http.MethodGet, "/workspace/space/doc", nil)
			request.Header.Set("Accept", "application/markdown")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusNotFound || reader.markdownCalls != 1 || reader.docCalls != 0 {
				t.Fatalf("markdown denial = status %d, calls markdown:%d preview:%d", response.Code, reader.markdownCalls, reader.docCalls)
			}
		})
	}
}

func TestMarkdownAcceptWithZeroQualityFallsBackToHTML(t *testing.T) {
	reader := &previewReaderStub{doc: PreviewResult{Exposure: ExposurePublicIndexable, Present: true, Title: "Public"}}
	handler := NewHandler(reader, nil, Config{})
	request := httptest.NewRequest(http.MethodGet, "/workspace/space/doc", nil)
	request.Header.Set("Accept", "text/markdown;q=0, text/html;q=1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.HasPrefix(response.Header().Get("Content-Type"), "text/html") || reader.markdownCalls != 0 || reader.docCalls != 1 {
		t.Fatalf("zero-quality markdown response = status %d, type %q, calls markdown:%d preview:%d", response.Code, response.Header().Get("Content-Type"), reader.markdownCalls, reader.docCalls)
	}
}
