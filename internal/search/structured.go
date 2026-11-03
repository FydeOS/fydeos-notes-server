// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package search

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"
)

type Table string

const (
	DocTable   Table = "doc"
	BlockTable Table = "block"
)

type Pagination struct {
	Limit  *uint32 `json:"limit,omitempty"`
	Skip   *uint32 `json:"skip,omitempty"`
	Cursor *string `json:"cursor,omitempty"`
}

type Highlight struct {
	Field  string `json:"field"`
	Before string `json:"before"`
	End    string `json:"end"`
}

type SearchOptions struct {
	Fields     []string    `json:"fields"`
	Highlights []Highlight `json:"highlights,omitempty"`
	Pagination Pagination  `json:"pagination"`
}

type AggregateOptions struct {
	Hits       SearchOptions `json:"hits"`
	Pagination Pagination    `json:"pagination"`
}

type Query struct {
	Type    string   `json:"type"`
	Field   string   `json:"field,omitempty"`
	Match   string   `json:"match,omitempty"`
	Query   *Query   `json:"query,omitempty"`
	Queries []*Query `json:"queries,omitempty"`
	Occur   string   `json:"occur,omitempty"`
	Boost   *float64 `json:"boost,omitempty"`
}

type SearchRequest struct {
	Table   Table         `json:"table"`
	Query   *Query        `json:"query"`
	Options SearchOptions `json:"options"`
}

type AggregateRequest struct {
	Table   Table            `json:"table"`
	Query   *Query           `json:"query"`
	Field   string           `json:"field"`
	Options AggregateOptions `json:"options"`
}

// PermissionScope identifies who runs an aggregate. Only WorkspaceID and
// ActorUserID are checked; the ACL flags are accepted for compatibility.
type PermissionScope struct {
	WorkspaceID    string
	ActorUserID    string
	ProjectedACL   bool
	ActiveMember   bool
	SharingEnabled bool
}

type ProjectionHit struct {
	DocID      string
	BlockID    string
	UpdatedAt  time.Time
	Score      float64
	Values     map[string]any
	Highlights map[string][]any
}

type ProjectionBucket struct {
	Key   string
	Count int
	Hits  []ProjectionHit
}

const (
	maxQueryDepth   = 100
	maxQueryNodes   = 1000
	defaultPageSize = 10
)

var (
	docFields = map[string]bool{
		"workspaceId": true, "docId": true, "title": true, "summary": true,
		"createdByUserId": true, "updatedByUserId": true, "createdAt": true, "updatedAt": true,
	}
	blockFields = map[string]bool{
		"workspaceId": true, "docId": true, "blockId": true, "unitId": true, "flavour": true,
		"title": true, "summary": true, "content": true, "blob": true, "refDocId": true, "ref": true,
		"parentFlavour": true, "parentBlockId": true, "additional": true, "markdownPreview": true,
		"createdByUserId": true, "updatedByUserId": true, "createdAt": true, "updatedAt": true,
	}
	arrayFields = map[string]bool{"blob": true, "refDocId": true, "ref": true}
)

func tableFields(table Table) map[string]bool {
	switch table {
	case DocTable:
		return docFields
	case BlockTable:
		return blockFields
	}
	return nil
}

func textField(table Table) string {
	if table == DocTable {
		return "title"
	}
	return "content"
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidQuery, fmt.Sprintf(format, args...))
}

// ProjectionService evaluates structured indexer queries over the projection.
type ProjectionService struct {
	db rowQuerier
}

func NewProjectionService(db rowQuerier) *ProjectionService {
	return &ProjectionService{db: db}
}

func validateQuery(table Table, query *Query) error {
	fields := tableFields(table)
	nodes := 0
	var walk func(q *Query, depth int) error
	walk = func(q *Query, depth int) error {
		if q == nil {
			return invalid("nil query")
		}
		nodes++
		if depth > maxQueryDepth {
			return invalid("query nested too deeply")
		}
		if nodes > maxQueryNodes {
			return invalid("query has too many nodes")
		}
		if q.Boost != nil {
			if b := *q.Boost; math.IsNaN(b) || math.IsInf(b, 0) || b <= 0 {
				return invalid("invalid boost")
			}
		}
		switch q.Type {
		case "all":
			return nil
		case "match":
			if q.Match == "" || !fields[q.Field] {
				return invalid("invalid match")
			}
			return nil
		case "exists":
			if !fields[q.Field] {
				return invalid("invalid exists field")
			}
			return nil
		case "boost":
			if q.Query == nil {
				return invalid("boost without query")
			}
			return walk(q.Query, depth+1)
		case "boolean":
			if q.Occur != "must" && q.Occur != "should" && q.Occur != "must_not" {
				return invalid("invalid occur")
			}
			if len(q.Queries) == 0 {
				return invalid("boolean without queries")
			}
			for _, child := range q.Queries {
				if err := walk(child, depth+1); err != nil {
					return err
				}
			}
			return nil
		}
		return invalid("unknown query type %q", q.Type)
	}
	return walk(query, 1)
}

func validatePagination(p Pagination) error {
	limit := uint64(defaultPageSize)
	if p.Limit != nil {
		limit = uint64(*p.Limit)
	}
	if limit > maxSearchResults {
		return invalid("limit too large")
	}
	if p.Skip != nil && uint64(*p.Skip)+limit > maxSearchResults {
		return invalid("skip too large")
	}
	return nil
}

func validateSearchOptions(table Table, options SearchOptions) error {
	fields := tableFields(table)
	for _, f := range options.Fields {
		if !fields[f] {
			return invalid("unsupported field %q", f)
		}
	}
	for _, h := range options.Highlights {
		if !fields[h.Field] {
			return invalid("unsupported highlight field %q", h.Field)
		}
	}
	return validatePagination(options.Pagination)
}

func validateSearch(table Table, query *Query, options SearchOptions) error {
	if tableFields(table) == nil {
		return invalid("unsupported table %q", table)
	}
	if query == nil {
		return invalid("missing query")
	}
	if err := validateQuery(table, query); err != nil {
		return err
	}
	return validateSearchOptions(table, options)
}

// ValidateProjectionAggregate checks an aggregate request before it runs.
func ValidateProjectionAggregate(request AggregateRequest, scope PermissionScope) error {
	if scope.WorkspaceID == "" || scope.ActorUserID == "" {
		return invalid("missing scope")
	}
	if err := validateSearch(request.Table, request.Query, request.Options.Hits); err != nil {
		return err
	}
	if !tableFields(request.Table)[request.Field] {
		return invalid("unsupported aggregate field %q", request.Field)
	}
	if request.Options.Pagination.Cursor != nil {
		return invalid("aggregate cursor is not supported")
	}
	return validatePagination(request.Options.Pagination)
}

type projectionRecord struct {
	docID     string
	blockID   string
	updatedAt time.Time
	values    map[string]any
}

const projectionRowsSQL = `
SELECT u.doc_id, u.block_id, u.unit_id, u.flavour, u.title, u.summary, u.content,
	u.blob, u.ref_doc_id, u.ref, u.parent_flavour, u.parent_block_id, u.additional,
	u.markdown_preview, sn.created_at, sn.updated_at, sn.created_by, sn.updated_by
FROM search_projection_units u
JOIN snapshots sn ON sn.workspace_id = u.workspace_id AND sn.guid = u.doc_id
WHERE u.workspace_id = $1
ORDER BY u.doc_id, u.block_id, u.unit_id`

func stringsToAny(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

func (s *ProjectionService) loadRecords(ctx context.Context, workspaceID string, table Table) ([]projectionRecord, error) {
	rows, err := s.db.Query(ctx, projectionRowsSQL, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("load search projection: %w", err)
	}
	defer rows.Close()
	var records []projectionRecord
	seenDocs := map[string]bool{}
	for rows.Next() {
		var (
			docID, blockID, unitID, flavour, title, summary, content string
			parentFlavour, parentBlockID, additional, markdown       string
			blob, refDocID, ref                                      []string
			createdAt, updatedAt                                     time.Time
			createdBy, updatedBy                                     *string
		)
		if err := rows.Scan(&docID, &blockID, &unitID, &flavour, &title, &summary, &content,
			&blob, &refDocID, &ref, &parentFlavour, &parentBlockID, &additional, &markdown,
			&createdAt, &updatedAt, &createdBy, &updatedBy); err != nil {
			return nil, err
		}
		if table == DocTable && seenDocs[docID] {
			continue
		}
		seenDocs[docID] = true
		values := map[string]any{
			"workspaceId": workspaceID, "docId": docID, "title": title, "summary": summary,
			"createdAt": createdAt, "updatedAt": updatedAt,
		}
		if createdBy != nil {
			values["createdByUserId"] = *createdBy
		}
		if updatedBy != nil {
			values["updatedByUserId"] = *updatedBy
		}
		record := projectionRecord{docID: docID, updatedAt: updatedAt, values: values}
		if table == BlockTable {
			record.blockID = blockID
			values["blockId"] = blockID
			values["unitId"] = unitID
			values["flavour"] = flavour
			values["content"] = content
			values["blob"] = stringsToAny(blob)
			values["refDocId"] = stringsToAny(refDocID)
			values["ref"] = stringsToAny(ref)
			values["parentFlavour"] = parentFlavour
			values["parentBlockId"] = parentBlockID
			values["additional"] = additional
			values["markdownPreview"] = markdown
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

// Search returns every hit matching the request, sorted by relevance. Callers
// filter by permission and then paginate with ApplySearchPagination.
func (s *ProjectionService) Search(ctx context.Context, workspaceID, actorID string, request SearchRequest) ([]ProjectionHit, error) {
	if s == nil || s.db == nil || workspaceID == "" || actorID == "" {
		return nil, invalid("missing service or scope")
	}
	if err := validateSearch(request.Table, request.Query, request.Options); err != nil {
		return nil, err
	}
	records, err := s.loadRecords(ctx, workspaceID, request.Table)
	if err != nil {
		return nil, err
	}
	hits := []ProjectionHit{}
	for _, record := range records {
		score, ok := evaluate(request.Table, request.Query, record.values)
		if !ok {
			continue
		}
		hit := ProjectionHit{
			DocID: record.docID, BlockID: record.blockID, UpdatedAt: record.updatedAt, Score: score,
			Values: map[string]any{},
		}
		for _, f := range request.Options.Fields {
			if v, ok := record.values[f]; ok {
				hit.Values[f] = v
			}
		}
		for _, h := range request.Options.Highlights {
			if marked, ok := highlightValue(request.Query, h, record.values[h.Field]); ok {
				if hit.Highlights == nil {
					hit.Highlights = map[string][]any{}
				}
				hit.Highlights[h.Field] = []any{marked}
			}
		}
		hits = append(hits, hit)
	}
	sort.SliceStable(hits, func(i, j int) bool {
		a, b := hits[i], hits[j]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if !a.UpdatedAt.Equal(b.UpdatedAt) {
			return a.UpdatedAt.After(b.UpdatedAt)
		}
		if a.DocID != b.DocID {
			return a.DocID < b.DocID
		}
		return a.BlockID < b.BlockID
	})
	return hits, nil
}

func boostOf(q *Query) float64 {
	if q.Boost == nil {
		return 1
	}
	return *q.Boost
}

func scalarString(v any) (string, bool) {
	switch value := v.(type) {
	case string:
		return value, true
	case time.Time:
		return value.UTC().Format(time.RFC3339Nano), true
	case nil:
		return "", false
	}
	return fmt.Sprint(v), true
}

func evaluate(table Table, q *Query, values map[string]any) (float64, bool) {
	boost := boostOf(q)
	switch q.Type {
	case "all":
		return boost, true
	case "match":
		value, present := values[q.Field]
		if !present {
			return 0, false
		}
		if arrayFields[q.Field] {
			list, _ := value.([]any)
			for _, item := range list {
				if s, ok := item.(string); ok && s == q.Match {
					return boost, true
				}
			}
			return 0, false
		}
		text, ok := scalarString(value)
		if !ok {
			return 0, false
		}
		if q.Field == textField(table) {
			n := strings.Count(strings.ToLower(text), strings.ToLower(q.Match))
			if n == 0 {
				return 0, false
			}
			return float64(n) * boost, true
		}
		if text == q.Match {
			return boost, true
		}
		return 0, false
	case "exists":
		value, present := values[q.Field]
		if !present || value == nil {
			return 0, false
		}
		switch v := value.(type) {
		case []any:
			if len(v) == 0 {
				return 0, false
			}
		case string:
			if v == "" {
				return 0, false
			}
		}
		return boost, true
	case "boost":
		score, ok := evaluate(table, q.Query, values)
		return score * boost, ok
	case "boolean":
		total := 0.0
		switch q.Occur {
		case "must":
			for _, child := range q.Queries {
				score, ok := evaluate(table, child, values)
				if !ok {
					return 0, false
				}
				total += score
			}
		case "should":
			matched := false
			for _, child := range q.Queries {
				if score, ok := evaluate(table, child, values); ok {
					matched = true
					total += score
				}
			}
			if !matched {
				return 0, false
			}
		case "must_not":
			for _, child := range q.Queries {
				if _, ok := evaluate(table, child, values); ok {
					return 0, false
				}
			}
			total = 1
		}
		return total * boost, true
	}
	return 0, false
}

// findMatch returns the first match query on field, searching boost and
// positive boolean children depth-first.
func findMatch(q *Query, field string) *Query {
	if q == nil {
		return nil
	}
	switch q.Type {
	case "match":
		if q.Field == field {
			return q
		}
	case "boost":
		return findMatch(q.Query, field)
	case "boolean":
		if q.Occur == "must_not" {
			return nil
		}
		for _, child := range q.Queries {
			if found := findMatch(child, field); found != nil {
				return found
			}
		}
	}
	return nil
}

func highlightValue(query *Query, h Highlight, value any) (string, bool) {
	text, ok := value.(string)
	if !ok || text == "" || h.Before == "" || h.End == "" {
		return "", false
	}
	match := findMatch(query, h.Field)
	if match == nil {
		return "", false
	}
	haystack := []rune(text)
	needle := []rune(match.Match)
	if len(needle) == 0 {
		return "", false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		equal := true
		for j, r := range needle {
			if unicode.ToLower(haystack[i+j]) != unicode.ToLower(r) {
				equal = false
				break
			}
		}
		if equal {
			return html.EscapeString(string(haystack[:i])) + h.Before +
				html.EscapeString(string(haystack[i:i+len(needle)])) + h.End +
				html.EscapeString(string(haystack[i+len(needle):])), true
		}
	}
	return "", false
}

// ApplySearchPagination returns one page of hits and whether more follow.
func ApplySearchPagination(hits []ProjectionHit, pagination Pagination) ([]ProjectionHit, bool, error) {
	limit := defaultPageSize
	if pagination.Limit != nil {
		if *pagination.Limit > maxSearchResults {
			return nil, false, invalid("limit too large")
		}
		limit = int(*pagination.Limit)
	}
	offset := 0
	switch {
	case pagination.Cursor != nil:
		var cursor struct {
			Offset *json.Number `json:"offset"`
		}
		decoder := json.NewDecoder(strings.NewReader(*pagination.Cursor))
		decoder.UseNumber()
		if err := decoder.Decode(&cursor); err != nil || cursor.Offset == nil {
			return nil, false, invalid("invalid cursor")
		}
		n, err := cursor.Offset.Int64()
		if err != nil || n < 0 {
			return nil, false, invalid("invalid cursor offset")
		}
		offset = int(min(n, int64(len(hits))))
	case pagination.Skip != nil:
		offset = int(*pagination.Skip)
	}
	offset = min(offset, len(hits))
	end := min(offset+limit, len(hits))
	return hits[offset:end], end < len(hits), nil
}

func clampRange(offset, limit, length int) (int, int) {
	offset = max(0, min(offset, length))
	limit = max(0, limit)
	return offset, min(length, offset+limit)
}

// AggregateHits groups hits into buckets by the value of field.
func AggregateHits(hits []ProjectionHit, field string, bucketOffset, bucketLimit, hitOffset, hitLimit int) ([]ProjectionBucket, int) {
	groups := map[string][]ProjectionHit{}
	for _, hit := range hits {
		var keys []string
		switch v := hit.Values[field].(type) {
		case nil:
		case []any:
			seen := map[string]bool{}
			for _, item := range v {
				key, ok := scalarString(item)
				if ok && key != "" && !seen[key] {
					seen[key] = true
					keys = append(keys, key)
				}
			}
		case []string:
			seen := map[string]bool{}
			for _, key := range v {
				if key != "" && !seen[key] {
					seen[key] = true
					keys = append(keys, key)
				}
			}
		default:
			if key, ok := scalarString(v); ok && key != "" {
				keys = append(keys, key)
			}
		}
		for _, key := range keys {
			groups[key] = append(groups[key], hit)
		}
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	start, end := clampRange(bucketOffset, bucketLimit, len(keys))
	buckets := make([]ProjectionBucket, 0, end-start)
	for _, key := range keys[start:end] {
		group := groups[key]
		hs, he := clampRange(hitOffset, hitLimit, len(group))
		buckets = append(buckets, ProjectionBucket{Key: key, Count: len(group), Hits: group[hs:he]})
	}
	return buckets, len(keys)
}

// SelectFields returns the requested fields of a hit as GraphQL field lists.
func SelectFields(hit ProjectionHit, fields []string) map[string]any {
	out := make(map[string]any, len(fields))
	for _, f := range fields {
		v, ok := hit.Values[f]
		if !ok {
			continue
		}
		switch value := v.(type) {
		case []any:
			out[f] = value
		case []string:
			out[f] = stringsToAny(value)
		default:
			out[f] = []any{value}
		}
	}
	return out
}
