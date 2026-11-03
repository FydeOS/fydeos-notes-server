// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package documents

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Deln0r/ygo"
)

func TestProjectSearchContentPreservesNestedDocumentOrder(t *testing.T) {
	update := searchProjectionUpdate(t)
	got, found, err := ProjectSearchContent([][]byte{update})
	if err != nil || !found {
		t.Fatalf("ProjectSearchContent() found=%t err=%v", found, err)
	}
	if got.Title != "Search title" {
		t.Fatalf("title = %q", got.Title)
	}
	want := []SearchUnit{
		{BlockID: "root", UnitID: "root", Flavour: "affine:page", Text: "Search title", Additional: `{"displayMode":"edgeless"}`},
		{BlockID: "first", UnitID: "first", Flavour: "affine:paragraph", Text: "first text", ParentFlavour: "affine:page", ParentBlockID: "root", Additional: `{"displayMode":"edgeless"}`},
		{BlockID: "nested", UnitID: "nested", Flavour: "affine:list", Text: "nested text", ParentFlavour: "affine:note", ParentBlockID: "container", Additional: `{"displayMode":"edgeless","noteBlockId":"container"}`},
		{BlockID: "last", UnitID: "last", Flavour: "affine:code", Text: "last text", ParentFlavour: "affine:page", ParentBlockID: "root", Additional: `{"displayMode":"edgeless"}`},
	}
	if !reflect.DeepEqual(got.Units, want) {
		t.Fatalf("units = %#v, want %#v", got.Units, want)
	}
	if got.Summary != "first textnested textlast text" {
		t.Fatalf("summary = %q", got.Summary)
	}
}

func TestProjectSearchContentRejectsInvalidAndNonPageDocuments(t *testing.T) {
	if _, _, err := ProjectSearchContent(nil); err == nil {
		t.Fatal("empty update list was accepted")
	}
	if _, _, err := ProjectSearchContent([][]byte{{0xff}}); err == nil {
		t.Fatal("invalid update was accepted")
	}
	doc := ygo.NewDoc()
	meta := ygo.NewMap(doc, "meta")
	txn := doc.WriteTxn()
	meta.Set(txn, "name", "workspace root")
	txn.Commit()
	if _, found, err := ProjectSearchContent([][]byte{ygo.EncodeStateAsUpdate(doc)}); err != nil || found {
		t.Fatalf("non-page projection found=%t err=%v", found, err)
	}
}

func searchProjectionUpdate(t *testing.T) []byte {
	t.Helper()
	doc := ygo.NewDoc()
	blocks := ygo.NewMap(doc, "blocks")
	txn := doc.WriteTxn()
	root := blocks.SetMap(txn, "root")
	root.Set(txn, "sys:id", "root")
	root.Set(txn, "sys:flavour", "affine:page")
	root.Set(txn, "prop:title", "Search title")
	children := root.SetArray(txn, "sys:children")
	children.Push(txn, "first")
	children.Push(txn, "container")
	children.Push(txn, "last")
	for _, child := range []struct {
		id, flavour, text string
		children          []string
	}{
		{id: "first", flavour: "affine:paragraph", text: "first text"},
		{id: "container", flavour: "affine:note", children: []string{"nested"}},
		{id: "nested", flavour: "affine:list", text: "nested text"},
		{id: "last", flavour: "affine:code", text: "last text"},
		{id: "orphan", flavour: "affine:paragraph", text: "not reachable"},
	} {
		block := blocks.SetMap(txn, child.id)
		block.Set(txn, "sys:id", child.id)
		block.Set(txn, "sys:flavour", child.flavour)
		if child.text != "" {
			text := block.SetText(txn, "prop:text")
			if err := text.Insert(txn, 0, child.text); err != nil {
				t.Fatal(err)
			}
		}
		if len(child.children) > 0 {
			array := block.SetArray(txn, "sys:children")
			for _, id := range child.children {
				array.Push(txn, id)
			}
		}
	}
	if got := children.Len(); got != 3 {
		t.Fatal(fmt.Sprintf("root children = %d", got))
	}
	txn.Commit()
	return ygo.EncodeStateAsUpdate(doc)
}

func TestSearchPreviewMatchesFrozenReader(t *testing.T) {
	data, err := os.ReadFile("testdata/search-preview-reader.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Summary  string
		SpaceID  string
		DocID    string
		Update   []byte
		Expected []struct {
			BlockID         string
			MarkdownPreview string
			Additional      map[string]any
			RefDocID        []string
		}
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	got, found, err := ProjectSearchContentInWorkspace([][]byte{fixture.Update}, fixture.SpaceID, fixture.DocID)
	if err != nil || !found {
		t.Fatalf("projection found=%v err=%v", found, err)
	}
	if got.Summary != fixture.Summary {
		t.Fatalf("summary=%q want=%q", got.Summary, fixture.Summary)
	}
	byID := map[string]SearchUnit{}
	for _, unit := range got.Units {
		byID[unit.BlockID] = unit
	}
	for _, want := range fixture.Expected {
		t.Run(want.BlockID, func(t *testing.T) {
			unit, ok := byID[want.BlockID]
			if !ok {
				t.Fatal("missing block")
			}
			if unit.MarkdownPreview != want.MarkdownPreview {
				t.Errorf("preview got=%q want=%q", unit.MarkdownPreview, want.MarkdownPreview)
			}
			var additional map[string]any
			if err := json.Unmarshal([]byte(unit.Additional), &additional); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(additional, want.Additional) {
				t.Errorf("additional=%v want=%v", additional, want.Additional)
			}
			if len(unit.RefDocIDs) != len(want.RefDocID) || (len(want.RefDocID) > 0 && !reflect.DeepEqual(unit.RefDocIDs, want.RefDocID)) {
				t.Errorf("refs=%v want=%v", unit.RefDocIDs, want.RefDocID)
			}
		})
	}
}

func TestSearchPreviewLongCodeFenceCompletesWithinBound(t *testing.T) {
	done := make(chan string, 1)
	go func() { done <- searchMarkdownFence(strings.Repeat("`", 1<<20), 3) }()
	select {
	case fence := <-done:
		if len(fence) != (1<<20)+1 {
			t.Fatal("fence closes inside code")
		}
	case <-time.After(time.Second):
		t.Fatal("fence scan exceeded one second")
	}
}

func TestSearchPreviewTruncationRespectsByteBudgetAndUTF8(t *testing.T) {
	const limit = 32
	preview := truncateSearchPreview(strings.Repeat("文", limit), limit)
	if len(preview) > limit {
		t.Fatalf("preview length = %d, budget = %d", len(preview), limit)
	}
	if !utf8.ValidString(preview) {
		t.Fatal("preview truncation produced invalid UTF-8")
	}
	if preview[len(preview)-3:] != "..." {
		t.Fatalf("preview lacks truncation marker: %q", preview)
	}
}
