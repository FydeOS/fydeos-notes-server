// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package copilot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/database/migrations"

	"github.com/fydeos/fydeos-notes-server/internal/storage"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type artifactDBStub struct {
	rowCalls int
	updated  bool
}

func (db *artifactDBStub) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	if db.updated {
		return pgconn.NewCommandTag("UPDATE 0"), nil
	}
	db.updated = true
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

func (*artifactDBStub) Query(context.Context, string, ...any) (pgx.Rows, error) { return nil, nil }

func (db *artifactDBStub) QueryRow(_ context.Context, query string, _ ...any) pgx.Row {
	db.rowCalls++
	if strings.Contains(query, "to_regclass('embedding_sources')") {
		return artifactRowStub{values: []any{false}}
	}
	if strings.Contains(query, "INSERT INTO workspace_artifacts") {
		return artifactRowStub{values: []any{"artifact-id", "reserving"}}
	}
	return artifactRowStub{values: []any{"workspace-id", "artifact-id", testArtifactHash(), "reference.txt", "text/plain", int64(len(testArtifactBody)), time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)}}
}

type artifactRowStub struct{ values []any }

func (r artifactRowStub) Scan(dest ...any) error {
	for i, value := range r.values {
		switch target := dest[i].(type) {
		case *string:
			*target = value.(string)
		case *int64:
			*target = value.(int64)
		case *time.Time:
			*target = value.(time.Time)
		case *bool:
			*target = value.(bool)
		default:
			return io.ErrUnexpectedEOF
		}
	}
	return nil
}

type artifactObjectsStub struct {
	objects  map[string][]byte
	putCount int
	getCount int
}

func (s *artifactObjectsStub) PutObject(_ context.Context, _, key string, body io.Reader, _ storage.PutOptions) (storage.ObjectMetadata, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return storage.ObjectMetadata{}, err
	}
	if s.objects == nil {
		s.objects = make(map[string][]byte)
	}
	s.objects[key] = data
	s.putCount++
	return storage.ObjectMetadata{ContentLength: int64(len(data))}, nil
}

func (s *artifactObjectsStub) GetObject(_ context.Context, _, key string) (io.ReadCloser, storage.ObjectMetadata, error) {
	s.getCount++
	data, ok := s.objects[key]
	if !ok {
		return nil, storage.ObjectMetadata{}, io.EOF
	}
	return io.NopCloser(bytes.NewReader(data)), storage.ObjectMetadata{ContentLength: int64(len(data))}, nil
}

const testArtifactBody = "same artifact payload"

func testArtifactHash() string {
	sum := sha256.Sum256([]byte(testArtifactBody))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func TestWorkspaceArtifactAddStoresVerifiableContentAddressedObject(t *testing.T) {
	db := &artifactDBStub{}
	objects := &artifactObjectsStub{}
	service := NewWorkspaceArtifacts(db, objects)
	artifact, err := service.Add(context.Background(), "workspace-id", "reference.txt", "text/plain; charset=utf-8", strings.NewReader(testArtifactBody))
	if err != nil {
		t.Fatal(err)
	}
	if artifact.ArtifactID != "artifact-id" || artifact.FileName != "reference.txt" || artifact.MediaType != "text/plain" || artifact.EmbeddingStatus != "processing" || artifact.Size != int64(len(testArtifactBody)) {
		t.Fatalf("artifact projection = %#v", artifact)
	}
	if db.rowCalls != 4 || objects.putCount != 1 || !bytes.Equal(objects.objects["artifacts/workspace-id/"+testArtifactHash()], []byte(testArtifactBody)) {
		t.Fatalf("db row calls=%d puts=%d objects=%#v", db.rowCalls, objects.putCount, objects.objects)
	}
}

func TestWorkspaceArtifactRejectsInvalidInputBeforeStorage(t *testing.T) {
	objects := &artifactObjectsStub{}
	service := NewWorkspaceArtifacts(&artifactDBStub{}, objects)
	for _, tc := range []struct {
		name     string
		fileName string
		body     io.Reader
	}{
		{name: "empty name", body: strings.NewReader("body")},
		{name: "path name", fileName: "../secret.txt", body: strings.NewReader("body")},
		{name: "empty body", fileName: "empty.txt", body: strings.NewReader("")},
		{name: "oversized", fileName: "large.bin", body: io.LimitReader(bytes.NewReader(bytes.Repeat([]byte{'x'}, int(maxWorkspaceArtifactBytes+1))), maxWorkspaceArtifactBytes+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := service.Add(context.Background(), "workspace-id", tc.fileName, "application/octet-stream", tc.body); err == nil {
				t.Fatal("expected invalid input to fail")
			}
			if objects.putCount != 0 {
				t.Fatalf("invalid input wrote %d objects", objects.putCount)
			}
		})
	}
}

type blobAliasDBStub struct {
	query      string
	args       []any
	status     string
	storage    string
	storageKey string
	updates    int
}

func (db *blobAliasDBStub) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	db.updates++
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

func (*blobAliasDBStub) Query(context.Context, string, ...any) (pgx.Rows, error) { return nil, nil }

func (db *blobAliasDBStub) QueryRow(_ context.Context, query string, args ...any) pgx.Row {
	if strings.Contains(query, "to_regclass('embedding_sources')") {
		return artifactRowStub{values: []any{false}}
	}
	db.query = query
	db.args = args
	if db.status == "" {
		db.status = "reserving"
	}
	if db.storage == "" {
		db.storage = storage.BlobScope
	}
	if db.storageKey == "" {
		db.storageKey = "workspace-1/legacy-key"
	}
	return artifactRowStub{values: []any{"artifact-id", db.status, db.storage, db.storageKey}}
}

func TestWorkspaceArtifactAdmitsLegacyBlobByVerifiedContentHash(t *testing.T) {
	db := &blobAliasDBStub{}
	objects := &artifactObjectsStub{objects: map[string][]byte{"workspace-1/legacy-key": []byte(testArtifactBody)}}
	service := NewWorkspaceArtifacts(db, objects)
	if err := service.EnsureBlobAlias(context.Background(), "workspace-1", "legacy-key", "text/plain; charset=utf-8"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(db.query, "storage_scope,storage_key,status,extracted_text_runes") || len(db.args) != 6 {
		t.Fatalf("legacy artifact insert query=%q args=%#v", db.query, db.args)
	}
	if db.args[0] != "workspace-1" || db.args[1] != testArtifactHash() || db.args[2] != "text/plain" || db.args[3] != len(testArtifactBody) || db.args[4] != "workspace-1/legacy-key" || *db.args[5].(*int64) != int64(len(testArtifactBody)) {
		t.Fatalf("legacy artifact insert args = %#v", db.args)
	}
}

func TestWorkspaceArtifactRejectsMissingLegacyBlobBeforeRegistration(t *testing.T) {
	db := &blobAliasDBStub{}
	service := NewWorkspaceArtifacts(db, &artifactObjectsStub{})
	if err := service.EnsureBlobAlias(context.Background(), "workspace-1", "missing", "text/plain"); err == nil {
		t.Fatal("missing object was admitted")
	}
	if db.query != "" {
		t.Fatalf("registered an artifact without verifying its object: %q", db.query)
	}
}

func TestWorkspaceArtifactCompletesExistingCopilotReservation(t *testing.T) {
	db := &blobAliasDBStub{storage: storage.CopilotScope, storageKey: "artifacts/workspace-1/existing"}
	objects := &artifactObjectsStub{objects: map[string][]byte{"workspace-1/legacy-key": []byte(testArtifactBody)}}
	service := NewWorkspaceArtifacts(db, objects)
	if err := service.EnsureBlobAlias(context.Background(), "workspace-1", "legacy-key", " Text/Plain; charset=utf-8 "); err != nil {
		t.Fatal(err)
	}
	if db.updates != 1 || objects.putCount != 1 || !bytes.Equal(objects.objects[db.storageKey], []byte(testArtifactBody)) {
		t.Fatalf("reservation completion updates=%d puts=%d objects=%#v", db.updates, objects.putCount, objects.objects)
	}
	if db.args[2] != "text/plain" {
		t.Fatalf("canonical MIME type = %#v", db.args[2])
	}
}

func TestWorkspaceArtifactMediaTypeUsesContentSniffing(t *testing.T) {
	if got := canonicalArtifactMediaType([]byte("hello world\n"), "application/octet-stream"); got != "text/plain" {
		t.Fatalf("sniffed text media type = %q", got)
	}
	if got := canonicalArtifactMediaType([]byte{0, 1, 2}, "Application/X-Custom; version=1"); got != "application/x-custom" {
		t.Fatalf("declared media type = %q", got)
	}
	if got := canonicalArtifactMediaType([]byte("%PDF-1.7"), "text/plain"); got != "application/pdf" {
		t.Fatalf("sniffed PDF media type = %q", got)
	}
}

func TestWorkspaceArtifactsPostgresAndObjectLifecycle(t *testing.T) {
	databaseURL := os.Getenv("NOTES_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set NOTES_TEST_DATABASE_URL to a disposable local notes_test database")
	}
	parsed, err := pgxpool.ParseConfig(databaseURL)
	if err != nil || (parsed.ConnConfig.Host != "localhost" && parsed.ConnConfig.Host != "127.0.0.1" && parsed.ConnConfig.Host != "::1") {
		t.Fatal("workspace artifact integration test requires a loopback PostgreSQL URL")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	schema := "workspace_artifact_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	parsed.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, `SET search_path TO `+schema)
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, parsed)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		if _, err := admin.Exec(ctx, `DROP SCHEMA `+schema+` CASCADE`); err != nil {
			t.Errorf("drop artifact test schema: %v", err)
		}
		admin.Close()
	})
	if err := migrations.Apply(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id) VALUES('workspace-id')`); err != nil {
		t.Fatal(err)
	}
	objects := &artifactObjectsStub{}
	service := NewWorkspaceArtifacts(pool, objects)
	first, err := service.Add(ctx, "workspace-id", "reference.txt", "text/plain", strings.NewReader(testArtifactBody))
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Add(ctx, "workspace-id", "renamed.txt", "text/plain", strings.NewReader(testArtifactBody))
	if err != nil {
		t.Fatal(err)
	}
	if first.ArtifactID != second.ArtifactID || second.FileName != "renamed.txt" || objects.putCount != 1 {
		t.Fatalf("duplicate content result first=%#v second=%#v putCount=%d", first, second, objects.putCount)
	}
	firstPage, err := service.ReadExtracted(ctx, "workspace-id", first.ArtifactID, 4, "")
	if err != nil || firstPage.Content != "same" || !firstPage.Truncated || firstPage.NextCursor != "4" {
		t.Fatalf("read unindexed artifact first page = %#v, err=%v", firstPage, err)
	}
	secondPage, err := service.ReadExtracted(ctx, "workspace-id", first.ArtifactID, 100, firstPage.NextCursor)
	if err != nil || secondPage.Content != " artifact payload" || secondPage.Truncated {
		t.Fatalf("read unindexed artifact continuation = %#v, err=%v", secondPage, err)
	}
	readsBeforeInvalidCursor := objects.getCount
	if _, err := service.ReadExtracted(ctx, "workspace-id", first.ArtifactID, 100, "999999999"); err == nil {
		t.Fatal("out-of-range cursor was accepted")
	}
	if objects.getCount != readsBeforeInvalidCursor {
		t.Fatalf("invalid cursor fetched object: reads %d -> %d", readsBeforeInvalidCursor, objects.getCount)
	}
	legacyBody := "    " + testArtifactBody + "    "
	legacyArtifact, err := service.Add(ctx, "workspace-id", "legacy.txt", "text/plain", strings.NewReader(legacyBody))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspace_artifacts SET extracted_text_runes=NULL WHERE workspace_id='workspace-id' AND id=$1`, legacyArtifact.ArtifactID); err != nil {
		t.Fatal(err)
	}
	readsBeforeLegacyCursor := objects.getCount
	legacyInvalidCursor := strconv.Itoa(len([]rune(testArtifactBody)) + 1)
	if _, err := service.ReadExtracted(ctx, "workspace-id", legacyArtifact.ArtifactID, 100, legacyInvalidCursor); err == nil {
		t.Fatal("out-of-range legacy cursor was accepted")
	}
	if objects.getCount != readsBeforeLegacyCursor {
		t.Fatalf("legacy cursor without a cached text length fetched object: reads %d -> %d", readsBeforeLegacyCursor, objects.getCount)
	}
	if _, err := service.ReadExtracted(ctx, "workspace-id", legacyArtifact.ArtifactID, 100, legacyInvalidCursor); err == nil {
		t.Fatal("repeated out-of-range legacy cursor was accepted")
	}
	if objects.getCount != readsBeforeLegacyCursor {
		t.Fatalf("repeated legacy cursor fetched object: reads %d -> %d", readsBeforeLegacyCursor, objects.getCount)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspace_artifacts SET library_owned=false WHERE workspace_id='workspace-id' AND id=$1`, legacyArtifact.ArtifactID); err != nil {
		t.Fatal(err)
	}
	if service.EmbeddingAvailable(ctx) {
		var sourceName, sourceKey, sourceHash string
		if err := pool.QueryRow(ctx, `SELECT file_name,storage_key,content_revision FROM embedding_sources WHERE workspace_id='workspace-id' AND source_kind='artifact' AND source_key=$1`, first.ArtifactID).Scan(&sourceName, &sourceKey, &sourceHash); err != nil {
			t.Fatal(err)
		}
		if sourceName != "renamed.txt" || sourceKey != "artifacts/workspace-id/"+testArtifactHash() || sourceHash != testArtifactHash() {
			t.Fatalf("registered artifact source = %q/%q/%q", sourceName, sourceKey, sourceHash)
		}
		if first.EmbeddingStatus != "processing" || second.EmbeddingStatus != "processing" {
			t.Fatalf("unprocessed artifact status = %q/%q", first.EmbeddingStatus, second.EmbeddingStatus)
		}
	}
	items, total, err := service.List(ctx, "workspace-id", 10, 0)
	if err != nil || total != 1 || len(items) != 1 {
		t.Fatalf("list artifacts count=%d items=%#v err=%v", total, items, err)
	}
	removed, err := service.Remove(ctx, "workspace-id", first.ArtifactID)
	if err != nil || !removed {
		t.Fatalf("remove artifact=%t err=%v", removed, err)
	}
	items, total, err = service.List(ctx, "workspace-id", 10, 0)
	if err != nil || total != 0 || len(items) != 0 {
		t.Fatalf("list after removal count=%d items=%#v err=%v", total, items, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspace_artifacts SET library_owned=false WHERE id=$1`, first.ArtifactID); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO users(id,name,email) VALUES('user-1','User','user-1@example.test')`,
		`INSERT INTO ai_sessions_metadata(id,user_id,workspace_id,prompt_name) VALUES('session-1','user-1','workspace-id','chat')`,
		`INSERT INTO ai_sessions_messages(id,session_id,role,content,updated_at) VALUES('message-1','session-1','user','hello',now())`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ai_message_artifacts(message_id,workspace_id,artifact_id,role) VALUES('message-1','workspace-id',$1,'attachment')`, first.ArtifactID); err != nil {
		t.Fatal(err)
	}
	attached, err := service.ReadExtractedScoped(ctx, "workspace-id", "message-1", first.ArtifactID, 100, "", ArtifactRetrievalScope{Mode: "required", RequiredArtifactIDs: []string{first.ArtifactID}})
	if err != nil || attached.Content != testArtifactBody {
		t.Fatalf("read message-scoped non-library artifact = %#v, err=%v", attached, err)
	}
}
