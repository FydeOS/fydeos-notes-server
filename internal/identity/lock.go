// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package identity

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

func LockEmail(ctx context.Context, tx pgx.Tx, email string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('auth:email:' || lower($1),0))`, email); err != nil {
		return fmt.Errorf("lock user email: %w", err)
	}
	return nil
}
