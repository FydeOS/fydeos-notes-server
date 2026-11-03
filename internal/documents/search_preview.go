// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package documents

import (
	"encoding/json"
	"net/url"
	"sort"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/Deln0r/ygo"
)

const (
	maxSearchPreviewIndent     = 64
	maxSearchPreviewBlockBytes = 16 << 10
	maxSearchPreviewBytes      = 4 << 20
)

// Preview context stays inside the reference block's nearest note. Limits match
// the reader: one preceding/four following rendered blocks, stopping above 500
// UTF-16 characters. Repeated snippets do not consume a context slot.
func fillSearchPreviews(units []SearchUnit, blocks map[string]*ygo.Map, parents map[string]string, workspaceID, docID string) {
	notes := make(map[string]string, len(units))
	listDepth := make(map[string]int, len(units))
	for id := range blocks {
		path := []string{}
		seen := map[string]bool{}
		cursor := id
		for cursor != "" && !seen[cursor] {
			if _, ok := listDepth[cursor]; ok {
				break
			}
			seen[cursor] = true
			path = append(path, cursor)
			cursor = parents[cursor]
		}
		depth := listDepth[cursor]
		if cursor != "" && metadataStringOrEmpty(blocks[cursor].Get("sys:flavour")) == "affine:list" {
			depth++
		}
		for i := len(path) - 1; i >= 0; i-- {
			current := path[i]
			listDepth[current] = depth
			if metadataStringOrEmpty(blocks[current].Get("sys:flavour")) == "affine:list" {
				depth++
			}
		}
	}
	noteMemo := make(map[string]string, len(blocks))
	for id := range blocks {
		if metadataStringOrEmpty(blocks[id].Get("sys:flavour")) == "affine:note" {
			noteMemo[id] = id
			continue
		}
		path := []string{}
		seen := map[string]bool{}
		cursor := id
		note := ""
		for cursor != "" && !seen[cursor] {
			if value, ok := noteMemo[cursor]; ok {
				note = value
				break
			}
			if metadataStringOrEmpty(blocks[cursor].Get("sys:flavour")) == "affine:note" {
				note = cursor
				break
			}
			seen[cursor] = true
			path = append(path, cursor)
			cursor = parents[cursor]
		}
		for i := len(path) - 1; i >= 0; i-- {
			noteMemo[path[i]] = note
		}
	}

	groups := map[string][]int{}
	rendered := make(map[int]string)
	render := func(i int) string {
		if value, ok := rendered[i]; ok {
			return value
		}
		value := renderSearchBlock(units[i], blocks, workspaceID, docID)
		value = truncateSearchPreview(value, maxSearchPreviewBlockBytes)
		if units[i].Flavour == "affine:list" && units[i].PreviewIndent > 0 && value != "" {
			value += "\n"
		}
		rendered[i] = value
		return value
	}
	for i := range units {
		unit := &units[i]
		noteID := noteMemo[unit.BlockID]
		unit.PreviewIndent = min(listDepth[unit.BlockID]*4, maxSearchPreviewIndent)
		notes[unit.BlockID] = noteID
		additional := map[string]any{"displayMode": "edgeless"}
		if noteID != "" {
			mode := metadataStringOrEmpty(blocks[noteID].Get("prop:displayMode"))
			if mode == "" {
				mode = "edgeless"
			}
			if mode == "both" {
				mode = "page"
			}
			additional["displayMode"] = mode
			additional["noteBlockId"] = noteID
			groups[noteID] = append(groups[noteID], i)
		}
		databaseID := ""
		if unit.Flavour == "affine:database" {
			databaseID = unit.BlockID
		} else if unit.ParentFlavour == "affine:database" && unit.Flavour == "affine:paragraph" {
			databaseID = unit.ParentBlockID
		}
		if databaseID != "" {
			additional["databaseName"] = metadataStringOrEmpty(blocks[databaseID].Get("prop:title"))
		}
		encoded, _ := json.Marshal(additional)
		unit.Additional = string(encoded)
	}
	previewBudget := maxSearchPreviewBytes
	for i := range units {
		if previewBudget <= 0 {
			break
		}
		if len(units[i].References) == 0 {
			continue
		}
		raw := render(i)
		logicalLength := utf16Length(raw) + units[i].PreviewIndent*countMarkdownLines(raw)
		group := groups[notes[units[i].BlockID]]
		index := -1
		for j, unitIndex := range group {
			if unitIndex == i {
				index = j
				break
			}
		}
		fragments := []int{i}
		previous, following := index, index
		before, after := 0, 0
		for logicalLength <= 500 && !((before >= 1 || previous < 0) && (after >= 4 || following >= len(group))) {
			if before < 1 {
				previous--
				if previous >= 0 && previous < len(group) {
					candidate := render(group[previous])
					if candidate != "" && !strings.HasPrefix(raw, candidate) {
						raw = candidate + "\n" + raw
						logicalLength += utf16Length(candidate) + units[group[previous]].PreviewIndent*countMarkdownLines(candidate) + 1
						fragments = append([]int{group[previous]}, fragments...)
						before++
					}
				}
			}
			if after < 4 {
				following++
				if following >= 0 && following < len(group) {
					candidate := render(group[following])
					if candidate != "" && !strings.HasSuffix(raw, candidate) {
						raw += "\n" + candidate
						logicalLength += utf16Length(candidate) + units[group[following]].PreviewIndent*countMarkdownLines(candidate) + 1
						fragments = append(fragments, group[following])
						after++
					}
				}
			}
		}
		var output strings.Builder
		for fragmentIndex, unitIndex := range fragments {
			text := render(unitIndex)
			for lineIndex, line := range strings.Split(text, "\n") {
				if lineIndex > 0 {
					output.WriteByte('\n')
				}
				output.WriteString(strings.Repeat(" ", units[unitIndex].PreviewIndent))
				output.WriteString(line)
			}
			if fragmentIndex < len(fragments)-1 {
				output.WriteByte('\n')
			}
		}
		preview := unindentSearchMarkdown(output.String())
		preview = truncateSearchPreview(preview, previewBudget)
		units[i].MarkdownPreview = preview
		previewBudget -= len(preview)
	}
}

func truncateSearchPreview(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	if maxBytes <= 3 {
		return value[:maxBytes]
	}
	end := maxBytes - 3
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end] + "..."
}

func renderSearchBlock(unit SearchUnit, blocks map[string]*ygo.Map, workspaceID, docID string) string {
	block := blocks[unit.BlockID]
	if block == nil {
		return ""
	}
	link := func(id string) string {
		return "/workspace/" + workspaceID + "/" + docID + "?blockIds=" + url.QueryEscape(id)
	}
	database := func(id string) string {
		title := metadataStringOrEmpty(blocks[id].Get("prop:title"))
		if title == "" {
			title = "Untitled"
		}
		return "[database · " + title + "][](" + link(id) + ")\n"
	}
	if unit.ParentFlavour == "affine:database" && (unit.Flavour == "affine:paragraph" || unit.Flavour == "affine:list" || unit.Flavour == "affine:code") {
		return database(unit.ParentBlockID)
	}
	switch unit.Flavour {
	case "affine:paragraph":
		text := searchMarkdownText(block.Get("prop:text"), workspaceID)
		kind := metadataStringOrEmpty(block.Get("prop:type"))
		if len(kind) == 2 && kind[0] == 'h' && kind[1] >= '1' && kind[1] <= '6' {
			text = strings.Repeat("#", int(kind[1]-'0')) + " " + text
		} else if kind == "quote" {
			text = "> " + strings.ReplaceAll(text, "\n", "\n> ")
		} else if kind != "text" {
			return ""
		}
		return trimSearchMarkdown(text, 3)
	case "affine:list":
		prefix := "* "
		switch metadataStringOrEmpty(block.Get("prop:type")) {
		case "numbered":
			prefix = "1. "
		case "todo":
			prefix = "* [ ] "
			if checked, _ := block.Get("prop:checked").(bool); checked {
				prefix = "* [x] "
			}
		}
		text := prefix + searchMarkdownText(block.Get("prop:text"), workspaceID) + "\n"
		return text
	case "affine:code":
		text := metadataStringOrEmpty(block.Get("prop:text"))
		fence := searchMarkdownFence(text, 3)
		language := metadataStringOrEmpty(block.Get("prop:language"))
		return trimSearchMarkdown(fence+language+"\n"+text+"\n"+fence, 4)
	case "affine:embed-linked-doc", "affine:embed-synced-doc":
		return "[](" + link(unit.BlockID) + ")\n"
	case "affine:attachment":
		return "[" + metadataStringOrEmpty(block.Get("prop:name")) + "](" + metadataStringOrEmpty(block.Get("prop:sourceId")) + ")\n"
	case "affine:image":
		text := "an image block"
		if id := metadataStringOrEmpty(block.Get("prop:sourceId")); id != "" {
			text += ", file id " + id
		}
		if caption := metadataStringOrEmpty(block.Get("prop:caption")); caption != "" {
			text += ", with caption " + caption
		}
		return text + "\n"
	case "affine:latex":
		return "LaTeX, with value " + strings.ReplaceAll(strings.TrimSpace(unit.Text), "\n", " ") + "\n"
	case "affine:bookmark", "affine:embed-youtube", "affine:embed-figma", "affine:embed-github", "affine:embed-loom":
		return "[" + metadataStringOrEmpty(block.Get("prop:title")) + "](" + metadataStringOrEmpty(block.Get("prop:url")) + ")\n"
	case "affine:table":
		return "[table][](" + link(unit.BlockID) + ")\n"
	case "affine:database":
		return database(unit.BlockID)
	default:
		return ""
	}
}

func trimSearchMarkdown(text string, keep int) string {
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	threshold := keep
	if keep == 4 {
		threshold = 5
	}
	if len(lines) > threshold {
		lines = append(append(lines[:keep:keep], "..."), lines[len(lines)-1])
	}
	if len(lines) == 0 {
		return "\n"
	}
	return strings.Join(lines, "\n") + "\n"
}

func unindentSearchMarkdown(text string) string {
	lines := strings.Split(text, "\n")
	base := -1
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " \t\r")
		indent := len(line) - len(trimmed)
		if indent > 0 {
			if base < 0 {
				base = indent
			}
			lines[i] = strings.Repeat(" ", max(0, indent-base)) + trimmed
		}
	}
	return strings.Join(lines, "\n")
}

func searchMarkdownText(value any, workspaceID string) string {
	text, ok := value.(*ygo.Text)
	if !ok {
		return escapeSearchMarkdown(metadataStringOrEmpty(value))
	}
	var out strings.Builder
	for _, op := range text.ToDelta() {
		raw := op.Insert
		attrs := op.Attributes
		if ref, ok := attrs["reference"].(map[string]any); ok {
			id, _ := ref["pageId"].(string)
			params := metadataObject(ref["params"])
			query := url.Values{}
			for key, value := range params {
				switch value := value.(type) {
				case string:
					if value != "" {
						query.Set(key, value)
					}
				case []any:
					var items []string
					for _, item := range value {
						if item, ok := item.(string); ok && item != "" {
							items = append(items, item)
						}
					}
					if len(items) > 0 {
						query.Set(key, strings.Join(items, ","))
					}
				}
			}
			target := "/workspace/" + workspaceID + "/" + id
			if len(query) > 0 {
				target += "?" + query.Encode()
			}
			out.WriteString("[](" + target + ")")
			continue
		}
		part := escapeSearchMarkdown(raw)
		if code, _ := attrs["code"].(bool); code {
			fence := searchMarkdownFence(raw, 1)
			part = fence + raw + fence
		} else {
			for _, style := range []struct{ key, marker string }{{"bold", "**"}, {"italic", "*"}, {"strike", "~~"}} {
				if enabled, _ := attrs[style.key].(bool); enabled {
					leading := len(part) - len(strings.TrimLeft(part, " "))
					trailing := len(part) - len(strings.TrimRight(part, " "))
					if leading+trailing > len(part) {
						trailing = 0
					}
					part = strings.Repeat("&#x20;", leading) + part[leading:len(part)-trailing] + strings.Repeat("&#x20;", trailing)
					part = style.marker + part + style.marker
				}
			}
		}
		if target, ok := attrs["link"].(string); ok {
			part = "[" + part + "](" + target + ")"
		}
		out.WriteString(part)
	}
	return out.String()
}

func escapeSearchMarkdown(text string) string {
	var out strings.Builder
	for _, r := range text {
		if strings.ContainsRune("\\`*_[<>]", r) {
			out.WriteByte('\\')
		}
		out.WriteRune(r)
	}
	return out.String()
}

func searchRawObject(value any) map[string]any {
	switch item := value.(type) {
	case map[string]any:
		return item
	case *ygo.Map:
		out := make(map[string]any, item.Len())
		item.Range(func(key string, value any) bool { out[key] = value; return true })
		return out
	}
	return nil
}

func databaseReferences(block *ygo.Map) ([]string, []string) {
	cells := searchRawObject(block.Get("prop:cells"))
	var refs, ids []string
	seen := map[string]bool{}
	// Stable key order avoids map iteration changing backlinks between projections.
	rowKeys := make([]string, 0, len(cells))
	for key := range cells {
		rowKeys = append(rowKeys, key)
	}
	sort.Strings(rowKeys)
	for _, rowKey := range rowKeys {
		row := searchRawObject(cells[rowKey])
		keys := make([]string, 0, len(row))
		for key := range row {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			cell := searchRawObject(row[key])
			value := cell["value"]
			if container := searchRawObject(value); container != nil {
				value = container["delta"]
			}
			var deltas []any
			switch typed := value.(type) {
			case *ygo.Text:
				for _, op := range typed.ToDelta() {
					deltas = append(deltas, map[string]any{"attributes": op.Attributes})
				}
			case []any:
				deltas = typed
			case *ygo.Array:
				typed.Range(func(_ uint64, v any) bool { deltas = append(deltas, v); return true })
			}
			for _, delta := range deltas {
				attrs := searchRawObject(searchRawObject(delta)["attributes"])
				ref := searchRawObject(attrs["reference"])
				id, _ := ref["pageId"].(string)
				if id == "" {
					continue
				}
				encoded := serializeReference(id, ref["params"])
				if !seen[encoded] {
					refs = append(refs, encoded)
					ids = append(ids, id)
					seen[encoded] = true
				}
			}
		}
	}
	return refs, ids
}

func searchMarkdownFence(text string, minimum int) string {
	longest, current := 0, 0
	for _, r := range text {
		if r == '`' {
			current++
			longest = max(longest, current)
		} else {
			current = 0
		}
	}
	return strings.Repeat("`", max(minimum, longest+1))
}

func utf16Length(text string) int { return len(utf16.Encode([]rune(text))) }
func countMarkdownLines(text string) int {
	if text == "" {
		return 0
	}
	return strings.Count(text, "\n") + 1
}
