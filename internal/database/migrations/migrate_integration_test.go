// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package migrations

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestApplyCreatesUsableSchemaAndIsIdempotent(t *testing.T) {
	pool := migrationTestPool(t)
	ctx := context.Background()
	if err := Check(ctx, pool); !errors.Is(err, ErrNotApplied) {
		t.Fatalf("check before migration = %v, want %v", err, ErrNotApplied)
	}
	for attempt := 1; attempt <= 2; attempt++ {
		if err := Apply(ctx, pool); err != nil {
			t.Fatalf("apply attempt %d: %v", attempt, err)
		}
		if err := Check(ctx, pool); err != nil {
			t.Fatalf("check after apply attempt %d: %v", attempt, err)
		}
	}
	var ledgerRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM go_backend_migrations`).Scan(&ledgerRows); err != nil {
		t.Fatal(err)
	}
	entries, err := loadMigrations(migrationFiles)
	if err != nil {
		t.Fatal(err)
	}
	if ledgerRows != len(entries) {
		t.Fatalf("ledger rows = %d, want %d", ledgerRows, len(entries))
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO users(id,name,email,password,registered,disabled) VALUES ('user-1','Admin','admin@example.test','hash',true,false);
		INSERT INTO user_features(user_id,name,type,reason,activated) VALUES ('user-1','administrator',0,'test',true);
		INSERT INTO workspaces(id) VALUES ('migrations-test-workspace');
		INSERT INTO snapshots(workspace_id,guid,blob,updated_at) VALUES ('migrations-test-workspace','doc-1','\x01'::bytea,now());
		INSERT INTO blobs(workspace_id,key,size,mime) VALUES ('migrations-test-workspace','blob-1',1,'text/plain');`); err != nil {
		t.Fatalf("insert into fresh schema: %v", err)
	}
	var administrators, snapshotSize int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM user_features WHERE user_id='user-1' AND name='administrator' AND activated`).Scan(&administrators); err != nil || administrators != 1 {
		t.Fatalf("administrator feature rows = %d, %v", administrators, err)
	}
	if err := pool.QueryRow(ctx, `SELECT size FROM snapshots WHERE workspace_id='migrations-test-workspace' AND guid='doc-1'`).Scan(&snapshotSize); err != nil || snapshotSize != 1 {
		t.Fatalf("snapshot size = %d, %v", snapshotSize, err)
	}
}

func TestCheckFailsClosedForUnknownAndChangedMigrationHistory(t *testing.T) {
	pool := migrationTestPool(t)
	ctx := context.Background()
	if err := Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO go_backend_migrations(name,checksum) VALUES('9999_unknown.sql','unknown')`); err != nil {
		t.Fatal(err)
	}
	if err := Check(ctx, pool); !errors.Is(err, ErrUnknownMigration) {
		t.Fatalf("unknown migration check error = %v, want %v", err, ErrUnknownMigration)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM go_backend_migrations WHERE name='9999_unknown.sql'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE go_backend_migrations SET checksum='changed' WHERE name='0001_initial_schema.sql'`); err != nil {
		t.Fatal(err)
	}
	if err := Check(ctx, pool); !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("checksum check error = %v, want %v", err, ErrChecksumMismatch)
	}
	if err := Apply(ctx, pool); !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("apply with changed checksum = %v, want %v", err, ErrChecksumMismatch)
	}
}

func migrationTestPool(t *testing.T) *pgxpool.Pool {
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
	schema := "notes_go_migrations_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := adminPool.Exec(context.Background(), `CREATE SCHEMA `+schema); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated schema: %v", err)
	}
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
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
	return pool
}
