// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package documents

import (
	"bytes"
	"flag"
	"os"
	"testing"

	"github.com/Deln0r/ygo"
)

// Regenerate the sample document fixtures with:
//
//	go test ./internal/documents -run TestSampleDocumentFixture -update
var updateGolden = flag.Bool("update", false, "rewrite golden test fixtures")

const (
	sampleDocumentSnapshot = "testdata/sample-document.snapshot.bin"
	sampleDocumentMarkdown = "testdata/sample-document.md"
)

// sampleDocumentBlocks covers the common block types and inline styles. The
// encoded Yjs update is also used by tests in other packages.
func sampleDocumentBlocks() []markdownTestBlock {
	return []markdownTestBlock{
		{id: "intro-heading", parent: "root", flavour: "affine:paragraph", props: map[string]any{"prop:type": "h1"}, text: "Field notes"},
		{id: "intro", parent: "root", flavour: "affine:paragraph", text: "Notes from the spring garden survey.", formats: []markdownTestFormat{
			{start: 15, length: 6, attrs: ygo.Attrs{"bold": true}},
			{start: 22, length: 6, attrs: ygo.Attrs{"italic": true}},
		}},
		{id: "link", parent: "root", flavour: "affine:paragraph", text: "See the planting guide for details.", formats: []markdownTestFormat{
			{start: 8, length: 14, attrs: ygo.Attrs{"link": "https://example.org/planting"}},
		}},
		{id: "plants-heading", parent: "root", flavour: "affine:paragraph", props: map[string]any{"prop:type": "h2"}, text: "Plants"},
		{id: "tomato", parent: "root", flavour: "affine:list", props: map[string]any{"prop:type": "bulleted"}, text: "Tomatoes"},
		{id: "tomato-cherry", parent: "tomato", flavour: "affine:list", props: map[string]any{"prop:type": "bulleted"}, text: "Cherry"},
		{id: "basil", parent: "root", flavour: "affine:list", props: map[string]any{"prop:type": "bulleted"}, text: "Basil"},
		{id: "steps-heading", parent: "root", flavour: "affine:paragraph", props: map[string]any{"prop:type": "h2"}, text: "Steps"},
		{id: "step-1", parent: "root", flavour: "affine:list", props: map[string]any{"prop:type": "numbered"}, text: "Water in the morning"},
		{id: "step-2", parent: "root", flavour: "affine:list", props: map[string]any{"prop:type": "numbered"}, text: "Check for pests"},
		{id: "todo-done", parent: "root", flavour: "affine:list", props: map[string]any{"prop:type": "todo", "prop:checked": true}, text: "Buy compost"},
		{id: "todo-open", parent: "root", flavour: "affine:list", props: map[string]any{"prop:type": "todo", "prop:checked": false}, text: "Fix the fence"},
		{id: "quote", parent: "root", flavour: "affine:paragraph", props: map[string]any{"prop:type": "quote"}, text: "Plant what you like to eat."},
		{id: "divider", parent: "root", flavour: "affine:divider"},
		{id: "code", parent: "root", flavour: "affine:code", props: map[string]any{"prop:language": "python"}, text: "print(\"harvest\")"},
		{id: "closing", parent: "root", flavour: "affine:paragraph", text: "Next survey in June."},
	}
}

func TestSampleDocumentFixture(t *testing.T) {
	update := markdownTestUpdate(t, sampleDocumentBlocks())
	got, found, err := ProjectMarkdown([][]byte{update})
	if err != nil || !found {
		t.Fatalf("ProjectMarkdown(sample) found=%t err=%v", found, err)
	}
	if *updateGolden {
		if err := os.WriteFile(sampleDocumentSnapshot, update, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(sampleDocumentMarkdown, []byte(got.Markdown), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(sampleDocumentMarkdown)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal([]byte(got.Markdown), want) {
		t.Fatalf("sample Markdown differs: %s", markdownDifference(string(want), got.Markdown))
	}

	// The stored snapshot must render to the same Markdown, so other packages
	// can rely on the pair.
	stored, err := os.ReadFile(sampleDocumentSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	fromStored, found, err := ProjectMarkdown([][]byte{stored})
	if err != nil || !found || fromStored.Title != "Workspace title" || fromStored.Markdown != string(want) {
		t.Fatalf("stored sample snapshot = %+v, found=%t, err=%v", fromStored, found, err)
	}
}

func TestProjectMarkdownHandlesDocumentWithoutContent(t *testing.T) {
	got, found, err := ProjectMarkdown([][]byte{markdownTestUpdate(t, nil)})
	if err != nil || !found || got.Title != "Workspace title" || got.Markdown != "" {
		t.Fatalf("empty document = %+v, found=%t, err=%v", got, found, err)
	}
}
