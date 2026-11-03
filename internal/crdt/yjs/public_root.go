// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package yjs

import (
	"bytes"
	"time"

	"github.com/Deln0r/ygo"
)

const defaultPublicDocTitle = "Untitled"

var simplePublicPageMetaKeys = []string{"id", "title", "createDate", "updatedDate", "trash", "trashDate", "headerImage"}

type PublicRootDocMeta struct {
	ID    string
	Title *string
}

// BuildPublicRootDoc creates a Yjs update containing only public pages from a workspace root document.
func BuildPublicRootDoc(rootUpdate []byte, metas []PublicRootDocMeta) ([]byte, error) {
	source := ygo.NewDoc()
	if len(rootUpdate) > 0 && !bytes.Equal(rootUpdate, []byte{0, 0}) {
		if err := ygo.ApplyUpdate(source, rootUpdate); err != nil {
			return nil, err
		}
	}

	publicIDs := make(map[string]struct{}, len(metas))
	for _, meta := range metas {
		publicIDs[meta.ID] = struct{}{}
	}

	output := ygo.NewDoc()
	outputMeta := ygo.NewMap(output, "meta")
	txn := output.WriteTxn()
	pages := outputMeta.SetArray(txn, "pages")
	copied := make(map[string]struct{}, len(metas))
	sourcePages := ygo.NewMap(source, "meta").Get("pages")
	switch entries := sourcePages.(type) {
	case *ygo.Array:
		for i := uint64(0); i < entries.Len(); i++ {
			page, ok := entries.Get(i).(*ygo.Map)
			if !ok {
				continue
			}
			if id, copyPage := copyPublicTypedPage(txn, pages, page, publicIDs); copyPage {
				copied[id] = struct{}{}
			}
		}
	case []any:
		for _, entry := range entries {
			page, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			if id, copyPage := copyPublicLegacyPage(txn, pages, page, publicIDs); copyPage {
				copied[id] = struct{}{}
			}
		}
	}

	for _, meta := range metas {
		if _, exists := copied[meta.ID]; exists {
			continue
		}
		title := defaultPublicDocTitle
		if meta.Title != nil {
			title = *meta.Title
		}
		page := pages.InsertMap(txn, pages.Len())
		page.Set(txn, "id", meta.ID)
		page.Set(txn, "title", title)
		page.Set(txn, "createDate", float64(time.Now().UnixMilli()))
		page.SetArray(txn, "tags")
	}
	txn.Commit()
	return ygo.EncodeStateAsUpdate(output), nil
}

func copyPublicTypedPage(txn *ygo.TransactionMut, pages *ygo.Array, source *ygo.Map, publicIDs map[string]struct{}) (string, bool) {
	id, ok := source.Get("id").(string)
	if !ok {
		return "", false
	}
	if _, isPublic := publicIDs[id]; !isPublic {
		return "", false
	}
	page := pages.InsertMap(txn, pages.Len())
	for _, key := range simplePublicPageMetaKeys {
		if source.Has(key) {
			copyPublicMetaValue(txn, page, key, source.Get(key))
		}
	}
	copyPublicTags(txn, page, source.Get("tags"))
	return id, true
}

func copyPublicLegacyPage(txn *ygo.TransactionMut, pages *ygo.Array, source map[string]any, publicIDs map[string]struct{}) (string, bool) {
	id, ok := source["id"].(string)
	if !ok {
		return "", false
	}
	if _, isPublic := publicIDs[id]; !isPublic {
		return "", false
	}
	page := pages.InsertMap(txn, pages.Len())
	for _, key := range simplePublicPageMetaKeys {
		if value, exists := source[key]; exists {
			copyPublicMetaValue(txn, page, key, value)
		}
	}
	copyPublicTags(txn, page, source["tags"])
	return id, true
}

func copyPublicTags(txn *ygo.TransactionMut, destination *ygo.Map, value any) {
	switch values := value.(type) {
	case *ygo.Array:
		copyPublicArray(txn, destination.SetArray(txn, "tags"), values)
	case []any:
		array := destination.SetArray(txn, "tags")
		for _, item := range values {
			if isPublicScalar(item) {
				array.Push(txn, item)
			}
		}
	}
}

func copyPublicMetaValue(txn *ygo.TransactionMut, destination *ygo.Map, key string, value any) {
	switch values := value.(type) {
	case *ygo.Array:
		copyPublicArray(txn, destination.SetArray(txn, key), values)
	case []any:
		array := destination.SetArray(txn, key)
		for _, item := range values {
			if isPublicScalar(item) {
				array.Push(txn, item)
			}
		}
	default:
		if isPublicScalar(value) {
			destination.Set(txn, key, value)
		}
	}
}

func copyPublicArray(txn *ygo.TransactionMut, destination *ygo.Array, source *ygo.Array) {
	for i := uint64(0); i < source.Len(); i++ {
		value := source.Get(i)
		if isPublicScalar(value) {
			destination.Push(txn, value)
		}
	}
}

func isPublicScalar(value any) bool {
	switch value.(type) {
	case *ygo.Map, *ygo.Array, *ygo.Text, *ygo.XmlFragment, *ygo.XmlElement, *ygo.XmlText,
		map[string]any, []any:
		return false
	default:
		return true
	}
}
