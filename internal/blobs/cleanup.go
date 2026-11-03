// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package blobs

import (
	"context"
	"errors"
	"fmt"

	"github.com/fydeos/fydeos-notes-server/internal/storage"
	"github.com/jackc/pgx/v5"
)

const candidateCondition = `(deleted_at IS NOT NULL OR (status::text = 'pending'
	AND (reservation_expires_at IS NULL OR reservation_expires_at <= clock_timestamp())))`

type cleanupCandidate struct {
	comment       bool
	workspaceID   string
	docID         string
	key           string
	reservationID string
}

func (c cleanupCandidate) finalKey() string {
	if c.comment {
		return commentObjectKey(c.workspaceID, c.docID, c.key)
	}
	return blobObjectKey(c.workspaceID, c.key)
}

func (c cleanupCandidate) stagingKey() string {
	if c.comment {
		return commentStagingKey(c.workspaceID, c.docID, c.reservationID, c.key)
	}
	return blobStagingKey(c.workspaceID, c.reservationID, c.key)
}

// CleanupExpiredStorageReservationsV1 removes up to limit tombstoned or
// expired reservations and their objects, returning the rows deleted.
func (s *Service) CleanupExpiredStorageReservationsV1(ctx context.Context, limit int64) (int64, error) {
	if limit <= 0 {
		return 0, errors.New("cleanup limit must be positive")
	}
	rows, err := s.pool.Query(ctx, `SELECT * FROM (
		(SELECT false, workspace_id, '', key, reservation_id::text FROM blobs WHERE `+candidateCondition+` LIMIT $1)
		UNION ALL
		(SELECT true, workspace_id, doc_id, key, reservation_id::text FROM comment_attachments WHERE `+candidateCondition+` LIMIT $1)
	) AS candidates LIMIT $1`, limit)
	if err != nil {
		return 0, err
	}
	candidates, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (cleanupCandidate, error) {
		var c cleanupCandidate
		err := row.Scan(&c.comment, &c.workspaceID, &c.docID, &c.key, &c.reservationID)
		return c, err
	})
	if err != nil {
		return 0, err
	}
	var deleted int64
	var storageErrs []error
	touched := map[string]bool{}
	for _, c := range candidates {
		var storageErr error
		err := s.withObjectLock(ctx, c.workspaceID, c.finalKey(), func() error {
			removed, objErr, err := s.cleanupOne(ctx, c)
			if removed {
				deleted++
				touched[c.workspaceID] = true
			}
			storageErr = objErr
			return err
		})
		if err != nil {
			s.publishTouched(ctx, touched)
			return deleted, err
		}
		if storageErr != nil {
			storageErrs = append(storageErrs, fmt.Errorf("cleanup %s: %w", c.finalKey(), storageErr))
		}
	}
	s.publishTouched(ctx, touched)
	return deleted, errors.Join(storageErrs...)
}

func (s *Service) publishTouched(ctx context.Context, touched map[string]bool) {
	for workspaceID := range touched {
		s.publishWorkspace(ctx, workspaceID)
	}
}

// cleanupOne handles one candidate. Database errors are returned as err;
// object storage errors as objErr.
func (s *Service) cleanupOne(ctx context.Context, c cleanupCandidate) (removed bool, objErr error, err error) {
	var status string
	var uploadID *string
	if c.comment {
		err = s.pool.QueryRow(ctx, `UPDATE comment_attachments SET deleted_at = COALESCE(deleted_at, clock_timestamp())
			WHERE workspace_id = $1 AND doc_id = $2 AND key = $3 AND reservation_id = $4::uuid AND `+candidateCondition+`
			RETURNING status::text`, c.workspaceID, c.docID, c.key, c.reservationID).Scan(&status)
	} else {
		err = s.pool.QueryRow(ctx, `UPDATE blobs SET deleted_at = COALESCE(deleted_at, clock_timestamp())
			WHERE workspace_id = $1 AND key = $2 AND reservation_id = $3::uuid AND `+candidateCondition+`
			RETURNING status::text, upload_id`, c.workspaceID, c.key, c.reservationID).Scan(&status, &uploadID)
	}
	if isNoRows(err) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	pending := status == "pending"
	if pending && uploadID != nil && *uploadID != "" {
		if abortErr := s.objects.AbortMultipartUpload(ctx, storage.BlobScope, c.stagingKey(), *uploadID); abortErr != nil && !errors.Is(abortErr, storage.ErrOperationUnsupported) {
			return false, abortErr, nil
		}
	}
	if delErr := s.objects.DeleteObject(ctx, storage.BlobScope, c.finalKey()); delErr != nil {
		return false, delErr, nil
	}
	if pending {
		if delErr := s.objects.DeleteObject(ctx, storage.BlobScope, c.stagingKey()); delErr != nil {
			return false, delErr, nil
		}
	}
	var tag interface{ RowsAffected() int64 }
	if c.comment {
		tag, err = s.pool.Exec(ctx, `DELETE FROM comment_attachments WHERE workspace_id = $1 AND doc_id = $2 AND key = $3 AND reservation_id = $4::uuid`,
			c.workspaceID, c.docID, c.key, c.reservationID)
	} else {
		tag, err = s.pool.Exec(ctx, `DELETE FROM blobs WHERE workspace_id = $1 AND key = $2 AND reservation_id = $3::uuid`,
			c.workspaceID, c.key, c.reservationID)
	}
	if err != nil {
		return false, nil, err
	}
	return tag.RowsAffected() > 0, nil, nil
}
