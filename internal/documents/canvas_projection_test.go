// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package documents

import (
	"reflect"
	"testing"

	"github.com/Deln0r/ygo"
)

func TestProjectCanvasProjectsBlocksSurfaceAndBounds(t *testing.T) {
	update := canvasProjectionUpdate(t)
	projection, found, err := ProjectCanvas([][]byte{update}, "doc-1", "rev-1")
	if err != nil || !found {
		t.Fatalf("ProjectCanvas() found=%t err=%v", found, err)
	}
	if projection.Title != "Canvas" || projection.SurfaceBlockID != "surface" || projection.Version != 1 {
		t.Fatalf("projection metadata = %#v", projection)
	}
	if len(projection.Blocks) != 3 || projection.Blocks[0].ID != "frame" || projection.Blocks[0].Visibility != "page" || projection.Blocks[1].ID != "note" || projection.Blocks[1].Visibility != "edgeless" || projection.Blocks[2].ID != "text" || projection.Blocks[2].Visibility != "edgeless" || projection.Blocks[2].Text != "Frame note" {
		t.Fatalf("canvas blocks = %#v", projection.Blocks)
	}
	if got := projection.Blocks[0].ChildIDs; !reflect.DeepEqual(got, []string{"shape"}) {
		t.Fatalf("frame child IDs = %#v", got)
	}
	if len(projection.Elements) != 1 || projection.Elements[0].ID != "shape" || projection.Elements[0].Type != "shape" || projection.Elements[0].FrameID != "frame" {
		t.Fatalf("canvas elements = %#v", projection.Elements)
	}
	if projection.Bounds == nil || *projection.Bounds != (CanvasBounds{X: 1, Y: 2, Width: 19, Height: 28}) || projection.Counts["shape"] != 1 {
		t.Fatalf("canvas aggregate bounds/counts = %#v/%#v", projection.Bounds, projection.Counts)
	}
}

func TestProjectCanvasRejectsInvalidUpdatesAndReportsBounds(t *testing.T) {
	if _, found, err := ProjectCanvas([][]byte{{0xff}}, "doc-1", "rev-1"); err == nil || found {
		t.Fatalf("invalid Yjs found=%t err=%v", found, err)
	}
	doc := ygo.NewDoc()
	blocks := ygo.NewMap(doc, "blocks")
	txn := doc.WriteTxn()
	root := blocks.SetMap(txn, "root")
	root.Set(txn, "sys:id", "root")
	root.Set(txn, "sys:flavour", "affine:page")
	root.Set(txn, "prop:title", "No canvas")
	txn.Commit()
	if _, found, err := ProjectCanvas([][]byte{ygo.EncodeStateAsUpdate(doc)}, "doc-1", "rev-1"); err != nil || !found {
		t.Fatalf("page-only projection found=%t err=%v", found, err)
	}
}

func TestCanvasVisibilityRejectsCyclicParents(t *testing.T) {
	blocks := map[string]*canvasBlockData{
		"a": {id: "a", flavour: "affine:paragraph"},
		"b": {id: "b", flavour: "affine:paragraph"},
	}
	for _, parents := range []map[string]string{
		{"a": "a"},
		{"a": "b", "b": "a"},
	} {
		if got := canvasVisibility("a", parents, blocks); got != "page" {
			t.Fatalf("cyclic hierarchy visibility = %q, want page", got)
		}
	}
}

func TestCanvasElementsPreservePlainConnectionsAndLabel(t *testing.T) {
	doc := ygo.NewDoc()
	blocks := ygo.NewMap(doc, "blocks")
	txn := doc.WriteTxn()
	surface := blocks.SetMap(txn, "surface")
	surface.Set(txn, "sys:id", "surface")
	surface.Set(txn, "sys:flavour", "affine:surface")
	container := surface.SetMap(txn, "prop:elements")
	container.Set(txn, "type", "$blocksuite:internal:native$")
	values := container.SetMap(txn, "value")
	connector := values.SetMap(txn, "connector")
	connector.Set(txn, "type", "connector")
	connector.Set(txn, "source", map[string]any{"id": "shape-a"})
	connector.Set(txn, "target", map[string]any{"id": "shape-b"})
	connector.Set(txn, "label", "connects shapes")
	txn.Commit()
	projection, found, err := ProjectCanvas([][]byte{ygo.EncodeStateAsUpdate(doc)}, "doc", "rev")
	if err != nil || !found || len(projection.Elements) != 1 {
		t.Fatalf("connector projection found=%t err=%v elements=%#v", found, err, projection.Elements)
	}
	element := projection.Elements[0]
	if element.SourceID != "shape-a" || element.TargetID != "shape-b" || element.Text != "connects shapes" {
		t.Fatalf("connector metadata lost: %#v", element)
	}
	txn = doc.WriteTxn()
	connector.Set(txn, "text", "")
	txn.Commit()
	projection, found, err = ProjectCanvas([][]byte{ygo.EncodeStateAsUpdate(doc)}, "doc", "rev-2")
	if err != nil || !found || len(projection.Elements) != 1 || projection.Elements[0].Text != "" {
		t.Fatalf("explicit empty text must take precedence over label: %#v, %v", projection, err)
	}
}

func canvasProjectionUpdate(t *testing.T) []byte {
	t.Helper()
	doc := ygo.NewDoc()
	blocks := ygo.NewMap(doc, "blocks")
	txn := doc.WriteTxn()
	root := blocks.SetMap(txn, "root")
	root.Set(txn, "sys:id", "root")
	root.Set(txn, "sys:flavour", "affine:page")
	root.Set(txn, "prop:title", "Canvas")
	rootChildren := root.SetArray(txn, "sys:children")
	rootChildren.Push(txn, "note")
	rootChildren.Push(txn, "frame")
	rootChildren.Push(txn, "surface")

	note := blocks.SetMap(txn, "note")
	note.Set(txn, "sys:id", "note")
	note.Set(txn, "sys:flavour", "affine:note")
	note.Set(txn, "prop:displayMode", "edgeless")
	noteChildren := note.SetArray(txn, "sys:children")
	noteChildren.Push(txn, "text")

	text := blocks.SetMap(txn, "text")
	text.Set(txn, "sys:id", "text")
	text.Set(txn, "sys:flavour", "affine:edgeless-text")
	textValue := text.SetText(txn, "prop:text")
	if err := textValue.Insert(txn, 0, "Frame note"); err != nil {
		t.Fatal(err)
	}
	setCanvasProjectionBounds(t, txn, text, "prop:xywh", 2, 3, 4, 5)

	frame := blocks.SetMap(txn, "frame")
	frame.Set(txn, "sys:id", "frame")
	frame.Set(txn, "sys:flavour", "affine:frame")
	setCanvasProjectionBounds(t, txn, frame, "prop:xywh", 1, 2, 10, 20)
	frameChildren := frame.SetMap(txn, "prop:childElementIds")
	frameChildren.Set(txn, "shape", true)

	surface := blocks.SetMap(txn, "surface")
	surface.Set(txn, "sys:id", "surface")
	surface.Set(txn, "sys:flavour", "affine:surface")
	elements := surface.SetMap(txn, "prop:elements")
	elements.Set(txn, "type", "$blocksuite:internal:native$")
	values := elements.SetMap(txn, "value")
	shape := values.SetMap(txn, "shape-entry")
	shape.Set(txn, "id", "shape")
	shape.Set(txn, "type", "shape")
	setCanvasProjectionBounds(t, txn, shape, "xywh", 10, 10, 10, 20)
	txn.Commit()
	return ygo.EncodeStateAsUpdate(doc)
}

func setCanvasProjectionBounds(t *testing.T, txn *ygo.TransactionMut, block *ygo.Map, key string, values ...float64) {
	t.Helper()
	array := block.SetArray(txn, key)
	for _, value := range values {
		array.Push(txn, value)
	}
}
