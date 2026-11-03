// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package documents

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Deln0r/ygo"
)

var ErrSearchProjection = errors.New("invalid Blocksuite document search projection")

type SearchUnit struct {
	BlockID         string
	UnitID          string
	Flavour         string
	Text            string
	Blob            []string
	RefDocIDs       []string
	References      []string
	ParentFlavour   string
	ParentBlockID   string
	Additional      string
	MarkdownPreview string
	PreviewIndent   int
}

type SearchProjection struct {
	Title   string
	Summary string
	Units   []SearchUnit
}

// ProjectSearchContent returns page text in document order. ACL and document
// metadata are intentionally loaded separately from canonical database rows.
func ProjectSearchContent(updates [][]byte) (SearchProjection, bool, error) {
	return ProjectSearchContentInWorkspace(updates, "", "")
}

func ProjectSearchContentInWorkspace(updates [][]byte, workspaceID, docID string) (SearchProjection, bool, error) {
	if len(updates) == 0 {
		return SearchProjection{}, false, fmt.Errorf("%w: no Yjs updates", ErrSearchProjection)
	}
	doc := ygo.NewDoc()
	for i, update := range updates {
		if err := ygo.ValidateUpdate(update); err != nil {
			return SearchProjection{}, false, fmt.Errorf("%w: update %d is invalid", ErrSearchProjection, i)
		}
		if err := ygo.ApplyUpdate(doc, update); err != nil {
			return SearchProjection{}, false, fmt.Errorf("%w: update %d cannot be applied", ErrSearchProjection, i)
		}
	}
	if ygo.HasPending(doc) {
		return SearchProjection{}, false, fmt.Errorf("%w: document has pending updates", ErrSearchProjection)
	}

	blocks := ygo.NewMap(doc, "blocks")
	if blocks.Len() == 0 {
		return SearchProjection{}, false, nil
	}
	byID := make(map[string]*ygo.Map, blocks.Len())
	blocks.Range(func(_ string, value any) bool {
		block, ok := value.(*ygo.Map)
		if !ok {
			return true
		}
		id, ok := metadataString(block.Get("sys:id"))
		if ok && id != "" {
			byID[id] = block
		}
		return true
	})

	rootID := ""
	for id, block := range byID {
		if flavour, _ := metadataString(block.Get("sys:flavour")); flavour == "affine:page" {
			rootID = id
			break
		}
	}
	if rootID == "" {
		return SearchProjection{}, false, fmt.Errorf("%w: page root block not found", ErrSearchProjection)
	}

	result := SearchProjection{Title: metadataStringOrEmpty(byID[rootID].Get("prop:title")), Units: []SearchUnit{}}
	summary := ""
	summaryRemaining := 1000
	parentByID := make(map[string]string, len(byID))
	for parentID, block := range byID {
		children, ok := block.Get("sys:children").(*ygo.Array)
		if !ok {
			continue
		}
		children.Range(func(_ uint64, value any) bool {
			if childID, ok := metadataString(value); ok {
				if _, found := byID[childID]; found {
					parentByID[childID] = parentID
				}
			}
			return true
		})
	}
	visited := map[string]struct{}{rootID: {}}
	stack := []string{rootID}
	for len(stack) > 0 {
		last := len(stack) - 1
		id := stack[last]
		stack = stack[:last]
		block := byID[id]
		if block == nil {
			continue
		}
		flavour := metadataStringOrEmpty(block.Get("sys:flavour"))
		pushMetadataChildren(block, byID, visited, &stack)
		parentID := parentByID[id]
		parentFlavour := ""
		if parent := byID[parentID]; parent != nil {
			parentFlavour = metadataStringOrEmpty(parent.Get("sys:flavour"))
		}
		unit, include := projectSearchUnit(id, flavour, parentID, parentFlavour, block)
		if !include {
			continue
		}
		if id != rootID && (flavour == "affine:paragraph" || flavour == "affine:list" || flavour == "affine:code") && summaryRemaining > 0 {
			content, length, ok := metadataText(block.Get("prop:text"))
			if ok {
				summary += content
				summaryRemaining -= length
			}
		}
		result.Units = append(result.Units, unit)
	}
	fillSearchPreviews(result.Units, byID, parentByID, workspaceID, docID)
	result.Summary = summary
	return result, true, nil
}

func projectSearchUnit(id, flavour, parentID, parentFlavour string, block *ygo.Map) (SearchUnit, bool) {
	unit := SearchUnit{BlockID: id, UnitID: id, Flavour: flavour, ParentFlavour: parentFlavour, ParentBlockID: parentID}
	switch flavour {
	case "affine:page":
		unit.Text = metadataStringOrEmpty(block.Get("prop:title"))
	case "affine:paragraph", "affine:list", "affine:code":
		text, _, ok := metadataText(block.Get("prop:text"))
		if !ok {
			return SearchUnit{}, false
		}
		unit.Text = text
		unit.References, unit.RefDocIDs = collectTextReferences(block.Get("prop:text"))
	case "affine:embed-linked-doc", "affine:embed-synced-doc":
		pageID := metadataStringOrEmpty(block.Get("prop:pageId"))
		if pageID == "" {
			return SearchUnit{}, false
		}
		unit.RefDocIDs = []string{pageID}
		unit.References = []string{serializeReference(pageID, block.Get("prop:params"))}
	case "affine:attachment":
		blobID := metadataStringOrEmpty(block.Get("prop:sourceId"))
		if blobID == "" {
			return SearchUnit{}, false
		}
		unit.Blob = []string{blobID}
		unit.Text = metadataStringOrEmpty(block.Get("prop:name"))
	case "affine:image":
		blobID := metadataStringOrEmpty(block.Get("prop:sourceId"))
		if blobID == "" {
			return SearchUnit{}, false
		}
		unit.Blob = []string{blobID}
		unit.Text = metadataStringOrEmpty(block.Get("prop:caption"))
	case "affine:latex":
		unit.Text = metadataStringOrEmpty(block.Get("prop:latex"))
	case "affine:surface":
		unit.Text = surfaceText(block.Get("prop:elements"))
	case "affine:table":
		unit.Text = tableText(block)
	case "affine:database":
		unit.Text = databaseText(block)
		unit.References, unit.RefDocIDs = databaseReferences(block)
	case "affine:bookmark", "affine:embed-youtube", "affine:embed-figma", "affine:embed-github", "affine:embed-loom":
	default:
		return SearchUnit{}, false
	}
	return unit, true
}

func collectTextReferences(value any) ([]string, []string) {
	text, ok := value.(*ygo.Text)
	if !ok {
		return nil, nil
	}
	refs := make([]string, 0)
	ids := make([]string, 0)
	seen := make(map[string]struct{})
	for _, op := range text.ToDelta() {
		raw, ok := op.Attributes["reference"].(map[string]any)
		if !ok {
			continue
		}
		pageID, _ := raw["pageId"].(string)
		if pageID == "" {
			continue
		}
		encoded := serializeReference(pageID, raw["params"])
		if _, ok := seen[encoded]; ok {
			continue
		}
		seen[encoded] = struct{}{}
		ids = append(ids, pageID)
		refs = append(refs, encoded)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	return refs, ids
}

func serializeReference(docID string, params any) string {
	value := map[string]any{"docId": docID}
	if extra := metadataObject(params); extra != nil {
		for key, item := range extra {
			value[key] = item
		}
	}
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func metadataObject(value any) map[string]any {
	switch object := value.(type) {
	case map[string]any:
		return object
	case *ygo.Map:
		result := make(map[string]any, object.Len())
		object.Range(func(key string, item any) bool {
			result[key] = metadataJSONValue(item)
			return true
		})
		return result
	default:
		return nil
	}
}

func metadataJSONValue(value any) any {
	switch item := value.(type) {
	case *ygo.Text:
		return item.String()
	case *ygo.Map:
		return metadataObject(item)
	case *ygo.Array:
		values := make([]any, 0, item.Len())
		item.Range(func(_ uint64, element any) bool {
			values = append(values, metadataJSONValue(element))
			return true
		})
		return values
	default:
		return value
	}
}

func surfaceText(value any) string {
	container, ok := value.(*ygo.Map)
	if !ok {
		return ""
	}
	elements, ok := container.Get("value").(*ygo.Map)
	if !ok {
		return ""
	}
	var text []string
	elements.Range(func(_ string, value any) bool {
		if element, ok := value.(*ygo.Map); ok {
			if content, _, ok := metadataText(element.Get("text")); ok && content != "" {
				text = append(text, content)
			}
		}
		return true
	})
	return strings.Join(text, "\n")
}

func tableText(block *ygo.Map) string {
	var text []string
	block.Range(func(key string, value any) bool {
		if strings.HasPrefix(key, "prop:cells.") && strings.HasSuffix(key, ".text") {
			if content, _, ok := metadataText(value); ok && content != "" {
				text = append(text, content)
			}
		}
		return true
	})
	return strings.Join(text, "\n")
}

func databaseText(block *ygo.Map) string {
	var text []string
	if title, _, ok := metadataText(block.Get("prop:title")); ok && title != "" {
		text = append(text, title)
	}
	if columns, ok := block.Get("prop:columns").(*ygo.Array); ok {
		columns.Range(func(_ uint64, value any) bool {
			column, ok := value.(*ygo.Map)
			if !ok {
				return true
			}
			if name := metadataStringOrEmpty(column.Get("name")); name != "" {
				text = append(text, name)
			}
			if data, ok := column.Get("data").(*ygo.Map); ok {
				if options, ok := data.Get("options").(*ygo.Array); ok {
					options.Range(func(_ uint64, value any) bool {
						if option, ok := value.(*ygo.Map); ok {
							if name := metadataStringOrEmpty(option.Get("value")); name != "" {
								text = append(text, name)
							}
						}
						return true
					})
				}
			}
			return true
		})
	}
	return strings.Join(text, "\n")
}
