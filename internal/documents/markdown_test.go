// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package documents

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Deln0r/ygo"
)

type markdownTestBlock struct {
	id      string
	parent  string
	flavour string
	props   map[string]any
	text    string
	attrs   ygo.Attrs
	formats []markdownTestFormat
}

type markdownTestFormat struct {
	start, length uint64
	attrs         ygo.Attrs
}

func TestProjectMarkdownRendersOrderedBlocksAndRichText(t *testing.T) {
	update := markdownTestUpdate(t, []markdownTestBlock{
		{id: "heading", parent: "root", flavour: "affine:paragraph", props: map[string]any{"prop:type": "h2"}, text: "Heading"},
		{id: "bold", parent: "root", flavour: "affine:paragraph", text: "bold", attrs: ygo.Attrs{"bold": true}},
		{id: "list", parent: "root", flavour: "affine:list", text: "item"},
		{id: "nested", parent: "list", flavour: "affine:list", props: map[string]any{"prop:type": "todo", "prop:checked": true}, text: "nested"},
		{id: "code", parent: "root", flavour: "affine:code", props: map[string]any{"prop:language": "go"}, text: "fmt.Println(\"ok\")"},
	})

	got, found, err := ProjectMarkdown([][]byte{update})
	if err != nil || !found {
		t.Fatalf("ProjectMarkdown() found=%t err=%v", found, err)
	}
	if got.Title != "Workspace title" {
		t.Fatalf("title = %q", got.Title)
	}
	want := "## Heading\n\n**bold**\n\n* item\n    - [x] nested\n\n```go\nfmt.Println(\"ok\")\n```\n\n"
	if got.Markdown != want {
		t.Fatalf("markdown = %q, want %q", got.Markdown, want)
	}
}

func TestMarkdownTableCollapsesRepeatedNewlines(t *testing.T) {
	var output strings.Builder
	writeMarkdownTable(&output, [][]string{{"Header"}, {"first\n\nsecond"}})
	if want := "|Header|\n|---|\n|first<br />second|\n\n"; output.String() != want {
		t.Fatalf("table = %q, want %q", output.String(), want)
	}
}

func TestProjectMarkdownKeepsInlineStylesAcrossAttributeChanges(t *testing.T) {
	update := markdownTestUpdate(t, []markdownTestBlock{{
		id: "mixed", parent: "root", flavour: "affine:paragraph", text: "Open docs",
		formats: []markdownTestFormat{
			{start: 0, length: 9, attrs: ygo.Attrs{"bold": true}},
			{start: 5, length: 4, attrs: ygo.Attrs{"link": "https://example.test/docs"}},
		},
	}})
	got, found, err := ProjectMarkdown([][]byte{update})
	if err != nil || !found {
		t.Fatalf("ProjectMarkdown() found=%t err=%v", found, err)
	}
	if want := "**Open [docs](https://example.test/docs)**\n\n"; got.Markdown != want {
		t.Fatalf("markdown = %q, want %q", got.Markdown, want)
	}
}

func TestProjectMarkdownRendersInlinePageReferences(t *testing.T) {
	update := markdownTestUpdate(t, []markdownTestBlock{{
		id: "reference", parent: "root", flavour: "affine:paragraph", text: " ",
		attrs: ygo.Attrs{"reference": map[string]any{"pageId": "page-id", "title": "Related"}},
	}})
	got, found, err := ProjectMarkdown([][]byte{update})
	if err != nil || !found {
		t.Fatalf("ProjectMarkdown() found=%t err=%v", found, err)
	}
	if want := "[Related](LinkedPage:page-id)\n\n"; got.Markdown != want {
		t.Fatalf("markdown = %q, want %q", got.Markdown, want)
	}
}

func markdownDifference(want, got string) string {
	index := 0
	for index < len(want) && index < len(got) && want[index] == got[index] {
		index++
	}
	start := max(0, index-32)
	return fmt.Sprintf("byte %d: want %q, got %q (lengths %d and %d)", index, want[start:min(len(want), index+64)], got[start:min(len(got), index+64)], len(want), len(got))
}

func TestProjectMarkdownRejectsMalformedYjs(t *testing.T) {
	if _, found, err := ProjectMarkdown([][]byte{{0xff}}); err == nil || found {
		t.Fatalf("malformed update result found=%t err=%v", found, err)
	}
}

func markdownTestUpdate(t *testing.T, children []markdownTestBlock) []byte {
	t.Helper()
	doc := ygo.NewDoc()
	blocks := ygo.NewMap(doc, "blocks")
	txn := doc.WriteTxn()
	root := blocks.SetMap(txn, "root")
	root.Set(txn, "sys:id", "root")
	root.Set(txn, "sys:flavour", "affine:page")
	root.Set(txn, "prop:title", "Workspace title")
	root.SetArray(txn, "sys:children")
	created := map[string]*ygo.Map{"root": root}
	for _, definition := range children {
		block := blocks.SetMap(txn, definition.id)
		block.Set(txn, "sys:id", definition.id)
		block.Set(txn, "sys:flavour", definition.flavour)
		created[definition.id] = block
	}
	for _, definition := range children {
		block := created[definition.id]
		for key, value := range definition.props {
			block.Set(txn, key, value)
		}
		if definition.text != "" {
			text := block.SetText(txn, "prop:text")
			var err error
			if len(definition.attrs) > 0 {
				err = text.InsertWithAttributes(txn, 0, definition.text, definition.attrs)
			} else {
				err = text.Insert(txn, 0, definition.text)
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, format := range definition.formats {
				if err := text.Format(txn, format.start, format.length, format.attrs); err != nil {
					t.Fatal(err)
				}
			}
		}
		parent := created[definition.parent]
		childrenArray, ok := parent.Get("sys:children").(*ygo.Array)
		if !ok {
			childrenArray = parent.SetArray(txn, "sys:children")
		}
		childrenArray.Push(txn, definition.id)
	}
	txn.Commit()
	return ygo.EncodeStateAsUpdate(doc)
}

func TestProjectMarkdownMovesWhitespaceOutsideEmphasisAndKeepsCodeInnermost(t *testing.T) {
	update := markdownTestUpdate(t, []markdownTestBlock{{
		id: "mixed", parent: "root", flavour: "affine:paragraph", text: "Run go test now",
		formats: []markdownTestFormat{
			{start: 0, length: 4, attrs: ygo.Attrs{"bold": true}},
			{start: 4, length: 7, attrs: ygo.Attrs{"code": true, "italic": true}},
		},
	}})
	got, found, err := ProjectMarkdown([][]byte{update})
	if err != nil || !found {
		t.Fatalf("ProjectMarkdown() found=%t err=%v", found, err)
	}
	if want := "**Run** _`go test`_ now\n\n"; got.Markdown != want {
		t.Fatalf("markdown = %q, want %q", got.Markdown, want)
	}
}

func TestProjectMarkdownRendersTableBlocks(t *testing.T) {
	update := markdownTestUpdate(t, []markdownTestBlock{{
		id: "table", parent: "root", flavour: "affine:table", props: map[string]any{
			"prop:rows.r1.rowId": "r1", "prop:rows.r1.order": "a0",
			"prop:rows.r2.rowId": "r2", "prop:rows.r2.order": "a1",
			"prop:columns.c1.columnId": "c1", "prop:columns.c1.order": "a1",
			"prop:columns.c2.columnId": "c2", "prop:columns.c2.order": "a0",
			"prop:cells.r1:c1.text": "Name", "prop:cells.r1:c2.text": "Id",
			"prop:cells.r2:c1.text": "a|b", "prop:cells.r2:c2.text": "1",
		},
	}, {id: "after", parent: "root", flavour: "affine:paragraph", text: "After"}})
	got, found, err := ProjectMarkdown([][]byte{update})
	if err != nil || !found {
		t.Fatalf("ProjectMarkdown() found=%t err=%v", found, err)
	}
	if want := "|Id|Name|\n|---|---|\n|1|a\\|b|\n\nAfter\n\n"; got.Markdown != want {
		t.Fatalf("markdown = %q, want %q", got.Markdown, want)
	}
}
