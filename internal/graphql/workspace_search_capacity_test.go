// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Opt in because this fixture exercises ten times the baseline volume and four
// concurrent HTTP requests. Ordinary tests retain small, deterministic fixtures.
func runWorkspaceSearchCapacity(t *testing.T, pool *pgxpool.Pool, handler http.Handler, f graphqlFixture) {
	if os.Getenv("NOTES_TEST_SEARCH_CAPACITY") != "1" {
		return
	}
	for _, docs := range []int{1000, 10000} {
		t.Run(fmt.Sprintf("capacity_%d_docs", docs), func(t *testing.T) {
			ctx := context.Background()
			statements := []string{
				`INSERT INTO snapshots(workspace_id,guid,blob,created_at,updated_at,created_by,updated_by) SELECT $1,'capacity-'||lpad(n::text,5,'0'),'\x0000','2026-10-01','2026-10-01',$3,$3 FROM generate_series(1,$2::int) n ON CONFLICT DO NOTHING`,
				`INSERT INTO workspace_pages(workspace_id,page_id,title) SELECT $1,'capacity-'||lpad(n::text,5,'0'),'capacity document' FROM generate_series(1,$2::int) n ON CONFLICT DO NOTHING`,
				`INSERT INTO doc_access_policies(workspace_id,doc_id,member_default_role) SELECT $1,'capacity-'||lpad(n::text,5,'0'),'none' FROM generate_series(1,$2::int) n ON CONFLICT DO NOTHING`,
				`INSERT INTO doc_grants(workspace_id,doc_id,principal_type,principal_id,role) SELECT $1,'capacity-'||lpad(n::text,5,'0'),'user',$3,'reader' FROM generate_series(1,$2::int) n WHERE n%2=0 AND NOT EXISTS(SELECT 1 FROM doc_grants g WHERE g.workspace_id=$1 AND g.doc_id='capacity-'||lpad(n::text,5,'0'))`,
				`INSERT INTO search_projection_units(workspace_id,doc_id,block_id,unit_id,flavour,title,content,markdown_preview,updated_at) SELECT $1,'capacity-'||lpad(n::text,5,'0'),'block-'||b,'unit-'||b,CASE WHEN b%2=0 THEN 'affine:paragraph' ELSE 'affine:code' END,'capacity document','capacity '||repeat('padding ',128),'preview '||repeat('padding ',128),'2026-10-01' FROM generate_series(1,$2::int) n CROSS JOIN generate_series(1,10) b ON CONFLICT DO NOTHING`,
			}
			for i, sql := range statements {
				args := []any{f.workspaceID, docs}
				if i == 0 {
					args = append(args, f.userID)
				}
				if i == 3 {
					args = append(args, f.sharedUserID)
				}
				if _, err := pool.Exec(ctx, sql, args...); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := pool.Exec(ctx, `ANALYZE snapshots; ANALYZE workspace_pages; ANALYZE doc_access_policies; ANALYZE doc_grants; ANALYZE search_projection_units`); err != nil {
				t.Fatal(err)
			}
			searchQuery := `query($workspace:String!,$input:SearchInput!){workspace(id:$workspace){search(input:$input){nodes{fields} pagination{count hasMore nextCursor}}}}`
			aggregateQuery := `query($workspace:String!,$input:AggregateInput!){workspace(id:$workspace){aggregate(input:$input){buckets{key count hits{nodes{fields highlights}}} pagination{count hasMore}}}}`
			docQuery := `query($workspace:String!,$input:SearchDocsInput!){workspace(id:$workspace){searchDocs(input:$input){docId highlight}}}`
			match := map[string]any{"type": "match", "field": "content", "match": "capacity"}
			searchInput := func(offset int) map[string]any {
				return map[string]any{"table": "block", "query": match, "options": map[string]any{"fields": []string{"docId", "blockId"}, "pagination": map[string]any{"limit": 20, "cursor": fmt.Sprintf(`{"offset":%d}`, offset)}}}
			}
			aggregateInput := func(field string) map[string]any {
				return map[string]any{"table": "block", "query": match, "field": field, "options": map[string]any{"hits": map[string]any{"fields": []string{"docId"}, "pagination": map[string]any{"limit": 2}}, "pagination": map[string]any{"limit": 10}}}
			}
			highlightInput := aggregateInput("docId")
			highlightInput["options"].(map[string]any)["hits"].(map[string]any)["highlights"] = []any{map[string]any{"field": "content", "before": "<b>", "end": "</b>"}}
			previewInput := aggregateInput("docId")
			previewInput["options"].(map[string]any)["hits"].(map[string]any)["fields"] = []string{"docId", "blockId", "parentBlockId", "parentFlavour", "additional", "markdownPreview"}
			cases := []struct {
				name, query string
				input       map[string]any
				concurrency int
			}{
				{"search_first", searchQuery, searchInput(0), 1},
				{"search_deep", searchQuery, searchInput(docs*5 - 20), 1},
				{"aggregate_flavour", aggregateQuery, aggregateInput("flavour"), 1},
				{"aggregate_documents", aggregateQuery, aggregateInput("docId"), 1},
				{"aggregate_highlights", aggregateQuery, highlightInput, 1},
				{"aggregate_previews", aggregateQuery, previewInput, 1},
				{"search_docs", docQuery, map[string]any{"keyword": "capacity", "limit": 20}, 1},
				{"search_concurrent", searchQuery, searchInput(0), 4},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					runtime.GC()
					var before runtime.MemStats
					runtime.ReadMemStats(&before)
					stop, done := make(chan struct{}), make(chan uint64, 1)
					go func() {
						peak := before.HeapAlloc
						ticker := time.NewTicker(5 * time.Millisecond)
						defer ticker.Stop()
						for {
							select {
							case <-ticker.C:
								var m runtime.MemStats
								runtime.ReadMemStats(&m)
								peak = max(peak, m.HeapAlloc)
							case <-stop:
								done <- peak
								return
							}
						}
					}()
					started := time.Now()
					var wg sync.WaitGroup
					errs := make(chan error, tc.concurrency)
					for i := 0; i < tc.concurrency; i++ {
						wg.Add(1)
						go func() {
							defer wg.Done()
							result, err := capacityGraphRequest(handler, tc.query, map[string]any{"workspace": f.workspaceID, "input": tc.input}, f)
							if err == nil {
								err = checkCapacityResult(tc.name, result, docs)
							}
							errs <- err
						}()
					}
					wg.Wait()
					close(stop)
					peak := <-done
					elapsed := time.Since(started)
					var after runtime.MemStats
					runtime.ReadMemStats(&after)
					for i := 0; i < tc.concurrency; i++ {
						if err := <-errs; err != nil {
							t.Error(err)
						}
					}
					peak = max(peak, after.HeapAlloc) - before.HeapAlloc
					t.Logf("CAPACITY docs=%d blocks=%d requests=%d elapsed_ms=%d peak_heap_delta_bytes=%d total_alloc_bytes=%d", docs, docs*10, tc.concurrency, elapsed.Milliseconds(), peak, after.TotalAlloc-before.TotalAlloc)
					if elapsed > 30*time.Second {
						t.Errorf("local capacity request group exceeded 30 seconds: %s", elapsed)
					}
					if peak > uint64(tc.concurrency)*512<<20 {
						t.Errorf("local capacity group exceeded 512 MiB per concurrent request: %d", peak)
					}
				})
			}
		})
	}
}

func capacityGraphRequest(handler http.Handler, query string, variables map[string]any, f graphqlFixture) (map[string]any, error) {
	body, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	request := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	request.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: f.sessionID})
	request.AddCookie(&http.Cookie{Name: auth.UserCookieName, Value: f.sharedUserID})
	response := newDeadlineTestResponseWriter()
	handler.ServeHTTP(response, request)
	if response.Code != 200 {
		return nil, fmt.Errorf("HTTP status %d: %s", response.Code, response.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		return nil, err
	}
	if result["errors"] != nil {
		return nil, fmt.Errorf("GraphQL errors: %v", result["errors"])
	}
	return result["data"].(map[string]any)["workspace"].(map[string]any), nil
}

func checkCapacityResult(name string, result map[string]any, docs int) error {
	checkID := func(id string) error {
		n, err := strconv.Atoi(strings.TrimPrefix(id, "capacity-"))
		if err != nil || n < 1 || n > docs || n%2 != 0 {
			return fmt.Errorf("denied or unexpected document %q", id)
		}
		return nil
	}
	if name == "search_docs" {
		items := result["searchDocs"].([]any)
		if len(items) != 20 {
			return fmt.Errorf("searchDocs count=%d", len(items))
		}
		for _, v := range items {
			if err := checkID(v.(map[string]any)["docId"].(string)); err != nil {
				return err
			}
		}
		return nil
	}
	if strings.HasPrefix(name, "aggregate") {
		data := result["aggregate"].(map[string]any)
		buckets := data["buckets"].([]any)
		want := 10
		if name == "aggregate_flavour" {
			want = 2
		}
		if len(buckets) != want {
			return fmt.Errorf("bucket count=%d, want %d", len(buckets), want)
		}
		for _, b := range buckets {
			bucket := b.(map[string]any)
			count := 10
			if name == "aggregate_flavour" {
				count = docs * 5 / 2
			}
			if bucket["count"] != float64(count) {
				return fmt.Errorf("bucket count=%v want %d", bucket["count"], count)
			}
			nodes := bucket["hits"].(map[string]any)["nodes"].([]any)
			if len(nodes) != 2 {
				return fmt.Errorf("bucket hits=%d", len(nodes))
			}
			for _, n := range nodes {
				node := n.(map[string]any)
				if name == "aggregate_highlights" && !strings.HasPrefix(node["highlights"].(map[string]any)["content"].([]any)[0].(string), "<b>capacity</b>") {
					return fmt.Errorf("missing highlighted content")
				}
				if name == "aggregate_previews" && !strings.HasPrefix(node["fields"].(map[string]any)["markdownPreview"].([]any)[0].(string), "preview padding") {
					return fmt.Errorf("missing markdown preview")
				}
				if err := checkID(n.(map[string]any)["fields"].(map[string]any)["docId"].([]any)[0].(string)); err != nil {
					return err
				}
			}
		}
		return nil
	}
	data := result["search"].(map[string]any)
	nodes := data["nodes"].([]any)
	if len(nodes) != 20 {
		return fmt.Errorf("search nodes=%d", len(nodes))
	}
	seen := map[string]bool{}
	for _, n := range nodes {
		fields := n.(map[string]any)["fields"].(map[string]any)
		id := fields["docId"].([]any)[0].(string)
		if err := checkID(id); err != nil {
			return err
		}
		key := id + "/" + fields["blockId"].([]any)[0].(string)
		if seen[key] {
			return fmt.Errorf("duplicate hit %s", key)
		}
		seen[key] = true
	}
	wantMore := name != "search_deep"
	if data["pagination"].(map[string]any)["hasMore"] != wantMore {
		return fmt.Errorf("pagination hasMore mismatch: %v", data["pagination"])
	}
	first := nodes[0].(map[string]any)["fields"].(map[string]any)["docId"].([]any)[0]
	wantFirst := "capacity-00002"
	if name == "search_deep" {
		wantFirst = fmt.Sprintf("capacity-%05d", docs-2)
	}
	if first != wantFirst {
		return fmt.Errorf("first hit=%v want %s", first, wantFirst)
	}
	return nil
}
