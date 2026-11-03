// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package documents

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/crdt/yjs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	ErrDocNotFound = errors.New("document not found")
	ErrInvalidDoc  = errors.New("invalid document update")
)

const MaxUpdateBytes = 32 * 1024 * 1024

type Doc struct {
	UserID    string
	ID        string
	Blob      []byte
	Timestamp time.Time
}

type userspaceStore interface {
	Get(context.Context, string, string) (Doc, error)
	Append(context.Context, string, string, func([]byte, time.Time) ([]byte, time.Time, error)) (time.Time, error)
	Delete(context.Context, string, string) error
	Timestamps(context.Context, string, *time.Time) (map[string]int64, error)
}

type UserspaceRepository struct {
	store userspaceStore
}

func NewUserspaceRepository(db userspaceDB) *UserspaceRepository {
	return &UserspaceRepository{store: postgresUserspaceStore{db: db}}
}

func (r *UserspaceRepository) Push(ctx context.Context, userID, docID string, update []byte) (int64, error) {
	if len(update) == 0 {
		return 0, nil
	}
	if len(update) > MaxUpdateBytes || yjs.ValidateUpdate(update) != nil {
		return 0, ErrInvalidDoc
	}
	timestamp, err := r.store.Append(ctx, userID, docID, func(existing []byte, now time.Time) ([]byte, time.Time, error) {
		updates := make([][]byte, 0, 2)
		if len(existing) > 0 {
			if err := yjs.ValidateUpdate(existing); err != nil {
				return nil, time.Time{}, fmt.Errorf("stored userspace document is invalid: %w", err)
			}
			updates = append(updates, existing)
		}
		updates = append(updates, update)
		merged, err := yjs.MergeUpdates(updates)
		if err != nil {
			return nil, time.Time{}, fmt.Errorf("merge userspace document updates: %w", err)
		}
		return merged, now, nil
	})
	if err != nil {
		return 0, err
	}
	return timestamp.UnixMilli(), nil
}

func (r *UserspaceRepository) Diff(ctx context.Context, userID, docID string, stateVector []byte) ([]byte, []byte, int64, error) {
	doc, err := r.store.Get(ctx, userID, docID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, 0, ErrDocNotFound
	}
	if err != nil {
		return nil, nil, 0, err
	}
	state, err := yjs.Restore(doc.Blob, nil)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("restore userspace document: %w", err)
	}
	missing := doc.Blob
	if stateVector != nil {
		missing, err = state.Diff(stateVector)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("diff userspace document: %w", err)
		}
	}
	return missing, state.StateVector(), doc.Timestamp.UnixMilli(), nil
}

func (r *UserspaceRepository) Timestamps(ctx context.Context, userID string, after *int64) (map[string]int64, error) {
	var cursor *time.Time
	if after != nil {
		value := time.UnixMilli(*after)
		cursor = &value
	}
	return r.store.Timestamps(ctx, userID, cursor)
}

func (r *UserspaceRepository) Delete(ctx context.Context, userID, docID string) error {
	return r.store.Delete(ctx, userID, docID)
}

type userspaceDB interface {
	Begin(context.Context) (pgx.Tx, error)
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type postgresUserspaceStore struct {
	db userspaceDB
}

func (s postgresUserspaceStore) Get(ctx context.Context, userID, docID string) (Doc, error) {
	var doc Doc
	err := s.db.QueryRow(ctx, `SELECT user_id, id, blob, updated_at FROM user_snapshots WHERE user_id=$1 AND id=$2`, userID, docID).
		Scan(&doc.UserID, &doc.ID, &doc.Blob, &doc.Timestamp)
	return doc, err
}

func (s postgresUserspaceStore) Append(ctx context.Context, userID, docID string, merge func([]byte, time.Time) ([]byte, time.Time, error)) (time.Time, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return time.Time{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockUserspaceDocument(ctx, tx, userID, docID); err != nil {
		return time.Time{}, err
	}
	var existing []byte
	err = tx.QueryRow(ctx, `SELECT blob FROM user_snapshots WHERE user_id=$1 AND id=$2 FOR UPDATE`, userID, docID).Scan(&existing)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, err
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return time.Time{}, err
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		var previous time.Time
		if err := tx.QueryRow(ctx, `SELECT updated_at FROM user_snapshots WHERE user_id=$1 AND id=$2`, userID, docID).Scan(&previous); err != nil {
			return time.Time{}, err
		}
		previous = previous.Add(time.Millisecond)
		if previous.After(now) {
			now = previous
		}
	}
	blob, timestamp, err := merge(existing, now.UTC().Truncate(time.Millisecond))
	if err != nil {
		return time.Time{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO user_snapshots(user_id,id,blob,created_at,updated_at)
		VALUES($1,$2,$3,$4,$4)
		ON CONFLICT(user_id,id) DO UPDATE SET blob=EXCLUDED.blob,updated_at=EXCLUDED.updated_at`,
		userID, docID, blob, timestamp)
	if err != nil {
		return time.Time{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return time.Time{}, err
	}
	return timestamp, nil
}

func (s postgresUserspaceStore) Timestamps(ctx context.Context, userID string, after *time.Time) (map[string]int64, error) {
	rows, err := s.db.Query(ctx, `SELECT id, updated_at FROM user_snapshots WHERE user_id=$1 AND ($2::timestamptz IS NULL OR updated_at>$2)`, userID, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]int64)
	for rows.Next() {
		var id string
		var timestamp time.Time
		if err := rows.Scan(&id, &timestamp); err != nil {
			return nil, err
		}
		result[id] = timestamp.UnixMilli()
	}
	return result, rows.Err()
}

func (s postgresUserspaceStore) Delete(ctx context.Context, userID, docID string) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockUserspaceDocument(ctx, tx, userID, docID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM user_snapshots WHERE user_id=$1 AND id=$2`, userID, docID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func lockUserspaceDocument(ctx context.Context, tx pgx.Tx, userID, docID string) error {
	key := "userspace:" + userID + ":" + docID
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, key)
	return err
}
