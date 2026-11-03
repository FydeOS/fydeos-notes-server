// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package documents

import (
	"testing"

	"github.com/Deln0r/ygo"
)

func TestWorkspaceRootAppendAllowsPageMetadataButRequiresLifecycleCommands(t *testing.T) {
	base := rootPageUpdate(t, 12001, "doc-1")
	addPage := mutateWorkspaceRoot(t, base, 12002, func(txn *ygo.TransactionMut, pages *ygo.Array, _ *ygo.Map) {
		page := pages.InsertMap(txn, pages.Len())
		page.Set(txn, "id", "doc-2")
		page.Set(txn, "title", "Second page")
	})
	if _, err := validateWorkspaceRootAppend(base, nil, addPage); err != nil {
		t.Fatalf("ordinary page addition was rejected: %v", err)
	}

	for name, mutate := range map[string]func(*ygo.TransactionMut, *ygo.Array, *ygo.Map){
		"trash": func(txn *ygo.TransactionMut, _ *ygo.Array, page *ygo.Map) {
			page.Set(txn, "trash", true)
		},
		"delete": func(txn *ygo.TransactionMut, pages *ygo.Array, _ *ygo.Map) {
			pages.Delete(txn, 0, 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			update := mutateWorkspaceRoot(t, base, 12003, mutate)
			if _, err := validateWorkspaceRootAppend(base, nil, update); err != ErrWorkspaceRootCommandRequired {
				t.Fatalf("lifecycle update error = %v, want ErrWorkspaceRootCommandRequired", err)
			}
		})
	}
}

func TestWorkspaceRootLifecycleEmitsTrashRestoreAndDeleteDeltas(t *testing.T) {
	base := rootPageUpdate(t, 12101, "doc-1")
	trash, err := mutateWorkspaceRootLifecycle(base, nil, "doc-1", WorkspaceDocTrash)
	if err != nil {
		t.Fatalf("trash workspace document: %v", err)
	}
	trashed := restoreRootForLifecycleTest(t, base, trash)
	trashedPage := workspaceRootPage(t, trashed, 0)
	if trashedPage.Get("trash") != true || trashedPage.Get("trashDate") == nil {
		t.Fatalf("trashed root metadata = %#v, want trash=true and a trashDate", trashedPage)
	}

	restore, err := mutateWorkspaceRootLifecycle(base, [][]byte{trash}, "doc-1", WorkspaceDocRestore)
	if err != nil {
		t.Fatalf("restore workspace document: %v", err)
	}
	restored := restoreRootForLifecycleTest(t, base, trash, restore)
	restoredPage := workspaceRootPage(t, restored, 0)
	if restoredPage.Get("trash") != false || restoredPage.Get("trashDate") != nil {
		t.Fatalf("restored root metadata = %#v, want trash=false without trashDate", restoredPage)
	}

	deleted, err := mutateWorkspaceRootLifecycle(base, nil, "doc-1", WorkspaceDocDelete)
	if err != nil {
		t.Fatalf("delete workspace document: %v", err)
	}
	deletedRoot := restoreRootForLifecycleTest(t, base, deleted)
	pages, ok := ygo.NewMap(deletedRoot, "meta").Get("pages").(*ygo.Array)
	if !ok || pages.Len() != 0 {
		t.Fatalf("root pages after delete = %#v, want empty array", pages)
	}
	if _, err := mutateWorkspaceRootLifecycle(base, nil, "missing", WorkspaceDocTrash); err != ErrWorkspaceDocNotFound {
		t.Fatalf("unknown lifecycle target error = %v, want ErrWorkspaceDocNotFound", err)
	}
}

func restoreRootForLifecycleTest(t *testing.T, updates ...[]byte) *ygo.Doc {
	t.Helper()
	doc := ygo.NewDoc()
	for _, update := range updates {
		if err := ygo.ApplyUpdate(doc, update); err != nil {
			t.Fatalf("apply lifecycle root update: %v", err)
		}
	}
	if ygo.HasPending(doc) {
		t.Fatal("lifecycle root updates left pending dependencies")
	}
	return doc
}

func workspaceRootPage(t *testing.T, root *ygo.Doc, index uint64) *ygo.Map {
	t.Helper()
	pages, ok := ygo.NewMap(root, "meta").Get("pages").(*ygo.Array)
	if !ok {
		t.Fatal("workspace root has no pages array")
	}
	page, ok := pages.Get(index).(*ygo.Map)
	if !ok {
		t.Fatalf("workspace root page %d is not a map", index)
	}
	return page
}

func rootPageUpdate(t *testing.T, clientID uint64, docID string) []byte {
	t.Helper()
	doc := ygo.NewDocWithOptions(ygo.Options{ClientID: clientID})
	meta := ygo.NewMap(doc, "meta")
	txn := doc.WriteTxn()
	pages := meta.SetArray(txn, "pages")
	page := pages.InsertMap(txn, 0)
	page.Set(txn, "id", docID)
	page.Set(txn, "title", "Untitled")
	txn.Commit()
	return ygo.EncodeStateAsUpdate(doc)
}

func mutateWorkspaceRoot(t *testing.T, snapshot []byte, clientID uint64, mutate func(*ygo.TransactionMut, *ygo.Array, *ygo.Map)) []byte {
	t.Helper()
	doc := ygo.NewDocWithOptions(ygo.Options{ClientID: clientID})
	if err := ygo.ApplyUpdate(doc, snapshot); err != nil {
		t.Fatalf("apply root fixture: %v", err)
	}
	stateVector := ygo.EncodeStateVector(doc)
	meta := ygo.NewMap(doc, "meta")
	pages, ok := meta.Get("pages").(*ygo.Array)
	if !ok {
		t.Fatal("root fixture has no pages array")
	}
	page, ok := pages.Get(0).(*ygo.Map)
	if !ok {
		t.Fatal("root fixture has no first page")
	}
	txn := doc.WriteTxn()
	mutate(txn, pages, page)
	txn.Commit()
	update, err := ygo.EncodeDiff(doc, stateVector)
	if err != nil {
		t.Fatalf("encode root fixture delta: %v", err)
	}
	return update
}

func mutateWorkspaceProfile(t *testing.T, base []byte, clientID uint64, name, avatar string) []byte {
	t.Helper()
	doc := ygo.NewDocWithOptions(ygo.Options{ClientID: clientID})
	if err := ygo.ApplyUpdate(doc, base); err != nil {
		t.Fatal(err)
	}
	state := ygo.EncodeStateVector(doc)
	meta := ygo.NewMap(doc, "meta")
	txn := doc.WriteTxn()
	meta.Set(txn, "name", name)
	meta.Set(txn, "avatar", avatar)
	txn.Commit()
	update, err := ygo.EncodeDiff(doc, state)
	if err != nil {
		t.Fatal(err)
	}
	return update
}
