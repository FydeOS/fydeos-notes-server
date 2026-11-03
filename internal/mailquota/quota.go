// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package mailquota

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var ErrDenied = errors.New("mail quota denied")

type Scope struct {
	Key                 string
	WindowSeconds       int64
	Limit               int64
	LegacyBucketSeconds int64
}

func Commit(ctx context.Context, tx pgx.Tx, scopes []Scope, now time.Time) error {
	denial, err := CommitDetailed(ctx, tx, scopes, now)
	if err != nil {
		return err
	}
	if denial != nil {
		return ErrDenied
	}
	return nil
}

type Denial struct {
	ScopeKey      string
	WindowSeconds int64
	Limit         int64
	Current       int64
	Requested     int64
}

func CommitDetailed(ctx context.Context, tx pgx.Tx, scopes []Scope, now time.Time) (*Denial, error) {
	if tx == nil || len(scopes) == 0 {
		return nil, errors.New("mail quota transaction and scopes are required")
	}
	keys := make([]string, len(scopes))
	for index, scope := range scopes {
		if scope.Key == "" || scope.WindowSeconds < 1 || scope.Limit < 1 || scope.LegacyBucketSeconds < 1 {
			return nil, errors.New("invalid mail quota scope")
		}
		keys[index] = scope.Key
	}
	sort.Strings(keys)
	for _, key := range keys {
		hash := sha256.Sum256([]byte(key))
		lockKey := int64(uint64(hash[0])<<56 | uint64(hash[1])<<48 | uint64(hash[2])<<40 | uint64(hash[3])<<32 |
			uint64(hash[4])<<24 | uint64(hash[5])<<16 | uint64(hash[6])<<8 | uint64(hash[7]))
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, lockKey); err != nil {
			return nil, fmt.Errorf("lock mail quota: %w", err)
		}
	}
	for _, scope := range scopes {
		windowStart := time.Unix(now.Unix()-scope.WindowSeconds, 0).UTC()
		var committed, reserved int64
		if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(count),0)::bigint FROM runtime_rolling_quota_counters
			WHERE scope_key=$1 AND window_seconds=$2 AND (
				bucket_start >= $3 OR (
					bucket_start < $3 AND bucket_start + ($4::double precision * interval '1 second') > $3
					AND MOD(EXTRACT(EPOCH FROM bucket_start)::bigint,$4::bigint)=0
				)
			)`, scope.Key, scope.WindowSeconds, windowStart, scope.LegacyBucketSeconds).Scan(&committed); err != nil {
			return nil, fmt.Errorf("read mail quota: %w", err)
		}
		if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(count),0)::bigint FROM runtime_rolling_quota_reservations
			WHERE scope_key=$1 AND window_seconds=$2 AND (
				bucket_start >= $3 OR (
					bucket_start < $3 AND bucket_start + ($4::double precision * interval '1 second') > $3
					AND MOD(EXTRACT(EPOCH FROM bucket_start)::bigint,$4::bigint)=0
				)
			) AND status='reserved' AND expires_at>$5`,
			scope.Key, scope.WindowSeconds, windowStart, scope.LegacyBucketSeconds, now).Scan(&reserved); err != nil {
			return nil, fmt.Errorf("read reserved mail quota: %w", err)
		}
		if committed+reserved+1 > scope.Limit {
			return &Denial{ScopeKey: scope.Key, WindowSeconds: scope.WindowSeconds, Limit: scope.Limit,
				Current: committed + reserved, Requested: 1}, nil
		}
	}
	for _, scope := range scopes {
		bucketStart := time.Unix(now.Unix(), 0).UTC()
		if _, err := tx.Exec(ctx, `INSERT INTO runtime_rolling_quota_counters(scope_key,window_seconds,bucket_start,count,expires_at,updated_at)
			VALUES($1,$2,$3,1,$4,$5) ON CONFLICT(scope_key,window_seconds,bucket_start) DO UPDATE SET
			count=runtime_rolling_quota_counters.count+1,expires_at=GREATEST(runtime_rolling_quota_counters.expires_at,EXCLUDED.expires_at),updated_at=EXCLUDED.updated_at`,
			scope.Key, scope.WindowSeconds, bucketStart, now.Add(time.Duration(scope.WindowSeconds*2)*time.Second), now); err != nil {
			return nil, fmt.Errorf("commit mail quota: %w", err)
		}
	}
	return nil, nil
}

func RecipientScopeHash(normalizedEmail string) string {
	hash := sha256.Sum256([]byte(normalizedEmail))
	return hex.EncodeToString(hash[:])[:24]
}

type Database interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func CleanupExpired(ctx context.Context, db Database, limit int64) (int64, error) {
	if db == nil || limit < 1 || limit > 10000 {
		return 0, errors.New("valid mail quota database and cleanup limit are required")
	}
	reservations, err := db.Exec(ctx, `WITH expired AS (
		SELECT ctid FROM runtime_rolling_quota_reservations
		WHERE status='reserved' AND expires_at<=clock_timestamp()
		ORDER BY expires_at LIMIT $1 FOR UPDATE SKIP LOCKED
	) UPDATE runtime_rolling_quota_reservations reservation SET status='expired',updated_at=clock_timestamp()
	FROM expired WHERE reservation.ctid=expired.ctid`, limit)
	if err != nil {
		return 0, fmt.Errorf("expire rolling mail quota reservations: %w", err)
	}
	counters, err := db.Exec(ctx, `DELETE FROM runtime_rolling_quota_counters WHERE ctid IN (
		SELECT ctid FROM runtime_rolling_quota_counters
		WHERE expires_at<=clock_timestamp() ORDER BY expires_at LIMIT $1 FOR UPDATE SKIP LOCKED
	)`, limit)
	if err != nil {
		return reservations.RowsAffected(), fmt.Errorf("delete expired rolling mail quota counters: %w", err)
	}
	return reservations.RowsAffected() + counters.RowsAffected(), nil
}
