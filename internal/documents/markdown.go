// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package documents

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/Deln0r/ygo"
)

// ErrMarkdownProjection reports updates that cannot be decoded into a
// document for Markdown projection.
var ErrMarkdownProjection = errors.New("invalid Blocksuite document for Markdown projection")

// MarkdownDocument is the page-mode Markdown rendering of one document.
type MarkdownDocument struct {
	Title    string
	Markdown string
}

// ProjectMarkdown restores a document from its Yjs updates and renders the
// page-mode content below its affine:page block as Markdown.
//
// It returns found=false without an error when there are no updates or the
// document has no page block, and an error when an update is malformed.
//
// Rendering conventions:
//   - Every block ends with a blank line, except list items, which stay
//     tight; a blank line closes a run of list items.
//   - Text is emitted verbatim (no Markdown escaping) so that the output stays
//     readable for people and language models.
//   - Blob-backed blocks have no public URL here, so they point at
//     "blob:<sourceId>": images render as ![caption](blob:id) and attachments
//     as [name](blob:id). Images and attachments without a source are
//     skipped.
//   - Linked and synced page embeds use the same "LinkedPage:<pageId>" target
//     as inline page references. Bookmarks and other URL embeds render as a
//     plain link.
//   - Edgeless-only content (affine:surface, affine:frame, affine:edgeless-*,
//     and notes whose display mode is EdgelessOnly) is skipped.
func ProjectMarkdown(updates [][]byte) (MarkdownDocument, bool, error) {
	if len(updates) == 0 {
		return MarkdownDocument{}, false, nil
	}
	doc := ygo.NewDoc()
	for i, update := range updates {
		if err := ygo.ValidateUpdate(update); err != nil {
			return MarkdownDocument{}, false, fmt.Errorf("%w: update %d is invalid", ErrMarkdownProjection, i)
		}
		if err := ygo.ApplyUpdate(doc, update); err != nil {
			return MarkdownDocument{}, false, fmt.Errorf("%w: update %d cannot be applied", ErrMarkdownProjection, i)
		}
	}
	if ygo.HasPending(doc) {
		return MarkdownDocument{}, false, fmt.Errorf("%w: document has pending updates", ErrMarkdownProjection)
	}

	blocks := make(map[string]*ygo.Map)
	ygo.NewMap(doc, "blocks").Range(func(key string, value any) bool {
		block, ok := value.(*ygo.Map)
		if !ok {
			return true
		}
		id, ok := metadataString(block.Get("sys:id"))
		if !ok || id == "" {
			id = key
		}
		blocks[id] = block
		return true
	})
	pageID := ""
	for _, id := range sortedKeys(blocks) {
		if metadataStringOrEmpty(blocks[id].Get("sys:flavour")) == "affine:page" {
			pageID = id
			break
		}
	}
	if pageID == "" {
		return MarkdownDocument{}, false, nil
	}

	r := markdownRenderer{blocks: blocks, visited: map[string]struct{}{pageID: {}}}
	r.renderChildren(blocks[pageID], 0)
	r.out.finish()
	return MarkdownDocument{
		Title:    metadataStringOrEmpty(blocks[pageID].Get("prop:title")),
		Markdown: r.out.String(),
	}, true, nil
}

type markdownRenderer struct {
	blocks  map[string]*ygo.Map
	visited map[string]struct{}
	out     markdownWriter
}

func (r *markdownRenderer) renderChildren(block *ygo.Map, indent int) {
	children, ok := block.Get("sys:children").(*ygo.Array)
	if !ok {
		return
	}
	var ids []string
	children.Range(func(_ uint64, value any) bool {
		if id, ok := value.(string); ok {
			ids = append(ids, id)
		}
		return true
	})
	for _, id := range ids {
		child, exists := r.blocks[id]
		if _, seen := r.visited[id]; !exists || seen {
			continue
		}
		r.visited[id] = struct{}{}
		r.renderBlock(child, indent)
	}
}

func (r *markdownRenderer) renderBlock(block *ygo.Map, indent int) {
	flavour := metadataStringOrEmpty(block.Get("sys:flavour"))
	kind := metadataStringOrEmpty(block.Get("prop:type"))
	switch {
	case flavour == "affine:surface", flavour == "affine:frame", strings.HasPrefix(flavour, "affine:edgeless-"):
		return
	case flavour == "affine:note" && metadataStringOrEmpty(block.Get("prop:displayMode")) == "EdgelessOnly":
		return
	case flavour == "affine:paragraph":
		text := renderInlineMarkdown(block.Get("prop:text"))
		if level := headingLevel(kind); level > 0 {
			if text = strings.TrimSpace(strings.ReplaceAll(text, "\n", " ")); text != "" {
				r.out.block(indent, strings.Repeat("#", level)+" "+text)
			}
		} else if kind == "quote" {
			r.out.block(indent, prefixLines(strings.TrimRight(text, "\n"), "> "))
		} else {
			r.out.block(indent, strings.TrimRight(text, "\n"))
		}
		r.renderChildren(block, indent)
		return
	case flavour == "affine:list":
		marker := "*"
		switch kind {
		case "numbered":
			marker = "1."
		case "todo":
			marker = "- [ ]"
			if checked, _ := block.Get("prop:checked").(bool); checked {
				marker = "- [x]"
			}
		}
		r.out.listItem(indent, marker, strings.TrimRight(renderInlineMarkdown(block.Get("prop:text")), "\n"))
		r.renderChildren(block, indent+1)
		return
	case flavour == "affine:code":
		code := strings.TrimRight(metadataStringOrEmpty(block.Get("prop:text")), "\n")
		fence := markdownFence(code, '`', 3)
		language := strings.Join(strings.Fields(metadataStringOrEmpty(block.Get("prop:language"))), "-")
		r.out.block(indent, fence+language+"\n"+code+"\n"+fence)
	case flavour == "affine:divider":
		r.out.block(indent, "---")
	case flavour == "affine:latex":
		if latex := strings.TrimSpace(metadataStringOrEmpty(block.Get("prop:latex"))); latex != "" {
			r.out.block(indent, "$$\n"+latex+"\n$$")
		}
	case flavour == "affine:image":
		if source := metadataStringOrEmpty(block.Get("prop:sourceId")); source != "" {
			caption := oneLine(metadataStringOrEmpty(block.Get("prop:caption")))
			r.out.block(indent, "!["+caption+"]("+markdownDestination("blob:"+source)+")")
		}
	case flavour == "affine:attachment":
		if source := metadataStringOrEmpty(block.Get("prop:sourceId")); source != "" {
			name := oneLine(metadataStringOrEmpty(block.Get("prop:name")))
			if name == "" {
				name = source
			}
			r.out.block(indent, "["+name+"]("+markdownDestination("blob:"+source)+")")
		}
	case flavour == "affine:embed-linked-doc", flavour == "affine:embed-synced-doc":
		if pageID := metadataStringOrEmpty(block.Get("prop:pageId")); pageID != "" {
			r.out.block(indent, linkedPageMarkdown(pageID, metadataStringOrEmpty(block.Get("prop:title"))))
		}
	case flavour == "affine:table":
		var table strings.Builder
		writeMarkdownTable(&table, tableRows(block))
		r.out.block(indent, strings.TrimRight(table.String(), "\n"))
	case flavour == "affine:database":
		if title := oneLine(metadataStringOrEmpty(block.Get("prop:title"))); title != "" {
			r.out.block(indent, "**"+title+"**")
		}
	default:
		// Bookmarks and URL embeds (affine:bookmark, affine:embed-youtube,
		// affine:embed-github, ...) carry prop:url. Everything else is
		// treated as a container.
		if url := metadataStringOrEmpty(block.Get("prop:url")); url != "" && (flavour == "affine:bookmark" || strings.HasPrefix(flavour, "affine:embed-")) {
			label := oneLine(metadataStringOrEmpty(block.Get("prop:title")))
			if label == "" {
				label = url
			}
			r.out.block(indent, "["+label+"]("+markdownDestination(url)+")")
		}
	}
	r.renderChildren(block, indent)
}

func headingLevel(kind string) int {
	if len(kind) == 2 && kind[0] == 'h' && kind[1] >= '1' && kind[1] <= '6' {
		return int(kind[1] - '0')
	}
	return 0
}

// markdownWriter assembles block-level output. Blocks are separated by a
// blank line; consecutive list items are kept tight.
type markdownWriter struct {
	strings.Builder
	inList bool
}

func (w *markdownWriter) block(indent int, body string) {
	if strings.TrimSpace(body) == "" {
		return
	}
	w.closeList()
	w.writeIndented(strings.Repeat("    ", indent), strings.Repeat("    ", indent), body)
	w.WriteString("\n")
}

func (w *markdownWriter) listItem(indent int, marker, text string) {
	prefix := strings.Repeat("    ", indent)
	line := marker
	if text != "" {
		line += " " + text
	}
	w.writeIndented(prefix, prefix+"    ", line)
	w.inList = true
}

func (w *markdownWriter) closeList() {
	if w.inList {
		w.WriteString("\n")
		w.inList = false
	}
}

func (w *markdownWriter) finish() { w.closeList() }

// writeIndented writes text line by line, prefixing the first line with
// first and every later non-empty line with rest.
func (w *markdownWriter) writeIndented(first, rest, text string) {
	for i, line := range strings.Split(text, "\n") {
		if line != "" {
			if i == 0 {
				w.WriteString(first)
			} else {
				w.WriteString(rest)
			}
			w.WriteString(line)
		}
		w.WriteString("\n")
	}
}

func prefixLines(text, prefix string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if line == "" {
			lines[i] = strings.TrimRight(prefix, " ")
		} else {
			lines[i] = prefix + line
		}
	}
	return strings.Join(lines, "\n")
}

func oneLine(text string) string { return strings.Join(strings.Fields(text), " ") }

// markdownFence returns a fence of marker characters longer than any run of
// that character in content, and at least minimum long.
func markdownFence(content string, marker rune, minimum int) string {
	longest, run := 0, 0
	for _, r := range content {
		if r == marker {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	return strings.Repeat(string(marker), max(minimum, longest+1))
}

func markdownDestination(target string) string {
	if strings.ContainsAny(target, " \t\n()<>") {
		return "<" + strings.NewReplacer("<", "%3C", ">", "%3E", "\n", "%0A").Replace(target) + ">"
	}
	return target
}

func linkedPageMarkdown(pageID, title string) string {
	if title = oneLine(title); title == "" {
		title = "Untitled"
	}
	return "[" + title + "](" + markdownDestination("LinkedPage:"+pageID) + ")"
}

// tableRows reads an affine:table block, whose flat properties are
// prop:rows.<rowId>.order, prop:columns.<columnId>.order and
// prop:cells.<rowId>:<columnId>.text, into rows of rendered cell text.
func tableRows(block *ygo.Map) [][]string {
	rowOrder := map[string]string{}
	columnOrder := map[string]string{}
	cells := map[string]string{}
	block.Range(func(key string, value any) bool {
		switch {
		case strings.HasPrefix(key, "prop:rows.") && strings.HasSuffix(key, ".order"):
			rowOrder[strings.TrimSuffix(strings.TrimPrefix(key, "prop:rows."), ".order")] = metadataStringOrEmpty(value)
		case strings.HasPrefix(key, "prop:columns.") && strings.HasSuffix(key, ".order"):
			columnOrder[strings.TrimSuffix(strings.TrimPrefix(key, "prop:columns."), ".order")] = metadataStringOrEmpty(value)
		case strings.HasPrefix(key, "prop:cells.") && strings.HasSuffix(key, ".text"):
			cells[strings.TrimSuffix(strings.TrimPrefix(key, "prop:cells."), ".text")] = renderInlineMarkdown(value)
		}
		return true
	})
	rows, columns := orderedIDs(rowOrder), orderedIDs(columnOrder)
	if len(columns) == 0 {
		return nil
	}
	table := make([][]string, 0, len(rows))
	for _, row := range rows {
		values := make([]string, len(columns))
		for i, column := range columns {
			values[i] = cells[row+":"+column]
		}
		table = append(table, values)
	}
	return table
}

func orderedIDs(order map[string]string) []string {
	ids := sortedKeys(order)
	sort.SliceStable(ids, func(i, j int) bool { return order[ids[i]] < order[ids[j]] })
	return ids
}

// writeMarkdownTable writes a pipe table whose first row is the header row,
// followed by a blank line. Line breaks inside a cell become <br />, with runs
// of blank lines collapsed, and pipes are escaped.
func writeMarkdownTable(output *strings.Builder, rows [][]string) {
	width := 0
	for _, row := range rows {
		width = max(width, len(row))
	}
	if width == 0 {
		return
	}
	writeRow := func(row []string) {
		output.WriteString("|")
		for i := range width {
			if i < len(row) {
				output.WriteString(markdownTableCell(row[i]))
			}
			output.WriteString("|")
		}
		output.WriteString("\n")
	}
	writeRow(rows[0])
	output.WriteString("|" + strings.Repeat("---|", width) + "\n")
	for _, row := range rows[1:] {
		writeRow(row)
	}
	output.WriteString("\n")
}

func markdownTableCell(text string) string {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, strings.ReplaceAll(line, "|", "\\|"))
		}
	}
	return strings.Join(lines, "<br />")
}

// Inline rendering.
//
// A Y.Text delta is flattened into pieces (single runes or page references)
// that each carry a style. Whitespace only keeps an emphasis style when the
// nearest non-whitespace pieces on both sides have it, so that markers never
// sit next to a space ("**a **b" would not parse). Equal neighbouring pieces
// are then merged into runs, and the runs are written with a stack of open
// markers: a marker stays open for as long as following runs keep the style,
// which keeps "**Open [docs](url)**" intact when only the link changes.

type inlineStyle struct {
	bold, italic, strike, code bool
	link                       string
}

type inlinePiece struct {
	text      string
	reference bool
	style     inlineStyle
}

type inlineMarkerKind int

const (
	markerLink inlineMarkerKind = iota
	markerBold
	markerItalic
	markerStrike
	markerCode
)

type inlineMarker struct {
	kind inlineMarkerKind
	link string
}

func renderInlineMarkdown(value any) string {
	text, ok := value.(*ygo.Text)
	if !ok {
		return metadataStringOrEmpty(value)
	}
	pieces := inlinePieces(text.ToDelta())
	adjustInlineWhitespace(pieces)
	return writeInlineRuns(mergeInlinePieces(pieces))
}

func inlinePieces(delta []ygo.DeltaOp) []inlinePiece {
	var pieces []inlinePiece
	for _, op := range delta {
		if op.Insert == "" {
			continue
		}
		attrs := op.Attributes
		style := inlineStyle{
			bold:   attrs["bold"] == true,
			italic: attrs["italic"] == true,
			strike: attrs["strike"] == true,
		}
		if reference := metadataObject(attrs["reference"]); reference != nil {
			if pageID, _ := reference["pageId"].(string); pageID != "" {
				title, _ := reference["title"].(string)
				token := linkedPageMarkdown(pageID, title)
				if n := len(pieces); n > 0 && pieces[n-1].reference && pieces[n-1].text == token {
					continue
				}
				pieces = append(pieces, inlinePiece{text: token, reference: true, style: style})
				continue
			}
		}
		style.code = attrs["code"] == true
		style.link, _ = attrs["link"].(string)
		for _, r := range op.Insert {
			pieces = append(pieces, inlinePiece{text: string(r), style: style})
		}
	}
	return pieces
}

func isInlineSpace(piece inlinePiece) bool {
	return !piece.reference && strings.TrimSpace(piece.text) == ""
}

func adjustInlineWhitespace(pieces []inlinePiece) {
	for i := range pieces {
		if !isInlineSpace(pieces[i]) {
			continue
		}
		var before, after inlineStyle
		for j := i - 1; j >= 0; j-- {
			if !isInlineSpace(pieces[j]) {
				before = pieces[j].style
				break
			}
		}
		for j := i + 1; j < len(pieces); j++ {
			if !isInlineSpace(pieces[j]) {
				after = pieces[j].style
				break
			}
		}
		style := &pieces[i].style
		style.bold = style.bold && before.bold && after.bold
		style.italic = style.italic && before.italic && after.italic
		style.strike = style.strike && before.strike && after.strike
	}
}

func mergeInlinePieces(pieces []inlinePiece) []inlinePiece {
	var runs []inlinePiece
	for _, piece := range pieces {
		if n := len(runs); n > 0 && !piece.reference && !runs[n-1].reference && runs[n-1].style == piece.style {
			runs[n-1].text += piece.text
			continue
		}
		runs = append(runs, piece)
	}
	return runs
}

func (s inlineStyle) markers() []inlineMarker {
	var markers []inlineMarker
	if s.link != "" {
		markers = append(markers, inlineMarker{kind: markerLink, link: s.link})
	}
	if s.bold {
		markers = append(markers, inlineMarker{kind: markerBold})
	}
	if s.italic {
		markers = append(markers, inlineMarker{kind: markerItalic})
	}
	if s.strike {
		markers = append(markers, inlineMarker{kind: markerStrike})
	}
	if s.code {
		markers = append(markers, inlineMarker{kind: markerCode})
	}
	return markers
}

func (s inlineStyle) has(marker inlineMarker) bool {
	switch marker.kind {
	case markerLink:
		return s.link != "" && s.link == marker.link
	case markerBold:
		return s.bold
	case markerItalic:
		return s.italic
	case markerStrike:
		return s.strike
	default:
		return s.code
	}
}

func writeInlineRuns(runs []inlinePiece) string {
	var out, code strings.Builder
	var stack []inlineMarker
	closeMarker := func(marker inlineMarker) {
		switch marker.kind {
		case markerLink:
			out.WriteString("](" + markdownDestination(marker.link) + ")")
		case markerBold:
			out.WriteString("**")
		case markerItalic:
			out.WriteString("_")
		case markerStrike:
			out.WriteString("~~")
		case markerCode:
			content := code.String()
			code.Reset()
			fence := markdownFence(content, '`', 1)
			if strings.HasPrefix(content, "`") || strings.HasSuffix(content, "`") {
				content = " " + content + " "
			}
			out.WriteString(fence + content + fence)
		}
	}
	openMarker := func(marker inlineMarker) {
		switch marker.kind {
		case markerLink:
			out.WriteString("[")
		case markerBold:
			out.WriteString("**")
		case markerItalic:
			out.WriteString("_")
		case markerStrike:
			out.WriteString("~~")
		}
	}
	for i, run := range runs {
		// Keep the longest prefix of the stack that this run still uses.
		keep := 0
		for keep < len(stack) && run.style.has(stack[keep]) {
			keep++
		}
		var open []inlineMarker
		collectOpen := func() {
			open = open[:0]
			for _, marker := range run.style.markers() {
				if !markerIn(marker, stack[:keep]) {
					open = append(open, marker)
				}
			}
		}
		collectOpen()
		// Code spans cannot contain markup, so code must stay innermost.
		if len(open) > 0 {
			for j := 0; j < keep; j++ {
				if stack[j].kind == markerCode {
					keep = j
					collectOpen()
					break
				}
			}
		}
		for j := len(stack) - 1; j >= keep; j-- {
			closeMarker(stack[j])
		}
		stack = stack[:keep]
		sort.SliceStable(open, func(a, b int) bool {
			if (open[a].kind == markerCode) != (open[b].kind == markerCode) {
				return open[b].kind == markerCode
			}
			return inlineSpan(runs[i:], open[a]) > inlineSpan(runs[i:], open[b])
		})
		for _, marker := range open {
			openMarker(marker)
			stack = append(stack, marker)
		}
		if n := len(stack); n > 0 && stack[n-1].kind == markerCode {
			code.WriteString(run.text)
		} else {
			out.WriteString(run.text)
		}
	}
	for j := len(stack) - 1; j >= 0; j-- {
		closeMarker(stack[j])
	}
	return out.String()
}

func markerIn(marker inlineMarker, markers []inlineMarker) bool {
	for _, candidate := range markers {
		if candidate == marker {
			return true
		}
	}
	return false
}

// inlineSpan counts how many consecutive runs keep a marker's style.
func inlineSpan(runs []inlinePiece, marker inlineMarker) int {
	count := 0
	for _, run := range runs {
		if !run.style.has(marker) {
			break
		}
		count++
	}
	return count
}
