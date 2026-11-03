// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package search

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"
	"golang.org/x/net/html"
)

const (
	maxZipEntries  = 4096
	maxCSVRows     = 200000
	maxExtractText = 50 << 20
)

// errUnsupportedFormat marks artifacts whose format cannot be read.
var errUnsupportedFormat = errors.New("unsupported artifact format")

func unsupported(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errUnsupportedFormat, fmt.Sprintf(format, args...))
}

const (
	mimeDOCX = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	mimeXLSX = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	mimePPTX = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
)

// ExtractArtifactText returns the readable text parts of an artifact.
func ExtractArtifactText(body []byte, fileName, mimeType string) ([]string, error) {
	if len(body) == 0 {
		return nil, errors.New("artifact is empty")
	}
	if len(body) > maxArtifactBytes {
		return nil, errors.New("artifact exceeds size limit")
	}
	media := strings.ToLower(strings.TrimSpace(strings.SplitN(mimeType, ";", 2)[0]))
	ext := strings.ToLower(path.Ext(fileName))
	var (
		parts []string
		err   error
	)
	switch {
	case media == "application/pdf" || ext == ".pdf":
		parts, err = extractPDF(body)
	case media == mimeDOCX || ext == ".docx":
		parts, err = extractDOCX(body)
	case media == mimeXLSX || ext == ".xlsx":
		parts, err = extractXLSX(body)
	case media == mimePPTX || ext == ".pptx":
		parts, err = extractPPTX(body)
	case media == "text/csv" || ext == ".csv":
		parts, err = extractCSV(body)
	case media == "text/html" || ext == ".html" || ext == ".htm":
		parts, err = extractHTML(body)
	case strings.HasPrefix(media, "text/") || ext == ".txt" || ext == ".md" || ext == ".markdown" || ext == ".log" || ext == ".rst":
		if !utf8.Valid(body) || bytes.IndexByte(body, 0) >= 0 {
			return nil, unsupported("text is not valid UTF-8")
		}
		parts = []string{string(body)}
	default:
		return nil, unsupported("media type %q", mimeType)
	}
	if err != nil {
		return nil, err
	}
	return cleanParts(parts)
}

func cleanParts(parts []string) ([]string, error) {
	out := make([]string, 0, len(parts))
	total := 0
	for _, part := range parts {
		part = strings.TrimSpace(strings.ToValidUTF8(part, ""))
		if part == "" {
			continue
		}
		total += len(part)
		if total > maxSourceTextBytes {
			return nil, errors.New("artifact text is too large")
		}
		out = append(out, part)
	}
	if len(out) == 0 {
		return nil, errors.New("artifact has no text")
	}
	return out, nil
}

func extractPDF(body []byte) (parts []string, err error) {
	defer func() {
		if r := recover(); r != nil {
			parts, err = nil, unsupported("malformed PDF")
		}
	}()
	reader, err := pdf.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return nil, unsupported("invalid PDF: %v", err)
	}
	total := 0
	for i := 1; i <= reader.NumPage(); i++ {
		page := reader.Page(i)
		if page.V.IsNull() {
			continue
		}
		text, err := page.GetPlainText(nil)
		if err != nil {
			return nil, fmt.Errorf("read PDF page %d: %w", i, err)
		}
		total += len(text)
		if total > maxExtractText {
			return nil, errors.New("extracted text is too large")
		}
		parts = append(parts, text)
	}
	return parts, nil
}

// zipArchive reads entries while bounding the bytes actually decompressed.
type zipArchive struct {
	reader *zip.Reader
	files  map[string]*zip.File
	read   int64
}

func openZip(body []byte) (*zipArchive, error) {
	reader, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return nil, unsupported("invalid zip: %v", err)
	}
	if len(reader.File) > maxZipEntries {
		return nil, errors.New("archive has too many entries")
	}
	a := &zipArchive{reader: reader, files: map[string]*zip.File{}}
	for _, f := range reader.File {
		a.files[f.Name] = f
	}
	return a, nil
}

func (a *zipArchive) readFile(name string) ([]byte, error) {
	f, ok := a.files[name]
	if !ok {
		return nil, unsupported("missing %s", name)
	}
	if f.UncompressedSize64 > maxArtifactBytes {
		return nil, errors.New("archive entry exceeds size limit")
	}
	rc, err := f.Open()
	if err != nil {
		return nil, unsupported("open %s: %v", name, err)
	}
	defer rc.Close()
	remaining := int64(maxArtifactBytes) - a.read
	data, err := io.ReadAll(io.LimitReader(rc, min(remaining, maxArtifactBytes)+1))
	if err != nil {
		return nil, unsupported("read %s: %v", name, err)
	}
	a.read += int64(len(data))
	if int64(len(data)) > maxArtifactBytes || a.read > maxArtifactBytes {
		return nil, errors.New("archive expands beyond size limit")
	}
	return data, nil
}

var numberSuffix = regexp.MustCompile(`(\d+)\.xml$`)

// numbered returns entries matching prefix+N.xml sorted by N.
func (a *zipArchive) numbered(prefix string) []string {
	type entry struct {
		name string
		n    int
	}
	var entries []entry
	for name := range a.files {
		if !strings.HasPrefix(name, prefix) || strings.Contains(name[len(prefix):], "/") {
			continue
		}
		m := numberSuffix.FindStringSubmatch(name)
		if m == nil || name != prefix+m[1]+".xml" {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		entries = append(entries, entry{name, n})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].n < entries[j].n })
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.name
	}
	return names
}

// wordprocessingText extracts text from Word/slide XML: t elements, tab, br/cr
// and paragraph breaks.
func wordprocessingText(data []byte) (string, error) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	var b strings.Builder
	inText := false
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", unsupported("invalid XML: %v", err)
		}
		switch t := token.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "t":
				inText = true
			case "tab":
				b.WriteByte('\t')
			case "br", "cr":
				b.WriteByte('\n')
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inText = false
			case "p":
				b.WriteByte('\n')
			}
		case xml.CharData:
			if inText {
				b.Write(t)
			}
		}
		if b.Len() > maxExtractText {
			return "", errors.New("extracted text is too large")
		}
	}
	return b.String(), nil
}

func extractDOCX(body []byte) ([]string, error) {
	a, err := openZip(body)
	if err != nil {
		return nil, err
	}
	names := []string{"word/document.xml"}
	names = append(names, a.numbered("word/header")...)
	names = append(names, a.numbered("word/footer")...)
	return a.wordParts(names)
}

func extractPPTX(body []byte) ([]string, error) {
	a, err := openZip(body)
	if err != nil {
		return nil, err
	}
	names := a.numbered("ppt/slides/slide")
	if len(names) == 0 {
		return nil, unsupported("presentation has no slides")
	}
	return a.wordParts(names)
}

func (a *zipArchive) wordParts(names []string) ([]string, error) {
	var parts []string
	total := 0
	for _, name := range names {
		data, err := a.readFile(name)
		if err != nil {
			return nil, err
		}
		text, err := wordprocessingText(data)
		if err != nil {
			return nil, err
		}
		total += len(text)
		if total > maxExtractText {
			return nil, errors.New("extracted text is too large")
		}
		parts = append(parts, text)
	}
	return parts, nil
}

func extractXLSX(body []byte) ([]string, error) {
	a, err := openZip(body)
	if err != nil {
		return nil, err
	}
	var shared []string
	if _, ok := a.files["xl/sharedStrings.xml"]; ok {
		data, err := a.readFile("xl/sharedStrings.xml")
		if err != nil {
			return nil, err
		}
		if shared, err = sharedStrings(data); err != nil {
			return nil, err
		}
	}
	sheets := a.numbered("xl/worksheets/sheet")
	if len(sheets) == 0 {
		return nil, unsupported("workbook has no sheets")
	}
	var parts []string
	total := 0
	for _, name := range sheets {
		data, err := a.readFile(name)
		if err != nil {
			return nil, err
		}
		text, err := sheetText(data, shared)
		if err != nil {
			return nil, err
		}
		total += len(text)
		if total > maxExtractText {
			return nil, errors.New("extracted text is too large")
		}
		parts = append(parts, text)
	}
	return parts, nil
}

func sharedStrings(data []byte) ([]string, error) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	var (
		out     []string
		current strings.Builder
		inSI    bool
		inText  bool
		inPhon  bool
		size    int
	)
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, unsupported("invalid shared strings: %v", err)
		}
		switch t := token.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "si":
				inSI = true
				current.Reset()
			case "t":
				inText = true
			case "rPh":
				inPhon = true
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "si":
				inSI = false
				out = append(out, current.String())
			case "t":
				inText = false
			case "rPh":
				inPhon = false
			}
		case xml.CharData:
			if inSI && inText && !inPhon {
				current.Write(t)
				size += len(t)
				if size > maxExtractText {
					return nil, errors.New("extracted text is too large")
				}
			}
		}
	}
}

func sheetText(data []byte, shared []string) (string, error) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	var (
		b        strings.Builder
		row      []string
		cellType string
		value    strings.Builder
		inValue  bool
		inCell   bool
		rows     int
	)
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return b.String(), nil
		}
		if err != nil {
			return "", unsupported("invalid worksheet: %v", err)
		}
		switch t := token.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "row":
				row = row[:0]
			case "c":
				inCell = true
				cellType = ""
				value.Reset()
				for _, attr := range t.Attr {
					if attr.Name.Local == "t" {
						cellType = attr.Value
					}
				}
			case "v", "t":
				if inCell {
					inValue = true
				}
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "v", "t":
				inValue = false
			case "c":
				inCell = false
				text := value.String()
				if cellType == "s" {
					index, err := strconv.Atoi(strings.TrimSpace(text))
					if err != nil || index < 0 || index >= len(shared) {
						return "", fmt.Errorf("shared string index %q out of range", text)
					}
					text = shared[index]
				}
				row = append(row, text)
			case "row":
				if rows > 0 {
					b.WriteByte('\n')
				}
				rows++
				b.WriteString(strings.Join(row, "\t"))
				if b.Len() > maxExtractText {
					return "", errors.New("extracted text is too large")
				}
			}
		case xml.CharData:
			if inValue {
				value.Write(t)
			}
		}
	}
}

func extractCSV(body []byte) ([]string, error) {
	if !utf8.Valid(body) {
		return nil, unsupported("CSV is not valid UTF-8")
	}
	reader := csv.NewReader(bytes.NewReader(body))
	reader.FieldsPerRecord = -1
	reader.LazyQuotes = true
	var b strings.Builder
	rows := 0
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, unsupported("invalid CSV: %v", err)
		}
		rows++
		if rows > maxCSVRows {
			return nil, errors.New("CSV has too many rows")
		}
		if rows > 1 {
			b.WriteByte('\n')
		}
		b.WriteString(strings.Join(record, "\t"))
		if b.Len() > maxExtractText {
			return nil, errors.New("extracted text is too large")
		}
	}
	return []string{b.String()}, nil
}

func extractHTML(body []byte) ([]string, error) {
	if !utf8.Valid(body) {
		return nil, unsupported("HTML is not valid UTF-8")
	}
	tokenizer := html.NewTokenizer(bytes.NewReader(body))
	var b strings.Builder
	skip := 0
	writeText := func(text string) {
		text = strings.Join(strings.Fields(text), " ")
		if text == "" {
			return
		}
		if b.Len() > 0 {
			if last := b.String()[b.Len()-1]; last != ' ' && last != '\n' {
				b.WriteByte(' ')
			}
		}
		b.WriteString(text)
	}
	for {
		tokenType := tokenizer.Next()
		switch tokenType {
		case html.ErrorToken:
			if tokenizer.Err() == io.EOF {
				return []string{b.String()}, nil
			}
			return nil, unsupported("invalid HTML: %v", tokenizer.Err())
		case html.TextToken:
			if skip == 0 {
				writeText(string(tokenizer.Text()))
			}
		case html.StartTagToken, html.SelfClosingTagToken:
			name, _ := tokenizer.TagName()
			switch string(name) {
			case "script", "style", "noscript":
				if tokenType == html.StartTagToken {
					skip++
				}
			case "br":
				b.WriteByte('\n')
			}
		case html.EndTagToken:
			name, _ := tokenizer.TagName()
			switch string(name) {
			case "script", "style", "noscript":
				if skip > 0 {
					skip--
				}
			case "p", "li", "div":
				b.WriteByte('\n')
			}
		}
		if b.Len() > maxExtractText {
			return nil, errors.New("extracted text is too large")
		}
	}
}
