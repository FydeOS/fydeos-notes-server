// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package yjs

import (
	"errors"
	"fmt"

	"github.com/Deln0r/ygo"
)

var ErrEmptyUpdate = errors.New("Yjs update is empty")

type Document struct {
	doc *ygo.Doc
}

func NewDocument() *Document {
	return &Document{doc: ygo.NewDoc()}
}

func Restore(snapshot []byte, updates [][]byte) (*Document, error) {
	doc := NewDocument()
	if len(snapshot) > 0 {
		if err := doc.ApplyUpdate(snapshot); err != nil {
			return nil, fmt.Errorf("apply document snapshot: %w", err)
		}
	}
	for i, update := range updates {
		if err := doc.ApplyUpdate(update); err != nil {
			return nil, fmt.Errorf("apply document update %d: %w", i, err)
		}
	}
	return doc, nil
}

func ValidateUpdate(update []byte) error {
	if len(update) == 0 {
		return ErrEmptyUpdate
	}
	return ygo.ValidateUpdate(update)
}

func (d *Document) ApplyUpdate(update []byte) error {
	if d == nil || d.doc == nil {
		return errors.New("Yjs document is not initialized")
	}
	if err := ValidateUpdate(update); err != nil {
		return err
	}
	return ygo.ApplyUpdate(d.doc, update)
}

func (d *Document) EncodeState() []byte {
	if d == nil || d.doc == nil {
		return nil
	}
	return ygo.EncodeStateAsUpdate(d.doc)
}

func (d *Document) StateVector() []byte {
	if d == nil || d.doc == nil {
		return nil
	}
	return ygo.EncodeStateVector(d.doc)
}

func (d *Document) Diff(stateVector []byte) ([]byte, error) {
	if d == nil || d.doc == nil {
		return nil, errors.New("Yjs document is not initialized")
	}
	return ygo.EncodeDiff(d.doc, stateVector)
}

func (d *Document) HasPendingUpdates() bool {
	return d != nil && d.doc != nil && ygo.HasPending(d.doc)
}

func MergeUpdates(updates [][]byte) ([]byte, error) {
	if len(updates) == 0 {
		return nil, nil
	}
	for i, update := range updates {
		if err := ValidateUpdate(update); err != nil {
			return nil, fmt.Errorf("validate document update %d: %w", i, err)
		}
	}
	return ygo.MergeUpdates(updates)
}
