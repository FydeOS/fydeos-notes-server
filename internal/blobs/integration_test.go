// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package blobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Deln0r/ygo"
	"github.com/fydeos/fydeos-notes-server/internal/storage"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const testSchema = `
CREATE TYPE "BlobStatus" AS ENUM ('pending', 'completed');
CREATE TABLE users (id text PRIMARY KEY);
CREATE TABLE workspaces (id text PRIMARY KEY);
CREATE TABLE workspace_members (workspace_id text NOT NULL, user_id text NOT NULL, role text NOT NULL, state text NOT NULL,
	updated_at timestamptz NOT NULL DEFAULT now());
CREATE TABLE workspace_invitations (workspace_id text NOT NULL, status text NOT NULL);
CREATE TABLE entitlements (target_type text NOT NULL, target_id text, source text NOT NULL, plan text NOT NULL, status text NOT NULL,
	quantity integer, starts_at timestamptz, expires_at timestamptz, grace_until timestamptz, signed_payload bytea,
	updated_at timestamptz NOT NULL DEFAULT now());
CREATE TABLE blobs (workspace_id varchar NOT NULL REFERENCES workspaces(id), key varchar NOT NULL, size integer NOT NULL,
	mime varchar NOT NULL, created_at timestamptz(3) NOT NULL DEFAULT CURRENT_TIMESTAMP, deleted_at timestamptz(3),
	status "BlobStatus" NOT NULL DEFAULT 'completed', upload_id varchar, reservation_id uuid NOT NULL DEFAULT gen_random_uuid(),
	reservation_expires_at timestamptz(3), PRIMARY KEY (workspace_id, key));
CREATE TABLE comment_attachments (workspace_id varchar NOT NULL, doc_id varchar NOT NULL, key varchar NOT NULL, size integer NOT NULL,
	mime varchar NOT NULL, name varchar NOT NULL, created_at timestamptz(3) NOT NULL DEFAULT CURRENT_TIMESTAMP, created_by varchar,
	status "BlobStatus" NOT NULL DEFAULT 'completed', reservation_id uuid NOT NULL DEFAULT gen_random_uuid(),
	reservation_expires_at timestamptz(3), deleted_at timestamptz(3), PRIMARY KEY (workspace_id, doc_id, key));
CREATE TABLE snapshots (workspace_id text NOT NULL, guid text NOT NULL, blob bytea, updated_at timestamptz(3) NOT NULL, PRIMARY KEY (workspace_id, guid));
CREATE TABLE updates (workspace_id text NOT NULL, guid text NOT NULL, blob bytea NOT NULL, created_at timestamptz(3) NOT NULL);
CREATE TABLE snapshot_histories (workspace_id text NOT NULL, guid text NOT NULL, "timestamp" timestamptz(3) NOT NULL, blob bytea NOT NULL);
`

type recordingPublisher struct {
	mu         sync.Mutex
	users      []string
	workspaces []string
}

func (p *recordingPublisher) PublishUserQuotaStateChanged(id, _ string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.users = append(p.users, id)
}

func (p *recordingPublisher) PublishWorkspaceQuotaStateChanged(id, _ string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.workspaces = append(p.workspaces, id)
}

type fakeAuth struct {
	doc      workspaces.DocumentAuthorization
	read     bool
	sync     bool
	denyDocs map[string]bool
}

func (f *fakeAuth) AuthorizeDocumentInTx(context.Context, pgx.Tx, string, string, string) (workspaces.DocumentAuthorization, error) {
	return f.doc, nil
}

func (f *fakeAuth) AuthorizeWorkspaceReadInTx(context.Context, pgx.Tx, string, string) (bool, error) {
	return f.read, nil
}

func (f *fakeAuth) AuthorizeWorkspaceSyncInTx(_ context.Context, _ pgx.Tx, _, _ string, docIDs []string) (workspaces.WorkspaceSyncAccess, error) {
	access := workspaces.WorkspaceSyncAccess{CanSync: f.sync, Documents: map[string]workspaces.DocumentSyncAccess{}}
	for _, id := range docIDs {
		access.Documents[id] = workspaces.DocumentSyncAccess{Read: !f.denyDocs[id]}
	}
	return access, nil
}

type harness struct {
	pool    *pgxpool.Pool
	objects *storage.Runtime
	dir     string
	service *Service
	pub     *recordingPublisher
	ws      string
	owner   string
	member  string
}

func testDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("NOTES_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set NOTES_TEST_DATABASE_URL to a disposable local notes_test database")
	}
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if host := parsed.Hostname(); host != "localhost" && host != "127.0.0.1" && host != "::1" {
		t.Fatal("integration tests require a loopback PostgreSQL host")
	}
	if strings.Trim(parsed.Path, "/") != "notes_test" {
		t.Fatal("integration tests require a database named notes_test")
	}
	admin, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	schema := "blobs_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(context.Background(), `CREATE SCHEMA `+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
		admin.Close()
	})
	if _, err := pool.Exec(context.Background(), testSchema); err != nil {
		t.Fatal(err)
	}
	return pool
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	pool := testDatabase(t)
	dir := t.TempDir()
	configData, _ := json.Marshal(map[string]any{"storages": map[string]any{"blob.storage": map[string]any{
		"provider": "fs", "bucket": "blobs", "config": map[string]any{"path": dir},
	}}})
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, configData, 0o600); err != nil {
		t.Fatal(err)
	}
	objects, err := storage.NewRuntime(t.Context(), pool, configPath, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{pool: pool, objects: objects, dir: dir, pub: &recordingPublisher{}, ws: "ws-1", owner: "owner", member: "member"}
	h.service = NewService(pool, objects, "cloud", "https://notes.example/")
	h.service.SetQuotaStateChangePublisher(h.pub)
	h.exec(t, `INSERT INTO users VALUES ('owner'), ('member'), ('admin'), ('outsider'), ('owner2')`)
	h.exec(t, `INSERT INTO workspaces VALUES ('ws-1'), ('ws-2'), ('ws-team')`)
	h.exec(t, `INSERT INTO workspace_members (workspace_id, user_id, role, state) VALUES
		('ws-1','owner','owner','active'), ('ws-1','member','member','active'), ('ws-1','admin','admin','active'),
		('ws-2','owner','owner','active'), ('ws-team','owner','owner','active'), ('ws-team','admin','admin','active')`)
	h.exec(t, `INSERT INTO entitlements (target_type, target_id, source, plan, status, quantity) VALUES ('workspace','ws-team','admin_grant','team','active',5)`)
	return h
}

func (h *harness) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := h.pool.Exec(t.Context(), query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func (h *harness) row(t *testing.T, ws, key string) *blobRow {
	t.Helper()
	row, err := loadBlobRow(t.Context(), h.pool, ws, key, false)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func (h *harness) objectExists(t *testing.T, key string) bool {
	t.Helper()
	meta, err := h.objects.HeadObject(t.Context(), storage.BlobScope, key)
	if err != nil {
		t.Fatal(err)
	}
	return meta != nil
}

func wantErr(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("err=%v want %v", err, want)
	}
}

func TestSetBlobLifecycle(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	body := []byte("hello")
	key := contentKey(body)
	got, err := h.service.SetBlob(ctx, h.member, h.ws, key, "text/plain", bytes.NewReader(body))
	if err != nil || got != key {
		t.Fatalf("set=%q err=%v", got, err)
	}
	row := h.row(t, h.ws, key)
	if row.status != "completed" || row.deleted {
		t.Fatalf("row=%+v", row)
	}
	if !h.objectExists(t, h.ws+"/"+key) || h.objectExists(t, blobStagingKey(h.ws, row.reservationID, key)) {
		t.Fatal("objects not in final state")
	}
	if len(h.pub.workspaces) == 0 || len(h.pub.users) == 0 || h.pub.users[0] != h.owner {
		t.Fatalf("publisher=%+v", h.pub)
	}
	// Repeat: already uploaded, no new write.
	before := h.row(t, h.ws, key).reservationID
	if got, err := h.service.SetBlob(ctx, h.member, h.ws, key, "text/plain", bytes.NewReader(body)); err != nil || got != key {
		t.Fatalf("repeat=%q err=%v", got, err)
	}
	if h.row(t, h.ws, key).reservationID != before {
		t.Fatal("repeat rewrote the reservation")
	}
	init, err := h.service.CreateBlobUpload(ctx, h.member, h.ws, key, int64(len(body)), "text/plain")
	if err != nil || init.AlreadyUploaded == nil || !*init.AlreadyUploaded || init.Method != UploadMethodGraphQL {
		t.Fatalf("init=%+v err=%v", init, err)
	}
	_, err = h.service.CreateBlobUpload(ctx, h.member, h.ws, key, 6, "text/plain")
	wantErr(t, err, errSizeMismatch)
	_, err = h.service.CreateBlobUpload(ctx, h.member, h.ws, key, 5, "image/png")
	wantErr(t, err, errMimeMismatch)
	if got, err := h.service.CompleteBlobUpload(ctx, h.member, h.ws, key, nil, nil); err != nil || got != key {
		t.Fatalf("complete completed=%q err=%v", got, err)
	}

	// Repair after the final object disappears.
	if err := h.objects.DeleteObject(ctx, storage.BlobScope, h.ws+"/"+key); err != nil {
		t.Fatal(err)
	}
	init, err = h.service.CreateBlobUpload(ctx, h.member, h.ws, key, 5, "text/plain")
	if err != nil || init.Method != UploadMethodGraphQL || init.AlreadyUploaded != nil {
		t.Fatalf("repair init=%+v err=%v", init, err)
	}
	if row := h.row(t, h.ws, key); row.status != "pending" || row.reservationID == before {
		t.Fatalf("repair row=%+v", row)
	}
	if _, err := h.service.SetBlob(ctx, h.member, h.ws, key, "text/plain", bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}

	// Metadata mismatch tombstones the row and removes the object.
	if _, err := h.objects.PutObject(ctx, storage.BlobScope, h.ws+"/"+key, bytes.NewReader(body), storage.PutOptions{ContentType: "image/gif"}); err != nil {
		t.Fatal(err)
	}
	_, err = h.service.CreateBlobUpload(ctx, h.member, h.ws, key, 5, "text/plain")
	wantErr(t, err, errMetadataMismatch)
	if row := h.row(t, h.ws, key); !row.deleted || h.objectExists(t, h.ws+"/"+key) {
		t.Fatal("mismatched object not tombstoned")
	}
	_, err = h.service.CreateBlobUpload(ctx, h.member, h.ws, key, 5, "text/plain")
	wantErr(t, err, errKeyDeleted)
}

func TestSetBlobKeyMismatch(t *testing.T) {
	h := newHarness(t)
	key := contentKey([]byte("other"))
	_, err := h.service.SetBlob(t.Context(), h.member, h.ws, key, "", bytes.NewReader([]byte("hello")))
	wantErr(t, err, errKeyMismatch)
	if row := h.row(t, h.ws, key); row == nil || !row.deleted || h.objectExists(t, h.ws+"/"+key) {
		t.Fatalf("row=%+v", row)
	}
}

func TestUploadValidationAndPermissions(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	h.exec(t, `INSERT INTO workspace_members (workspace_id, user_id, role, state) VALUES ('ws-1','outsider','member','inactive')`)
	for _, actor := range []string{"outsider", "nobody"} {
		_, err := h.service.CreateBlobUpload(ctx, actor, h.ws, "k", 1, "")
		wantErr(t, err, errUploadForbidden)
	}
	_, err := h.service.CreateBlobUpload(ctx, h.member, h.ws, "k", 0, "")
	wantErr(t, err, errInvalidReservation)
	_, err = h.service.CreateBlobUpload(ctx, h.member, h.ws, "k", 1<<31, "")
	wantErr(t, err, errSizeRange)
	_, err = h.service.CreateBlobUpload(ctx, h.member, h.ws, "a/b", 1, "")
	wantErr(t, err, errInvalidReservation)
	_, err = h.service.CompleteBlobUpload(ctx, h.member, h.ws, "missing", nil, nil)
	wantErr(t, err, errBlobNotFound)
	_, err = h.service.SetBlob(ctx, h.member, h.ws, "k", "", nil)
	wantErr(t, err, errInvalidReservation)
}

func TestQuotaDecisions(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	_, err := h.service.CreateBlobUpload(ctx, h.member, h.ws, "big", 11<<20, "")
	wantErr(t, err, errBlobLimit)
	// Usage in the owner's other free workspace counts.
	h.exec(t, `INSERT INTO blobs (workspace_id, key, size, mime, status) VALUES ('ws-2','x',2147483000,'a','completed')`)
	for i := range 4 {
		h.exec(t, `INSERT INTO blobs (workspace_id, key, size, mime, status) VALUES ('ws-2',$1,2147483000,'a','completed')`, "y"+string(rune('a'+i)))
	}
	_, err = h.service.CreateBlobUpload(ctx, h.member, h.ws, "small", 1<<20, "")
	wantErr(t, err, errStorageLimit)
	// The commercial workspace counts only itself.
	if _, err := h.service.CreateBlobUpload(ctx, "owner", "ws-team", "small", 1<<20, ""); err != nil {
		t.Fatal(err)
	}
	h.exec(t, `DELETE FROM blobs WHERE workspace_id = 'ws-2'`)
	h.exec(t, `INSERT INTO workspace_invitations VALUES ('ws-1','pending'), ('ws-1','waiting_seat')`)
	_, err = h.service.CreateBlobUpload(ctx, h.member, h.ws, "small", 1<<20, "")
	wantErr(t, err, errStorageLimit)
}

func TestConcurrentReserve(t *testing.T) {
	h := newHarness(t)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = h.service.CreateBlobUpload(t.Context(), h.member, h.ws, "same", 10, "")
		}()
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Fatal(errs)
	}
	var count int
	_ = h.pool.QueryRow(t.Context(), `SELECT COUNT(*) FROM blobs`).Scan(&count)
	if count != 1 {
		t.Fatalf("rows=%d", count)
	}
}

func TestLockTimeoutAndOwnerChange(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	conn, err := h.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1, 0))`, "storage-object:ws-1/locked"); err != nil {
		t.Fatal(err)
	}
	h.service.lockTimeout = 300 * time.Millisecond
	_, err = h.service.CreateBlobUpload(ctx, h.member, h.ws, "locked", 1, "")
	wantErr(t, err, errLockTimeout)
	_, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock_all()`)
	conn.Release()

	h.service.afterOwnerRead = func(ctx context.Context) {
		_, _ = h.pool.Exec(ctx, `UPDATE workspace_members SET state = 'left' WHERE workspace_id = 'ws-1' AND user_id = 'owner'`)
		_, _ = h.pool.Exec(ctx, `INSERT INTO workspace_members (workspace_id, user_id, role, state) VALUES ('ws-1','owner2','owner','active')`)
	}
	_, err = h.service.CreateBlobUpload(ctx, h.member, h.ws, "k", 1, "")
	wantErr(t, err, errQuotaSubjectChanged)
}

func TestManagement(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	var keys []string
	for _, content := range []string{"one", "two", "three", "four"} {
		key := contentKey([]byte(content))
		keys = append(keys, key)
		if _, err := h.service.SetBlob(ctx, h.member, h.ws, key, "", strings.NewReader(content)); err != nil {
			t.Fatal(err)
		}
	}
	list, err := h.service.ListManaged(ctx, h.owner, h.ws)
	if err != nil || len(list) != 4 || list[0].Key > list[1].Key {
		t.Fatalf("list=%v err=%v", list, err)
	}
	_, err = h.service.ListManaged(ctx, "admin", h.ws)
	wantErr(t, err, errManageForbidden)
	if _, err := h.service.ListManaged(ctx, "admin", "ws-team"); err != nil {
		t.Fatal(err)
	}
	_, err = h.service.ListManaged(ctx, h.owner, "nope")
	wantErr(t, err, errWorkspaceNotFound)

	if ok, err := h.service.Delete(ctx, h.owner, h.ws, keys[0], false); !ok || err != nil {
		t.Fatalf("delete=%v err=%v", ok, err)
	}
	if !h.objectExists(t, h.ws+"/"+keys[0]) {
		t.Fatal("soft delete removed object")
	}
	if ok, _ := h.service.Delete(ctx, h.owner, h.ws, keys[0], false); ok {
		t.Fatal("second delete affected a row")
	}
	if ok, err := h.service.Delete(ctx, h.owner, h.ws, keys[1], true); !ok || err != nil || h.objectExists(t, h.ws+"/"+keys[1]) || h.row(t, h.ws, keys[1]) != nil {
		t.Fatalf("permanent delete=%v err=%v", ok, err)
	}
	for _, key := range keys[2:] {
		if _, err := h.service.Delete(ctx, h.owner, h.ws, key, false); err != nil {
			t.Fatal(err)
		}
	}
	if n, more, err := h.service.ReleaseDeletedBatch(ctx, h.owner, h.ws, 2); n != 2 || !more || err != nil {
		t.Fatalf("release=%d/%v/%v", n, more, err)
	}
	if n, more, err := h.service.ReleaseDeletedBatch(ctx, h.owner, h.ws, 2); n != 1 || more || err != nil {
		t.Fatalf("release=%d/%v/%v", n, more, err)
	}
	for _, limit := range []int{0, 1001} {
		if _, _, err := h.service.ReleaseDeletedBatch(ctx, h.owner, h.ws, limit); err == nil {
			t.Fatal("bad limit accepted")
		}
	}
	h.exec(t, `INSERT INTO blobs (workspace_id, key, size, mime, status) SELECT 'ws-1', 'k' || g, 1, 'a', 'completed' FROM generate_series(1, 1001) g`)
	_, err = h.service.ListManaged(ctx, h.owner, h.ws)
	wantErr(t, err, errInventoryTooLarge)
}

func TestCleanup(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	if _, err := h.service.CleanupExpiredStorageReservationsV1(ctx, 0); err == nil {
		t.Fatal("zero limit accepted")
	}
	if _, err := h.service.CreateBlobUpload(ctx, h.member, h.ws, "expired", 3, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.CreateBlobUpload(ctx, h.member, h.ws, "live", 3, ""); err != nil {
		t.Fatal(err)
	}
	expired := h.row(t, h.ws, "expired")
	staging := blobStagingKey(h.ws, expired.reservationID, "expired")
	if _, err := h.objects.PutObject(ctx, storage.BlobScope, staging, strings.NewReader("abc"), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	h.exec(t, `UPDATE blobs SET reservation_expires_at = clock_timestamp() - interval '1 second' WHERE key = 'expired'`)
	n, err := h.service.CleanupExpiredStorageReservationsV1(ctx, 10)
	if err != nil || n != 1 {
		t.Fatalf("cleanup=%d err=%v", n, err)
	}
	if h.row(t, h.ws, "expired") != nil || h.row(t, h.ws, "live") == nil || h.objectExists(t, staging) {
		t.Fatal("cleanup touched the wrong rows")
	}
}

func TestCommentAttachments(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{1}, 32)...)
	_, err := h.service.UploadCommentAttachment(ctx, h.member, h.ws, "doc", "att", "pic.png", bytes.NewReader(png))
	wantErr(t, err, errInvalidReservation)
	auth := &fakeAuth{doc: workspaces.DocumentAuthorization{CommentsCreate: true}}
	h.service.SetCommentDocumentAuthorizer(auth)
	link, err := h.service.UploadCommentAttachment(ctx, h.member, h.ws, "doc", "att", "pic.png", bytes.NewReader(png))
	if err != nil || link != "https://notes.example/api/workspaces/ws-1/docs/doc/comment-attachments/att" {
		t.Fatalf("link=%q err=%v", link, err)
	}
	var mime, name, status string
	if err := h.pool.QueryRow(ctx, `SELECT mime, name, status::text FROM comment_attachments WHERE key = 'att'`).Scan(&mime, &name, &status); err != nil {
		t.Fatal(err)
	}
	if mime != "image/png" || name != "pic.png" || status != "completed" {
		t.Fatalf("row=%s/%s/%s", mime, name, status)
	}
	body, meta, err := h.service.GetCommentAttachment(ctx, h.ws, "doc", "att")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(body)
	body.Close()
	if !bytes.Equal(data, png) || meta.ContentType != "image/png" {
		t.Fatal("attachment content mismatch")
	}
	for _, key := range []string{"missing", "a/b"} {
		_, _, err := h.service.GetCommentAttachment(ctx, h.ws, "doc", key)
		wantErr(t, err, fs.ErrNotExist)
	}
	_, err = h.service.UploadCommentAttachment(ctx, h.member, h.ws, "doc", "big", "", bytes.NewReader(make([]byte, MaxCommentAttachmentSize+1)))
	wantErr(t, err, ErrCommentAttachmentQuotaExceeded)
	auth.doc.CommentsCreate = false
	_, err = h.service.UploadCommentAttachment(ctx, h.member, h.ws, "doc", "denied", "", bytes.NewReader(png))
	wantErr(t, err, errCommentCreateForbidden)
}

func encodeDoc(doc *ygo.Doc) []byte { return ygo.EncodeStateAsUpdate(doc) }

func TestManifests(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	contents := map[string][]byte{}
	for _, c := range []string{"alpha", "beta", "gamma"} {
		data := bytes.Repeat([]byte(c), 1)
		contents[contentKey(data)] = data
		if _, err := h.service.SetBlob(ctx, h.member, h.ws, contentKey(data), "text/plain", bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
	}
	big := bytes.Repeat([]byte("z"), 600<<10)
	bigKey := contentKey(big)
	if _, err := h.service.SetBlob(ctx, h.member, h.ws, bigKey, "application/octet-stream", bytes.NewReader(big)); err != nil {
		t.Fatal(err)
	}
	keyOf := func(s string) string { return contentKey([]byte(s)) }

	// Page doc referencing alpha (image), beta (attachment) and an unreachable gamma.
	page := ygo.NewDoc()
	blocks := ygo.NewMap(page, "blocks")
	txn := page.WriteTxn()
	addBlock := func(id, flavour, source string, children ...string) {
		block := blocks.SetMap(txn, id)
		block.Set(txn, "sys:id", id)
		block.Set(txn, "sys:flavour", flavour)
		if source != "" {
			block.Set(txn, "prop:sourceId", source)
		}
		arr := block.SetArray(txn, "sys:children")
		for _, child := range children {
			arr.Push(txn, child)
		}
	}
	addBlock("p", "affine:page", "", "i", "a", "b")
	addBlock("i", "affine:image", keyOf("alpha"))
	addBlock("a", "affine:attachment", keyOf("beta"))
	addBlock("b", "affine:attachment", bigKey)
	addBlock("o", "affine:image", keyOf("gamma"))
	txn.Commit()
	h.exec(t, `INSERT INTO snapshots VALUES ('ws-1', 'page-1', $1, clock_timestamp())`, encodeDoc(page))

	root := ygo.NewDoc()
	meta := ygo.NewMap(root, "meta")
	rtxn := root.WriteTxn()
	meta.Set(rtxn, "avatar", keyOf("gamma"))
	pages := meta.SetArray(rtxn, "pages")
	entry := pages.InsertMap(rtxn, 0)
	entry.Set(rtxn, "id", "page-1")
	entry2 := pages.InsertMap(rtxn, 1)
	entry2.Set(rtxn, "id", "page-hidden")
	rtxn.Commit()
	h.exec(t, `INSERT INTO snapshots VALUES ('ws-1', 'ws-1', $1, clock_timestamp())`, encodeDoc(root))
	h.exec(t, `INSERT INTO snapshots VALUES ('ws-1', 'page-hidden', $1, clock_timestamp())`, encodeDoc(page))

	auth := &fakeAuth{doc: workspaces.DocumentAuthorization{Read: true}, read: true, sync: true, denyDocs: map[string]bool{"page-hidden": true}}
	manifests := NewManifestService(h.pool, auth, h.objects)
	source := BlobSource{Type: "currentDoc", WorkspaceID: h.ws, DocID: "page-1"}
	manifest, err := manifests.DocManifest(ctx, h.member, source)
	if err != nil || len(manifest.Entries) != 3 || manifest.Version != 1 {
		t.Fatalf("manifest=%+v err=%v", manifest, err)
	}
	for i := 1; i < len(manifest.Entries); i++ {
		if manifest.Entries[i-1].Key > manifest.Entries[i].Key {
			t.Fatal("entries not sorted")
		}
	}
	_, err = manifests.DocManifest(ctx, h.member, BlobSource{Type: "currentDoc", WorkspaceID: h.ws, DocID: "none"})
	wantErr(t, err, ErrManifestNotFound)
	_, err = manifests.DocManifest(ctx, h.member, BlobSource{Type: "history", WorkspaceID: h.ws, DocID: "page-1"})
	wantErr(t, err, ErrManifestInvalidInput)
	ts := int64(1700000000000)
	_, err = manifests.DocManifest(ctx, h.member, BlobSource{Type: "history", WorkspaceID: h.ws, DocID: "page-1", TimestampMS: &ts})
	wantErr(t, err, ErrManifestAccessDenied)

	rootManifest, err := manifests.DocManifest(ctx, h.member, BlobSource{Type: "currentDoc", WorkspaceID: h.ws, DocID: h.ws})
	if err != nil || len(rootManifest.Entries) != 1 || rootManifest.Entries[0].Key != keyOf("gamma") {
		t.Fatalf("root manifest=%+v err=%v", rootManifest, err)
	}

	// Paged workspace manifest.
	var seen []string
	var cursor *string
	one := 1
	for range 10 {
		page, err := manifests.ReadableWorkspaceManifest(ctx, h.member, h.ws, cursor, &one)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range page.Entries {
			seen = append(seen, e.Source.DocID+"/"+e.Key)
		}
		if page.NextCursor == "" {
			break
		}
		next := page.NextCursor
		cursor = &next
	}
	if len(seen) != 4 {
		t.Fatalf("seen=%v", seen)
	}
	for i := 1; i < len(seen); i++ {
		if seen[i-1] >= seen[i] {
			t.Fatalf("unordered or duplicate: %v", seen)
		}
	}
	garbage := "@@"
	_, err = manifests.ReadableWorkspaceManifest(ctx, h.member, h.ws, &garbage, nil)
	wantErr(t, err, ErrManifestCursor)
	auth.sync = false
	_, err = manifests.ReadableWorkspaceManifest(ctx, h.member, h.ws, nil, nil)
	wantErr(t, err, ErrManifestWorkspaceSyncDenied)
	auth.sync = true

	// Streaming reads.
	_, _, err = manifests.OpenBlobV1(ctx, h.member, source, keyOf("gamma"))
	wantErr(t, err, ErrManifestBlobNotReferenced)
	reader, meta2, err := manifests.OpenBlobV1(ctx, h.member, source, bigKey)
	if err != nil || meta2.Size != int64(len(big)) {
		t.Fatalf("open meta=%+v err=%v", meta2, err)
	}
	data, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(data, big) {
		t.Fatalf("read %d bytes err=%v", len(data), err)
	}
	if reader.Close() != nil || reader.Close() != nil {
		t.Fatal("close failed")
	}
	if _, err := reader.Read(make([]byte, 1)); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("read after close=%v", err)
	}
	reader, _, err = manifests.OpenBlobV1(ctx, h.member, source, bigKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	h.exec(t, `INSERT INTO updates VALUES ('ws-1', 'page-1', $1, clock_timestamp())`, []byte{0, 0})
	if _, err := io.ReadAll(reader); !errors.Is(err, errManifestSourceChanged) {
		t.Fatalf("mid-stream change err=%v", err)
	}
	reader.Close()
	if err := h.objects.DeleteObject(ctx, storage.BlobScope, h.ws+"/"+keyOf("alpha")); err != nil {
		t.Fatal(err)
	}
	_, _, err = manifests.OpenBlobV1(ctx, h.member, source, keyOf("alpha"))
	wantErr(t, err, ErrManifestBlobObjectNotFound)
	h.exec(t, `UPDATE blobs SET status = 'pending', reservation_expires_at = clock_timestamp() + interval '1 hour' WHERE key = $1`, keyOf("beta"))
	_, _, err = manifests.OpenBlobV1(ctx, h.member, source, keyOf("beta"))
	wantErr(t, err, ErrManifestBlobNotCompleted)

	// Stream limit.
	for range maxOpenStreams {
		manifests.streams <- struct{}{}
	}
	_, _, err = manifests.OpenBlobV1(ctx, h.member, BlobSource{Type: "currentDoc", WorkspaceID: h.ws, DocID: h.ws}, keyOf("gamma"))
	wantErr(t, err, errManifestStreamLimit)
}
