// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package documents

import (
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/Deln0r/ygo"
)

type PageMetadata struct {
	Title   string
	Summary string
}

type WorkspaceMetadata struct {
	Name      string
	AvatarKey string
}

var ErrMetadataProjection = errors.New("invalid Blocksuite document metadata")

func ProjectPageMetadata(updates [][]byte, maxSummaryLength int) (PageMetadata, bool, error) {
	doc, err := restoreMetadataDocument(updates)
	if err != nil {
		return PageMetadata{}, false, err
	}
	blocks := ygo.NewMap(doc, "blocks")
	if blocks.Len() == 0 {
		return PageMetadata{}, false, nil
	}

	blockByID := make(map[string]*ygo.Map, blocks.Len())
	blocks.Range(func(_ string, value any) bool {
		block, ok := value.(*ygo.Map)
		if !ok {
			return true
		}
		id, ok := metadataString(block.Get("sys:id"))
		if ok && id != "" {
			blockByID[id] = block
		}
		return true
	})

	rootID := ""
	for id, block := range blockByID {
		flavour, _ := metadataString(block.Get("sys:flavour"))
		if flavour == "affine:page" {
			rootID = id
			break
		}
	}
	if rootID == "" {
		return PageMetadata{}, false, nil
	}

	result := PageMetadata{}
	result.Title, _ = metadataString(blockByID[rootID].Get("prop:title"))
	remaining := maxSummaryLength
	unlimited := maxSummaryLength < 0
	stack := []string{rootID}
	visited := map[string]struct{}{rootID: {}}
	for len(stack) > 0 {
		last := len(stack) - 1
		blockID := stack[last]
		stack = stack[:last]
		block := blockByID[blockID]
		if block == nil {
			continue
		}
		flavour, ok := metadataString(block.Get("sys:flavour"))
		if !ok {
			continue
		}
		switch flavour {
		case "affine:page", "affine:note", "affine:paragraph", "affine:list", "affine:code":
			pushMetadataChildren(block, blockByID, visited, &stack)
		}
		if flavour != "affine:paragraph" && flavour != "affine:list" && flavour != "affine:code" {
			continue
		}
		if !unlimited && remaining <= 0 {
			continue
		}
		text, length, ok := metadataText(block.Get("prop:text"))
		if !ok {
			continue
		}
		result.Summary += text
		if !unlimited {
			remaining -= length
		}
	}
	return result, true, nil
}

func ProjectWorkspaceMetadata(updates [][]byte) (WorkspaceMetadata, bool, error) {
	doc := ygo.NewDoc()
	meta := ygo.NewMap(doc, "meta")
	rootChanged := false
	unobserve := meta.Observe(func(*ygo.MapEvent) { rootChanged = true })
	err := applyMetadataUpdates(doc, updates)
	unobserve()
	if err != nil {
		return WorkspaceMetadata{}, false, err
	}
	if !rootChanged && meta.Len() == 0 {
		return WorkspaceMetadata{}, false, nil
	}
	return WorkspaceMetadata{
		Name:      metadataStringOrEmpty(meta.Get("name")),
		AvatarKey: metadataStringOrEmpty(meta.Get("avatar")),
	}, true, nil
}

func restoreMetadataDocument(updates [][]byte) (*ygo.Doc, error) {
	if len(updates) == 0 {
		return nil, fmt.Errorf("%w: no Yjs updates", ErrMetadataProjection)
	}
	doc := ygo.NewDoc()
	if err := applyMetadataUpdates(doc, updates); err != nil {
		return nil, err
	}
	return doc, nil
}

func applyMetadataUpdates(doc *ygo.Doc, updates [][]byte) error {
	if len(updates) == 0 {
		return fmt.Errorf("%w: no Yjs updates", ErrMetadataProjection)
	}
	for i, update := range updates {
		if err := ygo.ValidateUpdate(update); err != nil {
			return fmt.Errorf("%w: update %d is invalid", ErrMetadataProjection, i)
		}
		if err := ygo.ApplyUpdate(doc, update); err != nil {
			return fmt.Errorf("%w: update %d cannot be applied", ErrMetadataProjection, i)
		}
	}
	if ygo.HasPending(doc) {
		return fmt.Errorf("%w: document has pending updates", ErrMetadataProjection)
	}
	return nil
}

func pushMetadataChildren(block *ygo.Map, blockByID map[string]*ygo.Map, visited map[string]struct{}, stack *[]string) {
	children, ok := block.Get("sys:children").(*ygo.Array)
	if !ok {
		return
	}
	ids := make([]string, 0, children.Len())
	children.Range(func(_ uint64, value any) bool {
		if id, ok := metadataString(value); ok {
			if _, exists := blockByID[id]; exists {
				if _, seen := visited[id]; !seen {
					visited[id] = struct{}{}
					ids = append(ids, id)
				}
			}
		}
		return true
	})
	for i := len(ids) - 1; i >= 0; i-- {
		*stack = append(*stack, ids[i])
	}
}

func metadataText(value any) (string, int, bool) {
	switch text := value.(type) {
	case *ygo.Text:
		return text.String(), int(text.Length()), true
	case string:
		return text, utf8.RuneCountInString(text), true
	default:
		return "", 0, false
	}
}

func metadataStringOrEmpty(value any) string {
	result, _ := metadataString(value)
	return result
}

func metadataString(value any) (string, bool) {
	switch value := value.(type) {
	case string:
		return value, true
	case *ygo.Text:
		return value.String(), true
	default:
		return "", false
	}
}
