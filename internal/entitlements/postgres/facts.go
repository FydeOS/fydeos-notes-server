// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/entitlements"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type Queryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func LoadFacts(ctx context.Context, queryer Queryer, targetType string, targetIDs []string) (map[string][]entitlements.Fact, error) {
	factsByID := make(map[string][]entitlements.Fact, len(targetIDs))
	if len(targetIDs) == 0 {
		return factsByID, nil
	}
	rows, err := queryer.Query(ctx, `
		SELECT target_id, source, status, plan, quantity, starts_at, expires_at, grace_until, signed_payload
		FROM entitlements
		WHERE target_type=$1 AND target_id=ANY($2::text[])
		ORDER BY target_id, updated_at DESC`, targetType, targetIDs)
	if err != nil {
		return nil, fmt.Errorf("load %s entitlement facts: %w", targetType, err)
	}
	defer rows.Close()
	for rows.Next() {
		var targetID string
		var fact entitlements.Fact
		var quantity pgtype.Int4
		var startsAt, expiresAt, graceUntil pgtype.Timestamptz
		if err := rows.Scan(&targetID, &fact.Source, &fact.Status, &fact.Plan, &quantity, &startsAt, &expiresAt, &graceUntil, &fact.SignedPayload); err != nil {
			return nil, fmt.Errorf("scan %s entitlement fact: %w", targetType, err)
		}
		if quantity.Valid {
			value := quantity.Int32
			fact.Quantity = &value
		}
		fact.StartsAt = nullableTime(startsAt)
		fact.ExpiresAt = nullableTime(expiresAt)
		fact.GraceUntil = nullableTime(graceUntil)
		factsByID[targetID] = append(factsByID[targetID], fact)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read %s entitlement facts: %w", targetType, err)
	}
	return factsByID, nil
}

func nullableTime(value pgtype.Timestamptz) *time.Time {
	if !value.Valid {
		return nil
	}
	result := value.Time
	return &result
}
