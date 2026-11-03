// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package search

import (
	"context"
	"errors"
	"hash/fnv"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Deln0r/ygo"
	"github.com/fydeos/fydeos-notes-server/internal/byok"
	"github.com/fydeos/fydeos-notes-server/internal/database/migrations"
	"github.com/fydeos/fydeos-notes-server/internal/documents"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func testPool(t *testing.T, migrate bool) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("NOTES_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set NOTES_TEST_DATABASE_URL to a disposable local notes_test database")
	}
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if h := parsed.Hostname(); h != "localhost" && h != "127.0.0.1" && h != "::1" {
		t.Fatal("integration tests require a loopback PostgreSQL host")
	}
	if strings.Trim(parsed.Path, "/") != "notes_test" {
		t.Fatal("integration tests require a database named notes_test")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	schema := "search_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
		admin.Close()
	})
	if migrate {
		if err := migrations.Apply(ctx, pool); err != nil {
			t.Fatal(err)
		}
	}
	return pool
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func seedWorkspace(t *testing.T, pool *pgxpool.Pool, ws string) {
	t.Helper()
	mustExec(t, pool, `INSERT INTO users(id,name,email) VALUES ($1::text,$1::text,$1::text||'@example.test') ON CONFLICT DO NOTHING`, "owner")
	mustExec(t, pool, `INSERT INTO workspaces(id) VALUES ($1)`, ws)
	mustExec(t, pool, `INSERT INTO workspace_members(workspace_id,user_id,role,state) VALUES ($1,'owner','owner','active')`, ws)
}

func insertUnit(t *testing.T, pool *pgxpool.Pool, ws, doc, block, flavour, content string) {
	t.Helper()
	mustExec(t, pool, `INSERT INTO search_projection_units(workspace_id,doc_id,block_id,unit_id,flavour,title,content,ref_doc_id,updated_at)
		VALUES ($1,$2,$3,$3,$4,$2||' title',$5,ARRAY['d1','d2'],now())`, ws, doc, block, flavour, content)
}

func insertSnapshot(t *testing.T, pool *pgxpool.Pool, ws, doc string, blob []byte) {
	t.Helper()
	mustExec(t, pool, `INSERT INTO snapshots(workspace_id,guid,blob,created_at,updated_at,created_by,updated_by)
		VALUES ($1,$2,$3,now(),now(),'owner','owner')`, ws, doc, blob)
}

func TestIntegrationKeywordSearch(t *testing.T) {
	pool := testPool(t, true)
	ctx := context.Background()
	seedWorkspace(t, pool, "ws")
	seedWorkspace(t, pool, "other")
	for _, doc := range []string{"a", "b", "c"} {
		insertSnapshot(t, pool, "ws", doc, []byte{0, 0})
	}
	insertSnapshot(t, pool, "other", "x", []byte{0, 0})
	insertUnit(t, pool, "ws", "a", "b1", "affine:paragraph", "alpha once")
	insertUnit(t, pool, "ws", "b", "b1", "affine:paragraph", "alpha alpha twice")
	insertUnit(t, pool, "ws", "b", "b2", "affine:paragraph", "alpha again")
	insertUnit(t, pool, "ws", "c", "b1", "affine:paragraph", "<img src=x onerror=alert(1)> alpha")
	insertUnit(t, pool, "ws", "nosnap", "b1", "affine:paragraph", "alpha")
	insertUnit(t, pool, "other", "x", "b1", "affine:paragraph", "alpha")
	s := NewDocsService(pool)
	got, err := s.SearchDocsPage(ctx, "ws", "alpha", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].DocID != "b" || got[0].BlockID != "b1" || !strings.Contains(got[0].Highlight, "<b>alpha</b>") {
		t.Fatalf("results %+v", got)
	}
	for _, r := range got {
		if r.DocID == "c" && (!strings.Contains(r.Highlight, "&gt;") || strings.Contains(r.Highlight, "<img")) {
			t.Fatalf("unsafe highlight %q", r.Highlight)
		}
		if r.CreatedBy == nil || *r.CreatedBy != "owner" || r.WorkspaceID != "ws" {
			t.Fatalf("metadata %+v", r)
		}
	}
	p0, _ := s.SearchDocsPage(ctx, "ws", "alpha", 1, 0)
	p1, _ := s.SearchDocsPage(ctx, "ws", "alpha", 1, 1)
	if len(p0) != 1 || len(p1) != 1 || p0[0].DocID == p1[0].DocID || p0[0].DocID != got[0].DocID || p1[0].DocID != got[1].DocID {
		t.Fatalf("pages %+v %+v", p0, p1)
	}
}

func TestIntegrationStructuredSearch(t *testing.T) {
	pool := testPool(t, true)
	ctx := context.Background()
	seedWorkspace(t, pool, "ws")
	insertSnapshot(t, pool, "ws", "doc", []byte{0, 0})
	insertUnit(t, pool, "ws", "doc", "b1", "affine:paragraph", "foo bar foo")
	insertUnit(t, pool, "ws", "doc", "b2", "affine:paragraph", "a<b>Foo")
	insertUnit(t, pool, "ws", "doc", "b3", "affine:code", "nothing")
	s := NewProjectionService(pool)
	got, err := s.Search(ctx, "ws", "actor", SearchRequest{Table: BlockTable, Query: match("content", "Foo"),
		Options: SearchOptions{Fields: []string{"blockId", "refDocId"}, Highlights: []Highlight{{Field: "content", Before: "<em>", End: "</em>"}}}})
	if err != nil || len(got) != 2 || got[0].Score != 2 || got[0].BlockID != "b1" {
		t.Fatalf("hits %+v %v", got, err)
	}
	if len(got[0].Values) != 2 || got[1].Highlights["content"][0] != "a&lt;b&gt;<em>Foo</em>" {
		t.Fatalf("values/highlights %+v", got)
	}
	docs, err := s.Search(ctx, "ws", "actor", SearchRequest{Table: DocTable, Query: &Query{Type: "all"}, Options: SearchOptions{Fields: []string{"title", "createdAt"}}})
	if err != nil || len(docs) != 1 || docs[0].Values["title"] != "doc title" || docs[0].BlockID != "" {
		t.Fatalf("doc hits %+v %v", docs, err)
	}
	if _, ok := docs[0].Values["createdAt"].(time.Time); !ok || docs[0].Values["content"] != nil {
		t.Fatalf("doc values %+v", docs[0].Values)
	}
}

type countingNotifier struct{ n atomic.Int32 }

func (c *countingNotifier) PublishWorkspaceEmbeddingProgressChanged(string) { c.n.Add(1) }

type failingReader struct{}

func (failingReader) DiffWorkspace(context.Context, string, string, []byte) ([]byte, []byte, int64, error) {
	return nil, nil, 0, errors.New("boom")
}

func pageDoc(t *testing.T, title, text string) []byte {
	t.Helper()
	doc := ygo.NewDoc()
	blocks := ygo.NewMap(doc, "blocks")
	txn := doc.WriteTxn()
	root := blocks.SetMap(txn, "root")
	root.Set(txn, "sys:id", "root")
	root.Set(txn, "sys:flavour", "affine:page")
	root.Set(txn, "prop:title", title)
	children := root.SetArray(txn, "sys:children")
	children.Push(txn, "p1")
	block := blocks.SetMap(txn, "p1")
	block.Set(txn, "sys:id", "p1")
	block.Set(txn, "sys:flavour", "affine:paragraph")
	if err := block.SetText(txn, "prop:text").Insert(txn, 0, text); err != nil {
		t.Fatal(err)
	}
	txn.Commit()
	return ygo.EncodeStateAsUpdate(doc)
}

func count(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func TestIntegrationProjectionWorker(t *testing.T) {
	pool := testPool(t, true)
	ctx := context.Background()
	seedWorkspace(t, pool, "ws")
	notifier := &countingNotifier{}
	worker := NewProjectionWorker(pool, documents.NewWorkspaceRepository(pool), notifier)
	if worked, err := worker.RunOne(ctx); worked || err != nil {
		t.Fatalf("empty queue %v %v", worked, err)
	}
	insertSnapshot(t, pool, "ws", "doc", pageDoc(t, "Hello", "world text"))
	if count(t, pool, `SELECT count(*) FROM search_projection_jobs`) != 1 {
		t.Fatal("trigger did not enqueue")
	}
	if worked, err := worker.RunOne(ctx); !worked || err != nil {
		t.Fatalf("run %v %v", worked, err)
	}
	var title, summary string
	if err := pool.QueryRow(ctx, `SELECT title, summary FROM workspace_pages WHERE workspace_id='ws' AND page_id='doc'`).Scan(&title, &summary); err != nil || title != "Hello" || summary != "world text" {
		t.Fatalf("page %q %q %v", title, summary, err)
	}
	if count(t, pool, `SELECT count(*) FROM search_projection_units WHERE doc_id='doc'`) != 2 ||
		count(t, pool, `SELECT count(*) FROM search_projection_jobs`) != 0 || notifier.n.Load() != 1 {
		t.Fatal("projection not stored")
	}
	if count(t, pool, `SELECT count(*) FROM embedding_sources WHERE source_key='doc' AND deleted_at IS NULL`) != 1 {
		t.Fatal("embedding source missing")
	}

	// Unchanged re-projection keeps a ready embedding projection ready.
	indexID := uuid.NewString()
	mustExec(t, pool, `INSERT INTO embedding_indexes(id,workspace_id,fingerprint,route_source,provider,model_id,endpoint_fingerprint,contract_version,health_status)
		VALUES ($1,'ws','fp','managed','p','m','e',1,'ready')`, indexID)
	mustExec(t, pool, `INSERT INTO embedding_workspace_states(workspace_id,active_index_id,runtime_state,index_epoch) VALUES ('ws',$1,'active',1)`, indexID)
	mustExec(t, pool, `INSERT INTO embedding_projections(source_id,index_id,status) SELECT id,$1,'ready' FROM embedding_sources`, indexID)
	mustExec(t, pool, `INSERT INTO search_projection_jobs(workspace_id,doc_id) VALUES ('ws','doc')`)
	if worked, err := worker.RunOne(ctx); !worked || err != nil {
		t.Fatal(err)
	}
	if count(t, pool, `SELECT count(*) FROM embedding_projections WHERE status='ready'`) != 1 {
		t.Fatal("unchanged document reset embedding")
	}

	// Concurrent workers process each job once.
	for i := range 6 {
		insertSnapshot(t, pool, "ws", "c"+string(rune('a'+i)), pageDoc(t, "T", "x"))
	}
	var wg sync.WaitGroup
	var processed atomic.Int32
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := NewProjectionWorker(pool, documents.NewWorkspaceRepository(pool))
			for {
				worked, err := w.RunOne(ctx)
				if err != nil {
					t.Error(err)
					return
				}
				if !worked {
					return
				}
				processed.Add(1)
			}
		}()
	}
	wg.Wait()
	if processed.Load() != 6 || count(t, pool, `SELECT count(*) FROM search_projection_jobs`) != 0 {
		t.Fatalf("processed %d", processed.Load())
	}

	// Deleted document removes derived rows.
	mustExec(t, pool, `DELETE FROM snapshots WHERE workspace_id='ws' AND guid='doc'`)
	if worked, err := worker.RunOne(ctx); !worked || err != nil {
		t.Fatal(err)
	}
	if count(t, pool, `SELECT count(*) FROM search_projection_units WHERE doc_id='doc'`) != 0 ||
		count(t, pool, `SELECT count(*) FROM workspace_pages WHERE page_id='doc'`) != 0 ||
		count(t, pool, `SELECT count(*) FROM embedding_sources WHERE source_key='doc' AND deleted_at IS NOT NULL`) != 1 {
		t.Fatal("deletion not projected")
	}

	// Reader failure schedules a retry.
	insertSnapshot(t, pool, "ws", "bad", pageDoc(t, "T", "x"))
	failing := NewProjectionWorker(pool, failingReader{})
	if worked, err := failing.RunOne(ctx); !worked || err != nil {
		t.Fatal(err)
	}
	var attempts int
	var lastError string
	var delay float64
	var claimed bool
	if err := pool.QueryRow(ctx, `SELECT attempts,last_error,extract(epoch FROM available_at-now()),claim_token IS NOT NULL FROM search_projection_jobs WHERE doc_id='bad'`).Scan(&attempts, &lastError, &delay, &claimed); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || lastError != "projection_failed" || delay < 3 || delay > 6 || claimed {
		t.Fatalf("retry %d %s %v %v", attempts, lastError, delay, claimed)
	}

	// A stolen claim turns the original store into a no-op.
	mustExec(t, pool, `UPDATE search_projection_jobs SET available_at=now() WHERE doc_id='bad'`)
	job := projectionJob{token: uuid.NewString()}
	if err := pool.QueryRow(ctx, `UPDATE search_projection_jobs SET claim_token=$1, claimed_until=now()+interval '30 seconds' WHERE doc_id='bad' RETURNING workspace_id, doc_id`, job.token).Scan(&job.workspaceID, &job.docID); err != nil {
		t.Fatal(err)
	}
	mustExec(t, pool, `UPDATE search_projection_jobs SET claim_token=$1 WHERE doc_id='bad'`, uuid.NewString())
	if err := worker.process(ctx, job); err != nil {
		t.Fatal(err)
	}
	if count(t, pool, `SELECT count(*) FROM search_projection_jobs WHERE doc_id='bad'`) != 1 || count(t, pool, `SELECT count(*) FROM search_projection_units WHERE doc_id='bad'`) != 0 {
		t.Fatal("stale store wrote")
	}
}

type fakeResolver struct {
	mu     sync.Mutex
	target byok.ChatTarget
	err    error
}

func (r *fakeResolver) ResolveServerTarget(context.Context, string, string, string) (byok.ChatTarget, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.target, r.err
}

func fakeVector(text string) []float32 {
	v := make([]float32, byok.EmbeddingDimensions)
	v[0] = 0.1
	h := fnv.New32a()
	_, _ = h.Write([]byte(strings.Fields(text + " x")[0]))
	v[1+int(h.Sum32()%1000)] = 1
	return v
}

func fakeEmbed(_ context.Context, _ byok.ChatTarget, text string) ([]float32, error) {
	return fakeVector(text), nil
}

func TestIntegrationEmbeddingAbsent(t *testing.T) {
	pool := testPool(t, false)
	ctx := context.Background()
	resolver := &fakeResolver{}
	if err := NewEmbeddingCoordinator(pool, resolver).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if worked, err := NewEmbeddingWorker(pool, resolver, nil).RunOne(ctx); worked || err != nil {
		t.Fatal(worked, err)
	}
	if total, embedded, err := NewEmbeddingProgressReader(pool).ReadEmbeddingProgress(ctx, "ws"); total != 0 || embedded != 0 || err != nil {
		t.Fatal(total, embedded, err)
	}
	searcher := NewEmbeddingCandidateSearcher(pool, resolver)
	if _, err := searcher.SearchDocs(ctx, "ws", "q", nil, 5); !errors.Is(err, ErrEmbeddingUnavailable) {
		t.Fatal(err)
	}
	if _, err := searcher.SearchArtifacts(ctx, "ws", "q", 5); !errors.Is(err, ErrEmbeddingUnavailable) {
		t.Fatal(err)
	}
}

func runWorkerUntilIdle(t *testing.T, w *EmbeddingWorker) {
	t.Helper()
	for range 100 {
		worked, err := w.RunOne(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !worked {
			return
		}
	}
	t.Fatal("worker never idled")
}

func TestIntegrationEmbeddingLifecycle(t *testing.T) {
	pool := testPool(t, true)
	ctx := context.Background()
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('embedding_chunks') IS NOT NULL`).Scan(&exists); err != nil || !exists {
		t.Skip("pgvector is not available")
	}
	seedWorkspace(t, pool, "ws")
	projector := NewProjectionWorker(pool, documents.NewWorkspaceRepository(pool))
	insertSnapshot(t, pool, "ws", "alpha-doc", pageDoc(t, "Alpha", "alpha content"))
	insertSnapshot(t, pool, "ws", "beta-doc", pageDoc(t, "Beta", "beta content"))
	for {
		worked, err := projector.RunOne(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !worked {
			break
		}
	}
	resolver := &fakeResolver{target: byok.ChatTarget{ProfileID: "p", Source: "managed", Provider: "x", ModelID: "m"}}
	coordinator := NewEmbeddingCoordinator(pool, resolver)
	if err := coordinator.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var epoch int
	var state string
	if err := pool.QueryRow(ctx, `SELECT index_epoch, runtime_state FROM embedding_workspace_states WHERE workspace_id='ws'`).Scan(&epoch, &state); err != nil || epoch != 1 || state != "active" {
		t.Fatalf("state %d %s %v", epoch, state, err)
	}
	if count(t, pool, `SELECT count(*) FROM embedding_projections WHERE status='pending'`) != 2 ||
		count(t, pool, `SELECT count(*) FROM embedding_indexes WHERE health_status='pending'`) != 1 {
		t.Fatal("projections not enqueued")
	}
	if err := coordinator.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if count(t, pool, `SELECT index_epoch FROM embedding_workspace_states`) != 1 {
		t.Fatal("second run changed state")
	}

	notifier := &countingNotifier{}
	worker := NewEmbeddingWorker(pool, resolver, notifier)
	worker.embed = func(context.Context, byok.ChatTarget, string) ([]float32, error) { return nil, errors.New("down") }
	if worked, err := worker.RunOne(ctx); !worked || err != nil {
		t.Fatal(err)
	}
	if count(t, pool, `SELECT count(*) FROM embedding_indexes WHERE health_status='retry_wait' AND next_probe_at > now()`) != 1 {
		t.Fatal("probe failure not recorded")
	}
	mustExec(t, pool, `UPDATE embedding_indexes SET next_probe_at=now()-interval '1 second'`)
	worker.embed = fakeEmbed
	runWorkerUntilIdle(t, worker)
	if count(t, pool, `SELECT count(*) FROM embedding_projections WHERE status='ready' AND active_generation_token IS NOT NULL`) != 2 {
		t.Fatal("projections not ready")
	}
	if total, embedded, err := NewEmbeddingProgressReader(pool).ReadEmbeddingProgress(ctx, "ws"); total != 2 || embedded != 2 || err != nil {
		t.Fatal(total, embedded, err)
	}
	searcher := NewEmbeddingCandidateSearcher(pool, resolver)
	searcher.embed = fakeEmbed
	results, err := searcher.SearchDocs(ctx, "ws", "alpha", nil, 10)
	if err != nil || len(results) != 2 || results[0].DocID != "alpha-doc" || results[0].Title != "Alpha" {
		t.Fatalf("semantic %+v %v", results, err)
	}
	if results, err = searcher.SearchDocs(ctx, "ws", "alpha", []string{"beta-doc"}, 10); err != nil || len(results) != 1 || results[0].DocID != "beta-doc" {
		t.Fatalf("filtered %+v %v", results, err)
	}
	if _, err := searcher.SearchDocs(ctx, "ws", "alpha", nil, 501); !errors.Is(err, ErrInvalidSearchDocsInput) {
		t.Fatal(err)
	}

	// Revision change between claim and commit: stale commit.
	mustExec(t, pool, `UPDATE embedding_projections SET status='pending' WHERE source_id=(SELECT id FROM embedding_sources WHERE source_key='alpha-doc')`)
	worker.embed = func(ctx context.Context, target byok.ChatTarget, text string) ([]float32, error) {
		mustExec(t, pool, `UPDATE embedding_sources SET content_revision='changed' WHERE source_key='alpha-doc'`)
		return fakeVector(text), nil
	}
	if worked, err := worker.RunOne(ctx); !worked || err != nil {
		t.Fatal(err)
	}
	var status, code string
	if err := pool.QueryRow(ctx, `SELECT p.status, p.last_error_code FROM embedding_projections p JOIN embedding_sources s ON s.id=p.source_id WHERE s.source_key='alpha-doc'`).Scan(&status, &code); err != nil || status != "retry_wait" || code != codeCommitFailed {
		t.Fatalf("stale commit %s %s %v", status, code, err)
	}

	// Ten provider failures end in failed.
	worker.embed = func(context.Context, byok.ChatTarget, string) ([]float32, error) { return nil, errors.New("down") }
	for range 10 {
		mustExec(t, pool, `UPDATE embedding_indexes SET health_status='ready'`)
		mustExec(t, pool, `UPDATE embedding_projections SET next_attempt_at=NULL WHERE status='retry_wait'`)
		if _, err := worker.projectOne(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := pool.QueryRow(ctx, `SELECT p.status FROM embedding_projections p JOIN embedding_sources s ON s.id=p.source_id WHERE s.source_key='alpha-doc'`).Scan(&status); err != nil || status != "failed" {
		t.Fatalf("status %s %v", status, err)
	}

	// Route change: searcher refuses, coordinator switches index.
	resolver.mu.Lock()
	resolver.target.ModelID = "m2"
	resolver.mu.Unlock()
	if _, err := searcher.SearchDocs(ctx, "ws", "alpha", nil, 10); !errors.Is(err, ErrEmbeddingUnavailable) {
		t.Fatal(err)
	}
	if err := coordinator.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if count(t, pool, `SELECT index_epoch FROM embedding_workspace_states`) != 2 || count(t, pool, `SELECT count(*) FROM embedding_indexes WHERE inactive_at IS NOT NULL`) != 1 {
		t.Fatal("route change not applied")
	}

	// Disabling embedding.
	mustExec(t, pool, `UPDATE workspaces SET enable_doc_embedding=false`)
	if err := coordinator.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var reason string
	if err := pool.QueryRow(ctx, `SELECT runtime_state, reason_code FROM embedding_workspace_states`).Scan(&state, &reason); err != nil || state != "disabled" || reason != "workspace_embedding_disabled" {
		t.Fatalf("disabled %s %s %v", state, reason, err)
	}
}

func TestIntegrationArtifactSearch(t *testing.T) {
	pool := testPool(t, true)
	ctx := context.Background()
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('embedding_chunks') IS NOT NULL`).Scan(&exists); err != nil || !exists {
		t.Skip("pgvector is not available")
	}
	seedWorkspace(t, pool, "ws")
	resolver := &fakeResolver{target: byok.ChatTarget{ProfileID: "p", Source: "byok", Provider: "x", ModelID: "m"}}
	if err := NewEmbeddingCoordinator(pool, resolver).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	mustExec(t, pool, `UPDATE embedding_indexes SET health_status='ready'`)
	for i, a := range []struct {
		status  string
		library bool
	}{{"ready", true}, {"ready", false}, {"pending", true}} {
		artifactID := uuid.NewString()
		sourceID := uuid.NewString()
		mustExec(t, pool, `INSERT INTO workspace_artifacts(id,workspace_id,content_hash,display_name,canonical_media_type,size_bytes,storage_scope,storage_key,status,library_owned)
			VALUES ($1,'ws',$5,$2,'text/plain',1,'copilot','k',$3,$4)`, artifactID, "art"+string(rune('0'+i)), a.status, a.library, "hash-"+artifactID)
		mustExec(t, pool, `INSERT INTO embedding_sources(id,workspace_id,source_kind,source_key,content_revision,descriptor_revision,recipe_revision,storage_scope,storage_key)
			VALUES ($1,'ws','artifact',$2,'r','r','r','copilot','k')`, sourceID, artifactID)
		generation := uuid.NewString()
		literal, _ := vectorLiteral(fakeVector("alpha"))
		mustExec(t, pool, `INSERT INTO embedding_chunks(generation_token,workspace_id,index_id,source_id,chunk_index,content,embedding,source_kind,artifact_id)
			SELECT $1,'ws',id,$2,0,'alpha text',$3::text::public.vector,'artifact',$4::uuid FROM embedding_indexes`, generation, sourceID, literal, artifactID)
		mustExec(t, pool, `INSERT INTO embedding_projections(source_id,index_id,status,active_generation_token) SELECT $1,id,'ready',$2 FROM embedding_indexes`, sourceID, generation)
	}
	searcher := NewEmbeddingCandidateSearcher(pool, resolver)
	searcher.embed = fakeEmbed
	got, err := searcher.SearchArtifacts(ctx, "ws", "alpha", 20)
	if err != nil || len(got) != 1 || got[0].Name != "art0" || got[0].MediaType != "text/plain" {
		t.Fatalf("artifacts %+v %v", got, err)
	}
	if _, err := searcher.SearchArtifacts(ctx, "ws", "alpha", 21); !errors.Is(err, ErrInvalidSearchDocsInput) {
		t.Fatal(err)
	}
}
