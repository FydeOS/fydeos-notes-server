// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package documents

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/Deln0r/ygo"
)

const CanvasProjectionVersion = 1

var ErrCanvasProjection = errors.New("invalid Blocksuite canvas projection")

type CanvasBounds struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

type CanvasWarning struct {
	Code    string `json:"code"`
	Locator string `json:"locator"`
}

type CanvasBlock struct {
	ID         string        `json:"id"`
	Type       string        `json:"type"`
	Visibility string        `json:"visibility"`
	Bounds     *CanvasBounds `json:"bounds,omitempty"`
	Text       string        `json:"text,omitempty"`
	Title      string        `json:"title,omitempty"`
	ChildIDs   []string      `json:"childIds,omitempty"`
}

type CanvasElement struct {
	ID         string        `json:"id"`
	Type       string        `json:"type"`
	Bounds     *CanvasBounds `json:"bounds,omitempty"`
	Text       string        `json:"text,omitempty"`
	Title      string        `json:"title,omitempty"`
	FrameID    string        `json:"frameId,omitempty"`
	ChildIDs   []string      `json:"childIds,omitempty"`
	SourceID   string        `json:"sourceId,omitempty"`
	TargetID   string        `json:"targetId,omitempty"`
	ParentID   string        `json:"parentId,omitempty"`
	Index      string        `json:"index,omitempty"`
	PointCount *uint32       `json:"pointCount,omitempty"`
	Color      string        `json:"color,omitempty"`
	LineWidth  *float64      `json:"lineWidth,omitempty"`
}

type CanvasProjection struct {
	Version        int             `json:"version"`
	DocID          string          `json:"docId"`
	Revision       string          `json:"revision"`
	Title          string          `json:"title"`
	SurfaceBlockID string          `json:"surfaceBlockId,omitempty"`
	Bounds         *CanvasBounds   `json:"bounds,omitempty"`
	Counts         map[string]int  `json:"counts"`
	Blocks         []CanvasBlock   `json:"blocks"`
	Elements       []CanvasElement `json:"elements"`
	Warnings       []CanvasWarning `json:"warnings"`
}

type canvasBlockData struct {
	id      string
	flavour string
	value   *ygo.Map
}

func ProjectCanvas(updates [][]byte, docID, revision string) (CanvasProjection, bool, error) {
	if len(updates) == 0 || docID == "" {
		return CanvasProjection{}, false, fmt.Errorf("%w: missing document updates or ID", ErrCanvasProjection)
	}
	doc := ygo.NewDoc()
	for i, update := range updates {
		if err := ygo.ValidateUpdate(update); err != nil {
			return CanvasProjection{}, false, fmt.Errorf("%w: update %d is invalid", ErrCanvasProjection, i)
		}
		if err := ygo.ApplyUpdate(doc, update); err != nil {
			return CanvasProjection{}, false, fmt.Errorf("%w: update %d cannot be applied", ErrCanvasProjection, i)
		}
	}
	if ygo.HasPending(doc) {
		return CanvasProjection{}, false, fmt.Errorf("%w: document has pending updates", ErrCanvasProjection)
	}
	blocksMap := ygo.NewMap(doc, "blocks")
	if blocksMap.Len() == 0 {
		return CanvasProjection{}, false, nil
	}
	blocks := make(map[string]*canvasBlockData, blocksMap.Len())
	blocksMap.Range(func(_ string, value any) bool {
		block, ok := value.(*ygo.Map)
		if !ok {
			return true
		}
		id, ok := metadataString(block.Get("sys:id"))
		if !ok || id == "" {
			return true
		}
		blocks[id] = &canvasBlockData{id: id, flavour: metadataStringOrEmpty(block.Get("sys:flavour")), value: block}
		return true
	})
	if len(blocks) == 0 {
		return CanvasProjection{}, false, fmt.Errorf("%w: document blocks are missing IDs", ErrCanvasProjection)
	}

	parents := make(map[string]string, len(blocks))
	for id, block := range blocks {
		if children, ok := block.value.Get("sys:children").(*ygo.Array); ok {
			children.Range(func(_ uint64, child any) bool {
				if childID, ok := metadataString(child); ok {
					if _, exists := blocks[childID]; exists {
						parents[childID] = id
					}
				}
				return true
			})
		}
	}
	ids := sortedKeys(blocks)
	projection := CanvasProjection{
		Version: CanvasProjectionVersion, DocID: docID, Revision: revision,
		Counts: map[string]int{}, Blocks: []CanvasBlock{}, Elements: []CanvasElement{}, Warnings: []CanvasWarning{},
	}
	frames := make([]string, 0)
	frameMembers := make(map[string]string)
	for _, id := range ids {
		block := blocks[id]
		if block.flavour == "affine:page" {
			projection.Title = metadataStringOrEmpty(block.value.Get("prop:title"))
		}
		if block.flavour == "affine:frame" {
			frames = append(frames, id)
		}
	}
	sort.Strings(frames)
	for _, frameID := range frames {
		for _, childID := range canvasMapKeys(blocks[frameID].value, "prop:childElementIds") {
			if _, exists := frameMembers[childID]; !exists {
				frameMembers[childID] = frameID
			}
		}
	}

	for _, id := range ids {
		block := blocks[id]
		if block.flavour == "affine:page" {
			continue
		}
		if block.flavour == "affine:surface" {
			projection.SurfaceBlockID = id
			projection.Elements = append(projection.Elements, projectCanvasElements(block.value, frameMembers, &projection.Warnings)...)
			continue
		}
		bounds := parseCanvasBounds(block.value.Get("prop:xywh"), id, &projection.Warnings)
		text := canvasBlockText(block, blocks)
		if !strings.Contains("|affine:note|affine:frame|affine:edgeless-text|", "|"+block.flavour+"|") && text == "" {
			continue
		}
		typeName := strings.TrimPrefix(block.flavour, "affine:")
		switch block.flavour {
		case "affine:note":
			typeName = "note"
		case "affine:frame":
			typeName = "frame"
		case "affine:edgeless-text":
			typeName = "edgeless-text"
		}
		childIDs := []string{}
		if block.flavour == "affine:frame" {
			childIDs = canvasMapKeys(block.value, "prop:childElementIds")
		} else if block.flavour == "affine:note" || block.flavour == "affine:edgeless-text" {
			childIDs = canvasChildIDs(block.value)
		}
		projection.Blocks = append(projection.Blocks, CanvasBlock{
			ID: id, Type: typeName, Visibility: canvasVisibility(id, parents, blocks), Bounds: bounds,
			Text: emptyToNilString(text), Title: emptyToNilString(trimmedMetadataString(block.value.Get("prop:title"))), ChildIDs: childIDs,
		})
	}
	sort.Slice(projection.Elements, func(i, j int) bool { return projection.Elements[i].ID < projection.Elements[j].ID })
	sort.Slice(projection.Warnings, func(i, j int) bool {
		if projection.Warnings[i].Locator != projection.Warnings[j].Locator {
			return projection.Warnings[i].Locator < projection.Warnings[j].Locator
		}
		return projection.Warnings[i].Code < projection.Warnings[j].Code
	})
	for _, element := range projection.Elements {
		projection.Counts[knownCanvasElementType(element.Type)]++
	}
	projection.Bounds = aggregateCanvasBounds(projection.Blocks, projection.Elements)
	return projection, true, nil
}

func canvasBlockText(block *canvasBlockData, blocks map[string]*canvasBlockData) string {
	for _, key := range []string{"prop:text", "prop:caption", "prop:latex", "prop:name"} {
		if value := trimmedMetadataString(block.value.Get(key)); value != "" {
			return value
		}
	}
	children := canvasChildIDs(block.value)
	var texts []string
	for _, id := range children {
		if child := blocks[id]; child != nil {
			if text := canvasBlockTextDirect(child); text != "" {
				texts = append(texts, text)
			}
		}
	}
	return strings.Join(texts, "\n")
}

func canvasBlockTextDirect(block *canvasBlockData) string {
	for _, key := range []string{"prop:text", "prop:caption", "prop:latex", "prop:name"} {
		if value := trimmedMetadataString(block.value.Get(key)); value != "" {
			return value
		}
	}
	return ""
}

func canvasVisibility(id string, parents map[string]string, blocks map[string]*canvasBlockData) string {
	seen := make(map[string]struct{})
	for current := id; current != ""; current = parents[current] {
		if _, visited := seen[current]; visited {
			return "page"
		}
		seen[current] = struct{}{}
		block := blocks[current]
		if block != nil && block.flavour == "affine:note" {
			switch metadataStringOrEmpty(block.value.Get("prop:displayMode")) {
			case "both":
				return "both"
			case "page":
				return "page"
			default:
				return "edgeless"
			}
		}
	}
	return "page"
}

func parseCanvasBounds(value any, locator string, warnings *[]CanvasWarning) *CanvasBounds {
	if value == nil {
		return nil
	}
	converted := metadataJSONValue(value)
	items, ok := converted.([]any)
	if !ok || len(items) != 4 {
		*warnings = append(*warnings, CanvasWarning{Code: "INVALID_BOUNDS", Locator: locator})
		return nil
	}
	numbers := make([]float64, 4)
	for i, item := range items {
		number, ok := canvasFloat(item)
		if !ok || math.IsNaN(number) || math.IsInf(number, 0) {
			*warnings = append(*warnings, CanvasWarning{Code: "INVALID_BOUNDS", Locator: locator})
			return nil
		}
		numbers[i] = number
	}
	if numbers[2] < 0 || numbers[3] < 0 {
		*warnings = append(*warnings, CanvasWarning{Code: "INVALID_BOUNDS", Locator: locator})
		return nil
	}
	return &CanvasBounds{X: numbers[0], Y: numbers[1], Width: numbers[2], Height: numbers[3]}
}

func projectCanvasElements(surface *ygo.Map, frameMembers map[string]string, warnings *[]CanvasWarning) []CanvasElement {
	container, ok := surface.Get("prop:elements").(*ygo.Map)
	if !ok || metadataStringOrEmpty(container.Get("type")) != "$blocksuite:internal:native$" {
		return nil
	}
	values, ok := container.Get("value").(*ygo.Map)
	if !ok {
		return nil
	}
	result := make([]CanvasElement, 0, values.Len())
	mindmap := map[string][2]string{}
	values.Range(func(entryID string, value any) bool {
		element, ok := value.(*ygo.Map)
		if !ok {
			return true
		}
		id := metadataStringOrEmpty(element.Get("id"))
		if id == "" {
			id = entryID
		}
		elementType := strings.ToLower(metadataStringOrEmpty(element.Get("type")))
		if elementType == "" {
			elementType = "unknown"
		}
		known := knownCanvasElementType(elementType)
		if known == "unknown" {
			*warnings = append(*warnings, CanvasWarning{Code: "UNKNOWN_ELEMENT_TYPE:" + elementType, Locator: id})
		}
		children := canvasMapKeys(element, "children")
		if elementType == "mindmap" {
			if childMap, ok := element.Get("children").(*ygo.Map); ok {
				for _, childID := range children {
					if detail, ok := childMap.Get(childID).(*ygo.Map); ok {
						mindmap[childID] = [2]string{metadataStringOrEmpty(detail.Get("parent")), metadataStringOrEmpty(detail.Get("index"))}
					}
				}
			}
		}
		text, hasText := metadataString(element.Get("text"))
		if !hasText {
			text = metadataStringOrEmpty(element.Get("label"))
		}
		item := CanvasElement{ID: id, Type: elementType, Bounds: parseCanvasBounds(element.Get("xywh"), id, warnings),
			Text: emptyToNilString(text), Title: emptyToNilString(metadataStringOrEmpty(element.Get("title"))),
			FrameID: emptyToNilString(frameMembers[id]), ChildIDs: children,
			SourceID: canvasConnectionID(element.Get("source")), TargetID: canvasConnectionID(element.Get("target"))}
		if known == "brush" {
			pointCount := canvasBrushPointCount(element.Get("points"))
			item.PointCount = &pointCount
			item.Color = emptyToNilString(metadataStringOrEmpty(element.Get("color")))
			if width, ok := canvasFloat(metadataJSONValue(element.Get("lineWidth"))); ok {
				item.LineWidth = &width
			}
		}
		result = append(result, item)
		return true
	})
	for i := range result {
		if values, ok := mindmap[result[i].ID]; ok {
			result[i].ParentID = emptyToNilString(values[0])
			result[i].Index = emptyToNilString(values[1])
		}
	}
	return result
}

func canvasFloat(value any) (float64, bool) {
	switch number := value.(type) {
	case float64:
		return number, true
	case float32:
		return float64(number), true
	case int:
		return float64(number), true
	case int32:
		return float64(number), true
	case int64:
		return float64(number), true
	case uint64:
		return float64(number), true
	default:
		return 0, false
	}
}

func canvasConnectionID(value any) string {
	switch connection := value.(type) {
	case *ygo.Map:
		return emptyToNilString(metadataStringOrEmpty(connection.Get("id")))
	case map[string]any:
		return emptyToNilString(metadataStringOrEmpty(connection["id"]))
	default:
		return ""
	}
}

func canvasBrushPointCount(value any) uint32 {
	points, ok := value.(*ygo.Array)
	if !ok {
		return 0
	}
	count := points.Len()
	arrayPoints := false
	points.Range(func(_ uint64, point any) bool {
		if _, ok := point.(*ygo.Array); ok {
			arrayPoints = true
			return false
		}
		return true
	})
	if arrayPoints {
		return uint32(count)
	}
	return uint32((count + 1) / 2)
}

func canvasMapKeys(value *ygo.Map, key string) []string {
	child, ok := value.Get(key).(*ygo.Map)
	if !ok {
		return []string{}
	}
	result := make([]string, 0, child.Len())
	child.Range(func(key string, _ any) bool { result = append(result, key); return true })
	sort.Strings(result)
	return result
}

func canvasChildIDs(value *ygo.Map) []string {
	children, ok := value.Get("sys:children").(*ygo.Array)
	if !ok {
		return []string{}
	}
	result := make([]string, 0, children.Len())
	children.Range(func(_ uint64, child any) bool {
		if id, ok := metadataString(child); ok {
			result = append(result, id)
		}
		return true
	})
	return result
}

func knownCanvasElementType(value string) string {
	switch value {
	case "shape", "text", "connector", "group", "brush", "mindmap":
		return value
	default:
		return "unknown"
	}
}

func aggregateCanvasBounds(blocks []CanvasBlock, elements []CanvasElement) *CanvasBounds {
	var result *CanvasBounds
	include := func(bounds *CanvasBounds) {
		if bounds == nil {
			return
		}
		if result == nil {
			copy := *bounds
			result = &copy
			return
		}
		x, y := math.Min(result.X, bounds.X), math.Min(result.Y, bounds.Y)
		right := math.Max(result.X+result.Width, bounds.X+bounds.Width)
		bottom := math.Max(result.Y+result.Height, bounds.Y+bounds.Height)
		result = &CanvasBounds{X: x, Y: y, Width: right - x, Height: bottom - y}
	}
	for _, block := range blocks {
		include(block.Bounds)
	}
	for _, element := range elements {
		include(element.Bounds)
	}
	return result
}

func sortedKeys[V any](values map[string]V) []string {
	result := make([]string, 0, len(values))
	for key := range values {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

func trimmedMetadataString(value any) string { return strings.TrimSpace(metadataStringOrEmpty(value)) }

func emptyToNilString(value string) string { return value }
