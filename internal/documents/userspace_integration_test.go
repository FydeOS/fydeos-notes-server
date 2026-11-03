// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package documents

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/Deln0r/ygo"
	"github.com/fydeos/fydeos-notes-server/internal/crdt/yjs"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresUserspaceRepositoryPersistsMergesAndDeletesPerUser(t *testing.T) {
	pool := userspaceTestPool(t)
	ctx := context.Background()
	for _, userID := range []string{"user-1", "user-2"} {
		if _, err := pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
			t.Fatal(err)
		}
	}
	repository := NewUserspaceRepository(pool)
	first := fixtureUpdate(t, 9201, "first")
	second := fixtureUpdate(t, 9202, "second")
	firstTimestamp, err := repository.Push(ctx, "user-1", "doc-1", first)
	if err != nil {
		t.Fatal(err)
	}
	secondTimestamp, err := repository.Push(ctx, "user-1", "doc-1", second)
	if err != nil {
		t.Fatal(err)
	}
	if secondTimestamp <= firstTimestamp {
		t.Fatalf("updated userspace timestamp = %d, want > %d", secondTimestamp, firstTimestamp)
	}

	mergedUpdate, stateVector, timestamp, err := repository.Diff(ctx, "user-1", "doc-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	wantUpdate, err := yjs.MergeUpdates([][]byte{first, second})
	if err != nil {
		t.Fatal(err)
	}
	wantDoc, err := yjs.Restore(wantUpdate, nil)
	if err != nil {
		t.Fatal(err)
	}
	gotDoc, err := yjs.Restore(mergedUpdate, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := gotDoc.StateVector(), wantDoc.StateVector(); string(got) != string(want) {
		t.Fatalf("persisted userspace state vector = %x, want %x", got, want)
	}
	if got, want := stateVector, wantDoc.StateVector(); string(got) != string(want) {
		t.Fatalf("reported userspace state vector = %x, want %x", got, want)
	}
	if timestamp != secondTimestamp {
		t.Fatalf("loaded userspace timestamp = %d, want %d", timestamp, secondTimestamp)
	}
	peer := ygo.NewDocWithOptions(ygo.Options{ClientID: 9203})
	if err := ygo.ApplyUpdate(peer, mergedUpdate); err != nil {
		t.Fatal(err)
	}
	peerDiff, _, _, err := repository.Diff(ctx, "user-1", "doc-1", ygo.EncodeStateVector(peer))
	if err != nil {
		t.Fatal(err)
	}
	if len(peerDiff) != 0 && string(peerDiff) != string([]byte{0, 0}) {
		t.Fatalf("diff for synchronized userspace peer = %x, want empty update", peerDiff)
	}

	if _, err := repository.Push(ctx, "user-1", "doc-2", fixtureUpdate(t, 9204, "second document")); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Push(ctx, "user-2", "doc-1", fixtureUpdate(t, 9205, "other user")); err != nil {
		t.Fatal(err)
	}
	cursor := firstTimestamp
	timestamps, err := repository.Timestamps(ctx, "user-1", &cursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(timestamps) != 2 || timestamps["doc-1"] != secondTimestamp || timestamps["doc-2"] <= firstTimestamp {
		t.Fatalf("userspace timestamps after cursor = %#v", timestamps)
	}

	if err := repository.Delete(ctx, "user-1", "doc-1"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := repository.Diff(ctx, "user-1", "doc-1", nil); err != ErrDocNotFound {
		t.Fatalf("deleted document error = %v, want ErrDocNotFound", err)
	}
	for _, existing := range [][2]string{{"user-1", "doc-2"}, {"user-2", "doc-1"}} {
		if _, _, _, err := repository.Diff(ctx, existing[0], existing[1], nil); err != nil {
			t.Fatalf("neighboring userspace document %s/%s: %v", existing[0], existing[1], err)
		}
	}
}

func TestPostgresUserspaceConcurrentPushesKeepAllYjsUpdates(t *testing.T) {
	pool := userspaceTestPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO users(id) VALUES('user-1')`); err != nil {
		t.Fatal(err)
	}
	repository := NewUserspaceRepository(pool)
	const writes = 12
	updates := make([][]byte, writes)
	errorsByIndex := make([]error, writes)
	timestamps := make([]int64, writes)
	start := make(chan struct{})
	var group sync.WaitGroup
	for i := range writes {
		updates[i] = fixtureUpdate(t, uint64(9300+i), fmt.Sprintf("update-%02d", i))
		group.Add(1)
		go func(index int) {
			defer group.Done()
			<-start
			timestamps[index], errorsByIndex[index] = repository.Push(ctx, "user-1", "doc-1", updates[index])
		}(i)
	}
	close(start)
	group.Wait()
	for i, err := range errorsByIndex {
		if err != nil {
			t.Fatalf("concurrent userspace push %d: %v", i, err)
		}
	}
	sort.Slice(timestamps, func(i, j int) bool { return timestamps[i] < timestamps[j] })
	for i := 1; i < len(timestamps); i++ {
		if timestamps[i] <= timestamps[i-1] {
			t.Fatalf("concurrent userspace timestamps are not strictly increasing: %v", timestamps)
		}
	}

	allUpdates, _, _, err := repository.Diff(ctx, "user-1", "doc-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	wantUpdate, err := yjs.MergeUpdates(updates)
	if err != nil {
		t.Fatal(err)
	}
	wantDoc, err := yjs.Restore(wantUpdate, nil)
	if err != nil {
		t.Fatal(err)
	}
	gotDoc, err := yjs.Restore(allUpdates, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := gotDoc.StateVector(), wantDoc.StateVector(); string(got) != string(want) {
		t.Fatalf("persisted concurrent userspace state vector = %x, want %x", got, want)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM user_snapshots WHERE user_id='user-1' AND id='doc-1'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("userspace snapshot rows after concurrent writes = %d, want 1", rows)
	}
}

func userspaceTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("NOTES_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set NOTES_TEST_DATABASE_URL to a disposable local notes_test database")
	}
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatalf("parse NOTES_TEST_DATABASE_URL: %v", err)
	}
	if parsed.Hostname() != "localhost" && parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "::1" {
		t.Fatal("integration tests require a loopback PostgreSQL host")
	}
	if strings.Trim(parsed.Path, "/") != "notes_test" {
		t.Fatal("integration tests require a disposable database named notes_test")
	}
	adminPool, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		t.Fatalf("connect integration PostgreSQL: %v", err)
	}
	schema := "notes_userspace_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := adminPool.Exec(context.Background(), `CREATE SCHEMA `+schema); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated schema: %v", err)
	}
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		_, _ = adminPool.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
		adminPool.Close()
		t.Fatalf("parse integration PostgreSQL config: %v", err)
	}
	poolConfig.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(context.Background(), poolConfig)
	if err != nil {
		_, _ = adminPool.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
		adminPool.Close()
		t.Fatalf("open schema-scoped PostgreSQL pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = adminPool.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
		adminPool.Close()
	})
	if _, err := pool.Exec(context.Background(), `
		CREATE TABLE users (id varchar PRIMARY KEY);
		CREATE TABLE user_snapshots (
			user_id varchar NOT NULL REFERENCES users(id) ON DELETE CASCADE ON UPDATE CASCADE,
			id varchar NOT NULL,
			blob bytea NOT NULL,
			created_at timestamptz(3) NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at timestamptz(3) NOT NULL,
			PRIMARY KEY (user_id, id)
		);`); err != nil {
		t.Fatalf("create userspace test schema: %v", err)
	}
	return pool
}
