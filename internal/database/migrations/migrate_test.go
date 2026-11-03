// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package migrations

import (
	"testing"
	"testing/fstest"
)

func TestLoadMigrationsAcceptsVersionAndTimestampPrefixes(t *testing.T) {
	entries, err := loadMigrations(fstest.MapFS{
		"0001_initial.sql":                {Data: []byte("SELECT 1;")},
		"20261019085000_invite_quota.sql": {Data: []byte("SELECT 2;")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].name != "0001_initial.sql" || entries[1].name != "20261019085000_invite_quota.sql" {
		t.Fatalf("migrations = %#v", entries)
	}
}

func TestLoadMigrationsRejectsInvalidOrDuplicatePrefixes(t *testing.T) {
	for _, name := range []string{
		"123_initial.sql",
		"+001_initial.sql",
		"20261301000000_invalid_month.sql",
		"20260230000000_invalid_day.sql",
		"20261019085000.sql",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadMigrations(fstest.MapFS{name: {Data: []byte("SELECT 1;")}}); err == nil {
				t.Fatal("invalid migration name was accepted")
			}
		})
	}

	_, err := loadMigrations(fstest.MapFS{
		"20261019085000_first.sql":  {Data: []byte("SELECT 1;")},
		"20261019085000_second.sql": {Data: []byte("SELECT 2;")},
	})
	if err == nil {
		t.Fatal("duplicate migration timestamp was accepted")
	}
}
