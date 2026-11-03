// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package database

import (
	"context"
	"strings"
	"testing"
)

func TestOpenRejectsMissingOrInvalidDatabaseURL(t *testing.T) {
	for _, databaseURL := range []string{"", "   ", "not a url"} {
		t.Run(databaseURL, func(t *testing.T) {
			pool, err := Open(context.Background(), databaseURL)
			if err == nil {
				pool.Close()
				t.Fatal("expected configuration error")
			}
			if strings.Contains(err.Error(), databaseURL) && databaseURL != "" {
				t.Fatal("error must not echo the configured URL")
			}
		})
	}
}
