// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package documents

import (
	"bytes"
	"errors"
	"fmt"
	"time"

	"github.com/Deln0r/ygo"
)

var ErrWorkspaceRootInvalid = errors.New("workspace root document is incomplete or invalid")

func validateWorkspaceRootAppend(snapshot []byte, updates [][]byte, incoming []byte) ([]byte, error) {
	root, oldAll, oldActive, err := loadWorkspaceRoot(snapshot, updates)
	if err != nil {
		return nil, err
	}
	if len(incoming) == 0 || len(incoming) > MaxUpdateBytes || ygo.ValidateUpdate(incoming) != nil {
		return nil, ErrInvalidDoc
	}
	if err := ygo.ApplyUpdate(root, incoming); err != nil {
		return nil, fmt.Errorf("apply workspace root update: %w", ErrInvalidDoc)
	}
	if ygo.HasPending(root) {
		return nil, ErrWorkspaceRootInvalid
	}
	newAll, newActive, err := projectWorkspaceRoot(root)
	if err != nil {
		return nil, err
	}
	for docID := range oldAll {
		if _, exists := newAll[docID]; !exists {
			return nil, ErrWorkspaceRootCommandRequired
		}
	}
	for docID := range oldActive {
		if _, exists := newActive[docID]; !exists {
			return nil, ErrWorkspaceRootCommandRequired
		}
	}
	for docID := range newActive {
		if _, wasKnown := oldAll[docID]; wasKnown {
			if _, wasActive := oldActive[docID]; !wasActive {
				return nil, ErrWorkspaceRootCommandRequired
			}
		}
	}
	return ygo.EncodeStateAsUpdate(root), nil
}

func mutateWorkspaceRootLifecycle(snapshot []byte, updates [][]byte, docID string, lifecycle WorkspaceDocLifecycle) ([]byte, error) {
	root, all, _, err := loadWorkspaceRoot(snapshot, updates)
	if err != nil {
		return nil, err
	}
	if _, exists := all[docID]; !exists {
		return nil, ErrWorkspaceDocNotFound
	}
	stateVector := ygo.EncodeStateVector(root)
	meta := ygo.NewMap(root, "meta")
	pages, ok := meta.Get("pages").(*ygo.Array)
	if !ok {
		return nil, ErrWorkspaceRootInvalid
	}
	var page *ygo.Map
	var pageIndex uint64
	for index := uint64(0); index < pages.Len(); index++ {
		candidate, ok := pages.Get(index).(*ygo.Map)
		if !ok {
			return nil, ErrWorkspaceRootInvalid
		}
		if candidate.Get("id") == docID {
			page = candidate
			pageIndex = index
			break
		}
	}
	if page == nil {
		return nil, ErrWorkspaceDocNotFound
	}
	txn := root.WriteTxn()
	switch lifecycle {
	case WorkspaceDocTrash:
		page.Set(txn, "trash", true)
		page.Set(txn, "trashDate", time.Now().UTC().UnixMilli())
	case WorkspaceDocRestore:
		page.Set(txn, "trash", false)
		page.Delete(txn, "trashDate")
	case WorkspaceDocDelete:
		pages.Delete(txn, pageIndex, 1)
	default:
		txn.Commit()
		return nil, ErrInvalidDoc
	}
	txn.Commit()
	if ygo.HasPending(root) {
		return nil, ErrWorkspaceRootInvalid
	}
	update, err := ygo.EncodeDiff(root, stateVector)
	if err != nil {
		return nil, fmt.Errorf("encode workspace root lifecycle update: %w", err)
	}
	if len(update) == 0 || len(update) > MaxUpdateBytes || ygo.ValidateUpdate(update) != nil {
		return nil, ErrWorkspaceRootInvalid
	}
	return update, nil
}

func loadWorkspaceRoot(snapshot []byte, updates [][]byte) (*ygo.Doc, map[string]struct{}, map[string]struct{}, error) {
	root := ygo.NewDoc()
	if len(snapshot) > 0 {
		if err := ygo.ValidateUpdate(snapshot); err != nil {
			return nil, nil, nil, fmt.Errorf("%w: invalid snapshot", ErrWorkspaceRootInvalid)
		}
		if err := ygo.ApplyUpdate(root, snapshot); err != nil {
			return nil, nil, nil, fmt.Errorf("%w: cannot load snapshot", ErrWorkspaceRootInvalid)
		}
	}
	for _, update := range updates {
		if err := ygo.ValidateUpdate(update); err != nil {
			return nil, nil, nil, fmt.Errorf("%w: invalid stored update", ErrWorkspaceRootInvalid)
		}
		if err := ygo.ApplyUpdate(root, update); err != nil {
			return nil, nil, nil, fmt.Errorf("%w: cannot load stored update", ErrWorkspaceRootInvalid)
		}
	}
	if ygo.HasPending(root) {
		return nil, nil, nil, ErrWorkspaceRootInvalid
	}
	all, active, err := projectWorkspaceRoot(root)
	if err != nil {
		return nil, nil, nil, err
	}
	return root, all, active, nil
}

func projectWorkspaceRoot(root *ygo.Doc) (map[string]struct{}, map[string]struct{}, error) {
	all := make(map[string]struct{})
	active := make(map[string]struct{})
	if bytes.Equal(ygo.EncodeStateAsUpdate(root), []byte{0, 0}) {
		return all, active, nil
	}
	meta := ygo.NewMap(root, "meta")
	pages, ok := meta.Get("pages").(*ygo.Array)
	if !ok {
		return nil, nil, ErrWorkspaceRootInvalid
	}
	for index := uint64(0); index < pages.Len(); index++ {
		page, ok := pages.Get(index).(*ygo.Map)
		if !ok {
			return nil, nil, ErrWorkspaceRootInvalid
		}
		docID, ok := page.Get("id").(string)
		if !ok || docID == "" {
			return nil, nil, ErrWorkspaceRootInvalid
		}
		all[docID] = struct{}{}
		trashValue := page.Get("trash")
		trashed, exists := trashValue.(bool)
		if trashValue != nil && !exists {
			return nil, nil, ErrWorkspaceRootInvalid
		}
		if !trashed {
			active[docID] = struct{}{}
		}
	}
	return all, active, nil
}
