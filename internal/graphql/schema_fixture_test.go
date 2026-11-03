// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

const initialSchemaPath = "../database/migrations/0001_initial_schema.sql"

var (
	schemaObjectName = regexp.MustCompile(`^CREATE (?:TABLE|SEQUENCE|FUNCTION|TRIGGER|TYPE) "?([A-Za-z_]+)"?`)
	schemaTableName  = regexp.MustCompile(`^(?:ALTER TABLE (?:ONLY )?|ALTER SEQUENCE |CREATE (?:UNIQUE )?INDEX [a-z_]+ ON )([a-z_]+)`)
	schemaReference  = regexp.MustCompile(`REFERENCES ([a-z_]+)\(`)
)

// applyInitialSchemaObjects adds selected objects from the initial migration
// to a hand-built fixture schema. names lists tables, sequences, types,
// functions and triggers. Columns, constraints, indexes and sequence ownership
// follow their table or sequence. Foreign keys are applied only when the
// referenced table exists.
func applyInitialSchemaObjects(t *testing.T, ctx context.Context, pool *pgxpool.Pool, names ...string) {
	t.Helper()
	source, err := os.ReadFile(initialSchemaPath)
	if err != nil {
		t.Fatalf("read initial schema: %v", err)
	}
	wanted := make(map[string]bool, len(names))
	for _, name := range names {
		wanted[name] = true
	}
	applied := 0
	for _, statement := range splitSchemaStatements(string(source)) {
		match := schemaObjectName.FindStringSubmatch(statement)
		if match == nil {
			match = schemaTableName.FindStringSubmatch(statement)
		}
		if match == nil || !wanted[match[1]] {
			continue
		}
		if reference := schemaReference.FindStringSubmatch(statement); reference != nil {
			var exists bool
			if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, reference[1]).Scan(&exists); err != nil {
				t.Fatal(err)
			}
			if !exists && !wanted[reference[1]] {
				continue
			}
		}
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("apply initial schema statement %q: %v", statement, err)
		}
		applied++
	}
	if applied == 0 {
		t.Fatalf("initial schema has no objects named %v", names)
	}
}

// splitSchemaStatements splits the pg_dump-style migration into top-level
// statements, keeping dollar-quoted bodies intact. Parsing stops at the
// trailing DO blocks that hold the optional pgvector objects.
func splitSchemaStatements(source string) []string {
	var statements []string
	var current []string
	quoted := false
	for _, line := range strings.Split(source, "\n") {
		if len(current) == 0 && (strings.TrimSpace(line) == "" || strings.HasPrefix(line, "--")) {
			continue
		}
		if len(current) == 0 && strings.HasPrefix(line, "DO ") {
			break
		}
		current = append(current, line)
		if strings.Count(line, "$$")%2 == 1 {
			quoted = !quoted
		}
		if !quoted && strings.HasSuffix(strings.TrimSpace(line), ";") {
			statements = append(statements, strings.Join(current, "\n"))
			current = nil
		}
	}
	return statements
}
