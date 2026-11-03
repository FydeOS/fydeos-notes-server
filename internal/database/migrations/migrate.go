// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package migrations

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	ErrNotApplied       = errors.New("Go backend migrations are not applied")
	ErrUnknownMigration = errors.New("database contains an unknown Go backend migration")
	ErrChecksumMismatch = errors.New("Go backend migration checksum mismatch")
)

//go:embed *.sql
var migrationFiles embed.FS

type DB interface {
	Begin(context.Context) (pgx.Tx, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type migration struct {
	name     string
	sql      string
	checksum string
}

func Apply(ctx context.Context, db DB) error {
	entries, err := loadMigrations(migrationFiles)
	if err != nil {
		return err
	}
	return applyMigrations(ctx, db, entries)
}

func applyMigrations(ctx context.Context, db DB, entries []migration) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin migration transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	// Serialize concurrent migrators; the initial schema creates a
	// database-wide extension.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('go_backend_migrations'))`); err != nil {
		return fmt.Errorf("lock Go backend migrations: %w", err)
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS go_backend_migrations (
		name text PRIMARY KEY,
		checksum text NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT clock_timestamp()
	)`); err != nil {
		return fmt.Errorf("create Go backend migration ledger: %w", err)
	}
	applied, err := readApplied(ctx, tx)
	if err != nil {
		return err
	}
	if err := validateApplied(entries, applied, false); err != nil {
		return err
	}
	missingSeen := false
	for _, entry := range entries {
		checksum, ok := applied[entry.name]
		if ok {
			if missingSeen {
				return fmt.Errorf("migration history has a gap before %s", entry.name)
			}
			if checksum != entry.checksum {
				return fmt.Errorf("%w: %s", ErrChecksumMismatch, entry.name)
			}
			continue
		}
		missingSeen = true
		if _, err := tx.Exec(ctx, entry.sql); err != nil {
			return fmt.Errorf("apply migration %s: %w", entry.name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO go_backend_migrations(name, checksum) VALUES ($1, $2)`, entry.name, entry.checksum); err != nil {
			return fmt.Errorf("record migration %s: %w", entry.name, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit migration transaction: %w", err)
	}
	return nil
}

func Check(ctx context.Context, db DB) error {
	entries, err := loadMigrations(migrationFiles)
	if err != nil {
		return err
	}
	return checkMigrations(ctx, db, entries)
}

func checkMigrations(ctx context.Context, db DB, entries []migration) error {
	var exists bool
	if err := db.QueryRow(ctx, `SELECT to_regclass('go_backend_migrations') IS NOT NULL`).Scan(&exists); err != nil {
		return fmt.Errorf("check Go backend migration ledger: %w", err)
	}
	if !exists {
		return ErrNotApplied
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("read Go backend migration ledger: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	applied, err := readApplied(ctx, tx)
	if err != nil {
		return err
	}
	if err := validateApplied(entries, applied, true); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func readApplied(ctx context.Context, tx pgx.Tx) (map[string]string, error) {
	rows, err := tx.Query(ctx, `SELECT name, checksum FROM go_backend_migrations ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("read Go backend migration ledger: %w", err)
	}
	defer rows.Close()
	applied := make(map[string]string)
	for rows.Next() {
		var name, checksum string
		if err := rows.Scan(&name, &checksum); err != nil {
			return nil, fmt.Errorf("scan Go backend migration ledger: %w", err)
		}
		applied[name] = checksum
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read Go backend migration ledger: %w", err)
	}
	return applied, nil
}

func validateApplied(entries []migration, applied map[string]string, requireAll bool) error {
	known := make(map[string]string, len(entries))
	for _, entry := range entries {
		known[entry.name] = entry.checksum
	}
	for name, checksum := range applied {
		want, ok := known[name]
		if !ok {
			return fmt.Errorf("%w: %s", ErrUnknownMigration, name)
		}
		if checksum != want {
			return fmt.Errorf("%w: %s", ErrChecksumMismatch, name)
		}
	}
	if requireAll {
		for _, entry := range entries {
			if _, ok := applied[entry.name]; !ok {
				return fmt.Errorf("%w: %s", ErrNotApplied, entry.name)
			}
		}
	}
	return nil
}

func loadMigrations(source fs.FS) ([]migration, error) {
	files, err := fs.ReadDir(source, ".")
	if err != nil {
		return nil, fmt.Errorf("read migration files: %w", err)
	}
	entries := make([]migration, 0, len(files))
	for _, file := range files {
		if file.IsDir() || !strings.HasSuffix(file.Name(), ".sql") {
			continue
		}
		content, err := fs.ReadFile(source, file.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", file.Name(), err)
		}
		digest := sha256.Sum256(content)
		entries = append(entries, migration{name: file.Name(), sql: string(content), checksum: hex.EncodeToString(digest[:])})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	seenPrefixes := make(map[string]struct{}, len(entries))
	for i, entry := range entries {
		prefix, err := migrationPrefix(entry.name)
		if err != nil {
			return nil, err
		}
		if i > 0 && entries[i-1].name == entry.name {
			return nil, fmt.Errorf("duplicate migration name %q", entry.name)
		}
		if _, ok := seenPrefixes[prefix]; ok {
			return nil, fmt.Errorf("duplicate migration prefix %q", prefix)
		}
		seenPrefixes[prefix] = struct{}{}
	}
	if len(entries) == 0 {
		return nil, errors.New("no Go backend migrations are embedded")
	}
	return entries, nil
}

func migrationPrefix(name string) (string, error) {
	separator := strings.IndexByte(name, '_')
	if separator == -1 {
		return "", fmt.Errorf("invalid migration name %q", name)
	}
	prefix := name[:separator]
	switch len(prefix) {
	case 4:
		for _, digit := range prefix {
			if digit < '0' || digit > '9' {
				return "", fmt.Errorf("migration name must start with a four-digit version: %q", name)
			}
		}
	case 14:
		if _, err := time.Parse("20060102150405", prefix); err != nil {
			return "", fmt.Errorf("migration name must start with a valid fourteen-digit timestamp: %q", name)
		}
	default:
		return "", fmt.Errorf("migration name must start with a four-digit version or fourteen-digit timestamp: %q", name)
	}
	return prefix, nil
}
