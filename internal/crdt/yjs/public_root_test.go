// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package yjs

import (
	"testing"

	"github.com/Deln0r/ygo"
)

func TestBuildPublicRootDocFiltersTypedPagesAndCreatesStubs(t *testing.T) {
	source := ygo.NewDoc()
	meta := ygo.NewMap(source, "meta")
	txn := source.WriteTxn()
	pages := meta.SetArray(txn, "pages")
	publicPage := pages.InsertMap(txn, 0)
	publicPage.Set(txn, "id", "public")
	publicPage.Set(txn, "title", "Public title")
	publicPage.Set(txn, "createDate", float64(100))
	publicPage.Set(txn, "headerImage", "https://cdn.example/image.png")
	publicPage.Set(txn, "privateField", "must not escape")
	tags := publicPage.SetArray(txn, "tags")
	tags.Push(txn, "public-tag", float64(7), map[string]any{"private": "nested JSON"})
	nestedTag := tags.InsertMap(txn, 3)
	nestedTag.Set(txn, "secret", "nested Yjs")

	privatePage := pages.InsertMap(txn, 1)
	privatePage.Set(txn, "id", "private")
	privatePage.Set(txn, "title", "Private title")
	txn.Commit()

	update, err := BuildPublicRootDoc(ygo.EncodeStateAsUpdate(source), []PublicRootDocMeta{
		{ID: "public"},
		{ID: "missing", Title: stringPointer("Second page")},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := decodeRootDoc(t, update)
	resultPages := ygo.NewMap(result, "meta").Get("pages").(*ygo.Array)
	if resultPages.Len() != 2 {
		t.Fatalf("public page count = %d", resultPages.Len())
	}
	first := resultPages.Get(0).(*ygo.Map)
	if gotID := first.Get("id"); gotID != "public" {
		t.Fatalf("first page id = %#v", gotID)
	}
	if gotTitle := first.Get("title"); gotTitle != "Public title" {
		t.Fatalf("copied title = %#v", gotTitle)
	}
	if gotImage := first.Get("headerImage"); gotImage != "https://cdn.example/image.png" {
		t.Fatalf("copied header image = %#v", gotImage)
	}
	if first.Get("privateField") != nil {
		t.Fatalf("unexpected non-whitelisted page field: %#v", first.Get("privateField"))
	}
	resultTags := first.Get("tags").(*ygo.Array)
	if resultTags.Len() != 2 || resultTags.Get(0) != "public-tag" || resultTags.Get(1) != float64(7) {
		t.Fatalf("filtered tags = %#v, %#v", resultTags.Get(0), resultTags.Get(1))
	}
	second := resultPages.Get(1).(*ygo.Map)
	if second.Get("id") != "missing" || second.Get("title") != "Second page" {
		t.Fatalf("stub page = id:%#v title:%#v", second.Get("id"), second.Get("title"))
	}
	if second.Get("tags").(*ygo.Array).Len() != 0 || second.Get("createDate") == nil {
		t.Fatalf("stub metadata = tags:%#v created:%#v", second.Get("tags"), second.Get("createDate"))
	}
}

func TestBuildPublicRootDocReadsLegacyJSONPagesAndEmptyRoots(t *testing.T) {
	source := ygo.NewDoc()
	meta := ygo.NewMap(source, "meta")
	txn := source.WriteTxn()
	meta.Set(txn, "pages", []any{
		map[string]any{"id": "public", "title": "Legacy public", "tags": []any{"legacy-tag", map[string]any{"hidden": "value"}}, "unknown": "drop"},
		map[string]any{"id": "private", "title": "Legacy private"},
		"not a page",
	})
	txn.Commit()

	update, err := BuildPublicRootDoc(ygo.EncodeStateAsUpdate(source), []PublicRootDocMeta{{ID: "public"}})
	if err != nil {
		t.Fatal(err)
	}
	result := decodeRootDoc(t, update)
	pages := ygo.NewMap(result, "meta").Get("pages").(*ygo.Array)
	if pages.Len() != 1 || pages.Get(0).(*ygo.Map).Get("title") != "Legacy public" {
		t.Fatalf("legacy pages count/title = %d/%#v", pages.Len(), pages.Get(0))
	}
	legacyTags := pages.Get(0).(*ygo.Map).Get("tags").(*ygo.Array)
	if legacyTags.Len() != 1 || legacyTags.Get(0) != "legacy-tag" {
		t.Fatalf("legacy tags = %#v", legacyTags.Get(0))
	}

	for _, root := range [][]byte{nil, {0, 0}} {
		empty, err := BuildPublicRootDoc(root, nil)
		if err != nil {
			t.Fatal(err)
		}
		decoded := decodeRootDoc(t, empty)
		if ygo.NewMap(decoded, "meta").Get("pages").(*ygo.Array).Len() != 0 {
			t.Fatal("empty root unexpectedly contains pages")
		}
	}
}

func decodeRootDoc(t *testing.T, update []byte) *ygo.Doc {
	t.Helper()
	result := ygo.NewDoc()
	if err := ygo.ApplyUpdate(result, update); err != nil {
		t.Fatalf("public root update is invalid: %v", err)
	}
	return result
}

func stringPointer(value string) *string { return &value }
