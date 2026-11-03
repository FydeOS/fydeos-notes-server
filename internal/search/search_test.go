// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package search

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/fydeos/fydeos-notes-server/internal/byok"
	"github.com/jackc/pgx/v5"
)

type failingQuerier struct{}

func (failingQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("unexpected query")
}

func u32(v uint32) *uint32   { return &v }
func f64(v float64) *float64 { return &v }
func sp(v string) *string    { return &v }

func TestKeywordInputValidation(t *testing.T) {
	s := NewDocsService(failingQuerier{})
	got, err := s.SearchDocsPage(context.Background(), "ws", "   ", 10, 0)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("blank keyword = %v %v", got, err)
	}
	for _, tc := range []struct {
		ws, kw        string
		limit, offset int
	}{
		{"ws", "a", 0, 0}, {"ws", "a", 10001, 0}, {"ws", "a", 1, -1},
		{"ws", strings.Repeat("a", 5000), 1, 0}, {"", "a", 1, 0},
	} {
		if _, err := s.SearchDocsPage(context.Background(), tc.ws, tc.kw, tc.limit, tc.offset); !errors.Is(err, ErrInvalidSearchDocsInput) {
			t.Fatalf("%+v: %v", tc, err)
		}
	}
	var nilService *DocsService
	if _, err := nilService.SearchDocs(context.Background(), "ws", "a", 1); !errors.Is(err, ErrInvalidSearchDocsInput) {
		t.Fatal(err)
	}
}

func match(field, value string) *Query { return &Query{Type: "match", Field: field, Match: value} }

func TestStructuredValidation(t *testing.T) {
	s := NewProjectionService(failingQuerier{})
	deep := match("content", "x")
	for range 101 {
		deep = &Query{Type: "boost", Query: deep, Boost: f64(1)}
	}
	wide := &Query{Type: "boolean", Occur: "should"}
	for range 1001 {
		wide.Queries = append(wide.Queries, &Query{Type: "all"})
	}
	cases := map[string]SearchRequest{
		"table":       {Table: "x", Query: &Query{Type: "all"}},
		"nil query":   {Table: BlockTable},
		"type":        {Table: BlockTable, Query: &Query{Type: "nope"}},
		"empty match": {Table: BlockTable, Query: match("content", "")},
		"occur":       {Table: BlockTable, Query: &Query{Type: "boolean", Occur: "x", Queries: []*Query{{Type: "all"}}}},
		"no children": {Table: BlockTable, Query: &Query{Type: "boolean", Occur: "must"}},
		"boost inner": {Table: BlockTable, Query: &Query{Type: "boost", Boost: f64(2)}},
		"boost 0":     {Table: BlockTable, Query: &Query{Type: "all", Boost: f64(0)}},
		"boost -1":    {Table: BlockTable, Query: &Query{Type: "all", Boost: f64(-1)}},
		"boost NaN":   {Table: BlockTable, Query: &Query{Type: "all", Boost: f64(math.NaN())}},
		"deep":        {Table: BlockTable, Query: deep},
		"wide":        {Table: BlockTable, Query: wide},
		"visibility":  {Table: BlockTable, Query: &Query{Type: "all"}, Options: SearchOptions{Fields: []string{"visibility"}}},
		"sourceHash":  {Table: BlockTable, Query: match("sourceHash", "x")},
		"doc content": {Table: DocTable, Query: match("content", "x")},
		"highlight":   {Table: DocTable, Query: &Query{Type: "all"}, Options: SearchOptions{Highlights: []Highlight{{Field: "content", Before: "<b>", End: "</b>"}}}},
		"limit":       {Table: BlockTable, Query: &Query{Type: "all"}, Options: SearchOptions{Pagination: Pagination{Limit: u32(10001)}}},
		"skip":        {Table: BlockTable, Query: &Query{Type: "all"}, Options: SearchOptions{Pagination: Pagination{Limit: u32(10), Skip: u32(9995)}}},
	}
	for name, request := range cases {
		if _, err := s.Search(context.Background(), "ws", "actor", request); !errors.Is(err, ErrInvalidQuery) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := s.Search(context.Background(), "ws", "", SearchRequest{Table: BlockTable, Query: &Query{Type: "all"}}); !errors.Is(err, ErrInvalidQuery) {
		t.Fatal("missing actor accepted")
	}
}

func TestEvaluate(t *testing.T) {
	values := map[string]any{"content": "foo bar foo", "flavour": "affine:paragraph", "ref": []any{"d1", "d2"}, "refDocId": []any{"d1", "d2"}, "blob": []any{}}
	if score, ok := evaluate(BlockTable, match("content", "Foo"), values); !ok || score != 2 {
		t.Fatalf("content score %v %v", score, ok)
	}
	if _, ok := evaluate(BlockTable, match("refDocId", "d2"), values); !ok {
		t.Fatal("array match")
	}
	if _, ok := evaluate(BlockTable, match("refDocId", "d"), values); ok {
		t.Fatal("partial array match")
	}
	must := &Query{Type: "boolean", Occur: "must", Queries: []*Query{match("flavour", "affine:paragraph"), {Type: "boolean", Occur: "must_not", Queries: []*Query{match("flavour", "affine:code")}}}}
	if _, ok := evaluate(BlockTable, must, values); !ok {
		t.Fatal("must/must_not")
	}
	should := &Query{Type: "boolean", Occur: "should", Queries: []*Query{match("flavour", "x"), match("content", "zzz")}}
	if _, ok := evaluate(BlockTable, should, values); ok {
		t.Fatal("should without matching child")
	}
	if score, _ := evaluate(BlockTable, &Query{Type: "boost", Boost: f64(2), Query: match("content", "foo")}, values); score != 4 {
		t.Fatalf("boost score %v", score)
	}
	if _, ok := evaluate(BlockTable, &Query{Type: "exists", Field: "blob"}, values); ok {
		t.Fatal("empty array exists")
	}
}

func TestHighlight(t *testing.T) {
	got, ok := highlightValue(match("content", "foo"), Highlight{Field: "content", Before: "<em>", End: "</em>"}, "a<b>Foo")
	if !ok || got != "a&lt;b&gt;<em>Foo</em>" {
		t.Fatalf("highlight %q", got)
	}
	if _, ok := highlightValue(match("content", "zzz"), Highlight{Field: "content", Before: "<em>", End: "</em>"}, "abc"); ok {
		t.Fatal("highlight without match")
	}
}

func hits(n int) []ProjectionHit {
	out := make([]ProjectionHit, n)
	for i := range out {
		out[i] = ProjectionHit{DocID: fmt.Sprint(i)}
	}
	return out
}

func TestApplySearchPagination(t *testing.T) {
	all := hits(25)
	page, more, err := ApplySearchPagination(all, Pagination{Limit: u32(10), Cursor: sp(`{"offset":20}`)})
	if err != nil || len(page) != 5 || more {
		t.Fatalf("cursor page %d %v %v", len(page), more, err)
	}
	page, more, err = ApplySearchPagination(all, Pagination{Skip: u32(5)})
	if err != nil || len(page) != 10 || page[0].DocID != "5" || page[9].DocID != "14" || !more {
		t.Fatalf("skip page %v %v", more, err)
	}
	for _, cursor := range []string{"x", `{"offset":-1}`, `{"offset":1.5}`} {
		if _, _, err := ApplySearchPagination(all, Pagination{Cursor: sp(cursor)}); !errors.Is(err, ErrInvalidQuery) {
			t.Fatalf("cursor %s: %v", cursor, err)
		}
	}
	if _, _, err := ApplySearchPagination(all, Pagination{Limit: u32(10001)}); !errors.Is(err, ErrInvalidQuery) {
		t.Fatal(err)
	}
}

func TestAggregateAndSelect(t *testing.T) {
	in := []ProjectionHit{
		{DocID: "1", Values: map[string]any{"flavour": "p", "ref": []any{"a", "a", "b"}}},
		{DocID: "2", Values: map[string]any{"flavour": "p"}},
		{DocID: "3", Values: map[string]any{"flavour": "h"}},
	}
	buckets, total := AggregateHits(in, "flavour", 0, 10, 0, 10)
	if total != 2 || len(buckets) != 2 || buckets[0].Key != "h" || buckets[0].Count != 1 || buckets[1].Key != "p" || buckets[1].Count != 2 {
		t.Fatalf("buckets %+v", buckets)
	}
	if b, _ := AggregateHits(in, "flavour", 5, 10, 0, 10); len(b) != 0 {
		t.Fatal("offset beyond total")
	}
	buckets, _ = AggregateHits(in, "ref", 0, 10, 0, 10)
	if len(buckets) != 2 || buckets[0].Count != 1 || buckets[1].Count != 1 {
		t.Fatalf("array buckets %+v", buckets)
	}
	fields := SelectFields(in[0], []string{"flavour", "ref", "missing"})
	if len(fields) != 2 || fields["flavour"].([]any)[0] != "p" || len(fields["ref"].([]any)) != 3 {
		t.Fatalf("select %v", fields)
	}
	scope := PermissionScope{WorkspaceID: "ws", ActorUserID: "u"}
	base := AggregateRequest{Table: BlockTable, Query: &Query{Type: "all"}, Field: "flavour"}
	if err := ValidateProjectionAggregate(base, scope); err != nil {
		t.Fatal(err)
	}
	withCursor := base
	withCursor.Options.Pagination.Cursor = sp(`{"offset":0}`)
	if err := ValidateProjectionAggregate(withCursor, scope); !errors.Is(err, ErrInvalidQuery) {
		t.Fatal("cursor accepted")
	}
	badField := base
	badField.Field = "visibility"
	if err := ValidateProjectionAggregate(badField, scope); !errors.Is(err, ErrInvalidQuery) {
		t.Fatal("bad field accepted")
	}
	if err := ValidateProjectionAggregate(base, PermissionScope{WorkspaceID: "ws"}); !errors.Is(err, ErrInvalidQuery) {
		t.Fatal("missing actor accepted")
	}
}

func TestRouteFingerprintGolden(t *testing.T) {
	target := byok.ChatTarget{ProfileID: "p1", Source: "byok_user", Provider: "openai", ModelID: "m",
		BaseURL: "https://api.example.com/v1", Endpoint: byok.Endpoint{Kind: "openai", URL: "https://e.example", Dialect: "d"}}
	h := func(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
	endpoint := h(`{"baseUrl":"https://api.example.com/v1","kind":"openai","url":"https://e.example","dialect":"d"}`)
	want := h(`{"source":"byok","profileId":"p1","provider":"openai","modelId":"m","endpoint":"` + endpoint + `","dimensions":1024,"contract":1}`)
	if got := routeFingerprint(target); got != want {
		t.Fatalf("fingerprint %s want %s", got, want)
	}
	if routeSource("managed_x") != "managed" || routeSource("byok") != "byok" {
		t.Fatal("route source")
	}
}

func TestVectorLiteral(t *testing.T) {
	v := make([]float32, byok.EmbeddingDimensions)
	v[0], v[1] = 0.1, -2
	lit, err := vectorLiteral(v)
	if err != nil || !strings.HasPrefix(lit, "[0.1,-2,0,") || !strings.HasSuffix(lit, ",0]") {
		t.Fatalf("literal %q %v", lit[:20], err)
	}
	if _, err := vectorLiteral(v[:3]); err == nil {
		t.Fatal("wrong dimension accepted")
	}
	v[5] = float32(math.NaN())
	if _, err := vectorLiteral(v); err == nil {
		t.Fatal("NaN accepted")
	}
}

func TestSplitText(t *testing.T) {
	pieces := splitText(strings.Repeat("a", 100*1024))
	if len(pieces) != 3 {
		t.Fatalf("pieces=%d", len(pieces))
	}
	for i, p := range pieces {
		if len(p) > maxChunkBytes {
			t.Fatalf("piece %d too large", i)
		}
	}
	if !strings.HasSuffix(pieces[0], pieces[1][:512]) {
		t.Fatal("missing overlap")
	}
	for _, p := range splitText(strings.Repeat("界", 40000)) {
		if !utf8.ValidString(p) || len(p) > maxChunkBytes {
			t.Fatal("split mid-rune")
		}
	}
}

func TestDocumentChunks(t *testing.T) {
	if _, err := documentChunks([]byte(`{"docId":"d","units":[{"unitId":"u","visibility":"page","text":"  "}]}`)); err == nil {
		t.Fatal("zero chunks accepted")
	}
	if _, err := documentChunks([]byte(`{"docId":"d","units":[{"unitId":"u","visibility":"other","text":"x"}]}`)); err == nil {
		t.Fatal("bad visibility accepted")
	}
	chunks, err := documentChunks([]byte(`{"docId":"d","units":[{"unitId":"u","visibility":"edgeless","text":"hi","blockId":"b"}]}`))
	if err != nil || len(chunks) != 1 || *chunks[0].unitID != "u" || *chunks[0].blockID != "b" {
		t.Fatalf("chunks %v %v", chunks, err)
	}
}

func zipOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, body := range files {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractArtifactText(t *testing.T) {
	docx := zipOf(t, map[string]string{"word/document.xml": `<w:document xmlns:w="w"><w:body><w:p><w:r><w:t>p1</w:t></w:r></w:p><w:p><w:r><w:t>p2</w:t></w:r></w:p></w:body></w:document>`})
	if parts, err := ExtractArtifactText(docx, "a.docx", ""); err != nil || len(parts) != 1 || parts[0] != "p1\np2" {
		t.Fatalf("docx %q %v", parts, err)
	}
	xlsx := zipOf(t, map[string]string{
		"xl/sharedStrings.xml":     `<sst><si><t>a</t></si><si><t>b</t></si><si><t>c</t></si></sst>`,
		"xl/worksheets/sheet1.xml": `<worksheet><sheetData><row><c t="s"><v>0</v></c><c t="s"><v>1</v></c></row><row><c t="s"><v>2</v></c><c t="inlineStr"><is><t>d</t></is></c></row></sheetData></worksheet>`,
	})
	if parts, err := ExtractArtifactText(xlsx, "a.XLSX", ""); err != nil || len(parts) != 1 || parts[0] != "a\tb\nc\td" {
		t.Fatalf("xlsx %q %v", parts, err)
	}
	badIndex := zipOf(t, map[string]string{"xl/worksheets/sheet1.xml": `<worksheet><sheetData><row><c t="s"><v>9</v></c></row></sheetData></worksheet>`})
	if _, err := ExtractArtifactText(badIndex, "a.xlsx", ""); err == nil {
		t.Fatal("bad shared index accepted")
	}
	pptx := zipOf(t, map[string]string{
		"ppt/slides/slide10.xml": `<p:sld xmlns:a="a" xmlns:p="p"><a:p><a:r><a:t>ten</a:t></a:r></a:p></p:sld>`,
		"ppt/slides/slide2.xml":  `<p:sld xmlns:a="a" xmlns:p="p"><a:p><a:r><a:t>two</a:t></a:r></a:p></p:sld>`,
	})
	if parts, err := ExtractArtifactText(pptx, "a.pptx", ""); err != nil || len(parts) != 2 || parts[0] != "two" || parts[1] != "ten" {
		t.Fatalf("pptx %q %v", parts, err)
	}
	if parts, err := ExtractArtifactText([]byte("<p>Hello</p><script>secret()</script><div>World</div>"), "a.html", ""); err != nil || strings.Contains(parts[0], "secret") || parts[0] != "Hello\nWorld" {
		t.Fatalf("html %q %v", parts, err)
	}
	if parts, err := ExtractArtifactText([]byte("a,b\nc,d"), "a.csv", ""); err != nil || parts[0] != "a\tb\nc\td" {
		t.Fatalf("csv %q %v", parts, err)
	}
	if parts, err := ExtractArtifactText([]byte(" hi "), "notes", "text/plain; charset=utf-8"); err != nil || parts[0] != "hi" {
		t.Fatalf("text %q %v", parts, err)
	}
	for _, tc := range []struct{ name, mime, body string }{
		{"a.bin", "", "x"}, {"a", "application/octet-stream", "x"}, {"a.txt", "", "a\x00b"},
	} {
		if _, err := ExtractArtifactText([]byte(tc.body), tc.name, tc.mime); !errors.Is(err, errUnsupportedFormat) {
			t.Fatalf("%+v: %v", tc, err)
		}
	}
	if _, err := ExtractArtifactText(nil, "a.txt", ""); err == nil {
		t.Fatal("empty accepted")
	}
}

func TestExtractZipBomb(t *testing.T) {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	f, err := w.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	chunk := bytes.Repeat([]byte(" "), 1<<20)
	for range 51 {
		if _, err := f.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ExtractArtifactText(buf.Bytes(), "a.docx", ""); err == nil {
		t.Fatal("zip bomb accepted")
	}
	// Forge a small declared size; the actual decompressed bytes must be bounded.
	data := buf.Bytes()
	r, _ := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if len(r.File) != 1 {
		t.Fatal("unexpected archive")
	}
	forged := append([]byte(nil), data...)
	// Patch uncompressed size (offset 24) in the central directory header.
	cd := bytes.LastIndex(forged, []byte("PK\x01\x02"))
	copy(forged[cd+24:cd+28], []byte{0x10, 0, 0, 0})
	if _, err := ExtractArtifactText(forged, "a.docx", ""); err == nil {
		t.Fatal("forged zip accepted")
	}
}
