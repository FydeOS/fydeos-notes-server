// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package documents

import (
	"fmt"
	"testing"

	"github.com/Deln0r/ygo"
)

type pageMetadataBlock struct {
	id      string
	flavour string
	text    string
}

func TestProjectPageMetadataUsesRootTitleAndDocumentOrder(t *testing.T) {
	update := pageMetadataUpdate(t, "Page title", []pageMetadataBlock{
		{id: "first", flavour: "affine:paragraph", text: "First"},
		{id: "ignored", flavour: "affine:image", text: "not summary text"},
		{id: "second", flavour: "affine:list", text: " second"},
		{id: "third", flavour: "affine:code", text: " third"},
	})

	got, found, err := ProjectPageMetadata([][]byte{update}, 150)
	if err != nil || !found {
		t.Fatalf("ProjectPageMetadata() found=%t err=%v", found, err)
	}
	if got.Title != "Page title" || got.Summary != "First second third" {
		t.Fatalf("ProjectPageMetadata() = %+v", got)
	}
}

func TestProjectPageMetadataUsesUTF16LimitWithoutSplittingTextBlock(t *testing.T) {
	update := pageMetadataUpdate(t, "Title", []pageMetadataBlock{
		{id: "first", flavour: "affine:paragraph", text: "a"},
		{id: "second", flavour: "affine:paragraph", text: "😀bc"},
		{id: "third", flavour: "affine:paragraph", text: "ignored"},
	})

	got, found, err := ProjectPageMetadata([][]byte{update}, 2)
	if err != nil || !found {
		t.Fatalf("ProjectPageMetadata() found=%t err=%v", found, err)
	}
	if got.Summary != "a😀bc" {
		t.Fatalf("summary = %q, want complete second text block", got.Summary)
	}
}

func TestProjectWorkspaceMetadataReadsRootMeta(t *testing.T) {
	doc := ygo.NewDoc()
	meta := ygo.NewMap(doc, "meta")
	txn := doc.WriteTxn()
	meta.Set(txn, "name", "Root name")
	meta.Set(txn, "avatar", "avatar-key")
	txn.Commit()

	got, found, err := ProjectWorkspaceMetadata([][]byte{ygo.EncodeStateAsUpdate(doc)})
	if err != nil || !found {
		t.Fatalf("ProjectWorkspaceMetadata() found=%t err=%v", found, err)
	}
	if got.Name != "Root name" || got.AvatarKey != "avatar-key" {
		t.Fatalf("ProjectWorkspaceMetadata() = %+v", got)
	}
}

func TestProjectWorkspaceMetadataWithoutMetaRootIsNotFound(t *testing.T) {
	doc := ygo.NewDoc()
	blocks := ygo.NewMap(doc, "blocks")
	txn := doc.WriteTxn()
	blocks.Set(txn, "unrelated", "value")
	txn.Commit()

	if got, found, err := ProjectWorkspaceMetadata([][]byte{ygo.EncodeStateAsUpdate(doc)}); err != nil || found {
		t.Fatalf("ProjectWorkspaceMetadata() = %+v, found=%t, err=%v", got, found, err)
	}
}

func TestProjectWorkspaceMetadataFindsDeletedMetaRoot(t *testing.T) {
	doc := ygo.NewDoc()
	meta := ygo.NewMap(doc, "meta")
	txn := doc.WriteTxn()
	meta.Set(txn, "name", "old name")
	txn.Commit()
	txn = doc.WriteTxn()
	meta.Delete(txn, "name")
	txn.Commit()

	got, found, err := ProjectWorkspaceMetadata([][]byte{ygo.EncodeStateAsUpdate(doc)})
	if err != nil || !found || got.Name != "" {
		t.Fatalf("ProjectWorkspaceMetadata() = %+v, found=%t, err=%v", got, found, err)
	}
}

func TestProjectMetadataRejectsInvalidUpdatesAndMissingPageBlocks(t *testing.T) {
	if _, _, err := ProjectPageMetadata([][]byte{{0xff}}, 150); err == nil {
		t.Fatal("invalid Yjs update was accepted")
	}

	doc := ygo.NewDoc()
	meta := ygo.NewMap(doc, "meta")
	txn := doc.WriteTxn()
	meta.Set(txn, "name", "workspace only")
	txn.Commit()
	if _, found, err := ProjectPageMetadata([][]byte{ygo.EncodeStateAsUpdate(doc)}, 150); err != nil || found {
		t.Fatalf("page projection without blocks found=%t err=%v", found, err)
	}
}

func pageMetadataUpdate(t *testing.T, title string, children []pageMetadataBlock) []byte {
	t.Helper()
	doc := ygo.NewDoc()
	blocks := ygo.NewMap(doc, "blocks")
	txn := doc.WriteTxn()
	root := blocks.SetMap(txn, "root")
	root.Set(txn, "sys:id", "root")
	root.Set(txn, "sys:flavour", "affine:page")
	root.Set(txn, "prop:title", title)
	childIDs := root.SetArray(txn, "sys:children")
	for i, child := range children {
		childID := child.id
		if childID == "" {
			childID = fmt.Sprintf("child-%d", i)
		}
		childIDs.Push(txn, childID)
		block := blocks.SetMap(txn, childID)
		block.Set(txn, "sys:id", childID)
		block.Set(txn, "sys:flavour", child.flavour)
		if child.text != "" {
			text := block.SetText(txn, "prop:text")
			if err := text.Insert(txn, 0, child.text); err != nil {
				t.Fatal(err)
			}
		}
	}
	txn.Commit()
	return ygo.EncodeStateAsUpdate(doc)
}
