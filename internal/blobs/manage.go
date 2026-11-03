// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package blobs

import (
	"context"
	"errors"

	"github.com/fydeos/fydeos-notes-server/internal/storage"
	"github.com/jackc/pgx/v5"
)

const maxManagedBlobs = 1000

func (s *Service) ListManaged(ctx context.Context, actorID, workspaceID string) ([]Blob, error) {
	if err := s.requireManager(ctx, s.pool, actorID, workspaceID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT key, mime, size, created_at FROM blobs
		WHERE workspace_id = $1 AND status = 'completed' AND deleted_at IS NULL ORDER BY key LIMIT $2`, workspaceID, maxManagedBlobs+1)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Blob, error) {
		var b Blob
		err := row.Scan(&b.Key, &b.MIME, &b.Size, &b.CreatedAt)
		return b, err
	})
	if err != nil {
		return nil, err
	}
	if len(list) > maxManagedBlobs {
		return nil, errInventoryTooLarge
	}
	if list == nil {
		list = []Blob{}
	}
	return list, nil
}

func (s *Service) Delete(ctx context.Context, actorID, workspaceID, key string, permanently bool) (bool, error) {
	if err := s.requireManager(ctx, s.pool, actorID, workspaceID); err != nil {
		return false, err
	}
	deleted := false
	err := s.withObjectLock(ctx, workspaceID, blobObjectKey(workspaceID, key), func() error {
		var reservationID string
		err := s.inTx(ctx, func(tx pgx.Tx) error {
			if err := s.requireManager(ctx, tx, actorID, workspaceID); err != nil {
				return err
			}
			err := tx.QueryRow(ctx, `UPDATE blobs SET deleted_at = COALESCE(deleted_at, clock_timestamp()), reservation_expires_at = NULL
				WHERE workspace_id = $1 AND key = $2 AND status = 'completed' AND deleted_at IS NULL
				RETURNING reservation_id::text`, workspaceID, key).Scan(&reservationID)
			if isNoRows(err) {
				return nil
			}
			deleted = err == nil
			return err
		})
		if err != nil || !deleted {
			return err
		}
		defer s.publishWorkspace(ctx, workspaceID)
		if !permanently {
			return nil
		}
		if err := s.objects.DeleteObject(ctx, storage.BlobScope, blobObjectKey(workspaceID, key)); err != nil {
			return err
		}
		_, err = s.pool.Exec(ctx, `DELETE FROM blobs WHERE workspace_id = $1 AND key = $2 AND reservation_id = $3::uuid AND deleted_at IS NOT NULL`,
			workspaceID, key, reservationID)
		return err
	})
	return deleted, err
}

func (s *Service) ReleaseDeletedBatch(ctx context.Context, actorID, workspaceID string, limit int) (int, bool, error) {
	if limit < 1 || limit > maxManagedBlobs {
		return 0, false, errors.New("release batch limit must be between 1 and 1000")
	}
	if err := s.requireManager(ctx, s.pool, actorID, workspaceID); err != nil {
		return 0, false, err
	}
	rows, err := s.pool.Query(ctx, `SELECT key, reservation_id::text FROM blobs
		WHERE workspace_id = $1 AND deleted_at IS NOT NULL ORDER BY deleted_at, key LIMIT $2`, workspaceID, limit+1)
	if err != nil {
		return 0, false, err
	}
	type candidate struct{ key, reservationID string }
	candidates, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (candidate, error) {
		var c candidate
		err := row.Scan(&c.key, &c.reservationID)
		return c, err
	})
	if err != nil {
		return 0, false, err
	}
	hasMore := len(candidates) > limit
	if hasMore {
		candidates = candidates[:limit]
	}
	released := 0
	for _, c := range candidates {
		err := s.withObjectLock(ctx, workspaceID, blobObjectKey(workspaceID, c.key), func() error {
			row, err := loadBlobRow(ctx, s.pool, workspaceID, c.key, false)
			if err != nil || row == nil || !row.deleted || row.reservationID != c.reservationID {
				return err
			}
			if row.status == "pending" {
				s.discardStaging(ctx, workspaceID, c.key, row)
			}
			if err := s.objects.DeleteObject(ctx, storage.BlobScope, blobObjectKey(workspaceID, c.key)); err != nil {
				return err
			}
			tag, err := s.pool.Exec(ctx, `DELETE FROM blobs WHERE workspace_id = $1 AND key = $2 AND reservation_id = $3::uuid AND deleted_at IS NOT NULL`,
				workspaceID, c.key, c.reservationID)
			if err == nil && tag.RowsAffected() > 0 {
				released++
			}
			return err
		})
		if err != nil {
			if released > 0 {
				s.publishWorkspace(ctx, workspaceID)
			}
			return released, hasMore, err
		}
	}
	if released > 0 {
		s.publishWorkspace(ctx, workspaceID)
	}
	return released, hasMore, nil
}
