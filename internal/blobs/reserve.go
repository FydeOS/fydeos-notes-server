// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package blobs

import (
	"context"
	"errors"
	"math"
	"strings"

	"github.com/fydeos/fydeos-notes-server/internal/storage"
	"github.com/jackc/pgx/v5"
)

type reserveOutcome int

const (
	outcomeCreated reserveOutcome = iota
	outcomeResumed
	outcomeReplaced
	outcomeRepaired
	outcomeAlreadyUploaded
)

type reservation struct {
	id       string
	outcome  reserveOutcome
	uploadID *string
}

// blobRow is a snapshot of a blobs ledger row.
type blobRow struct {
	size          int64
	mime          string
	status        string
	deleted       bool
	reservationID string
	live          bool // pending and not expired
	uploadID      *string
}

func loadBlobRow(ctx context.Context, q queryer, workspaceID, key string, forUpdate bool) (*blobRow, error) {
	query := `SELECT size, mime, status::text, deleted_at IS NOT NULL, reservation_id::text,
		COALESCE(reservation_expires_at > clock_timestamp(), false), upload_id
		FROM blobs WHERE workspace_id = $1 AND key = $2`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	var row blobRow
	var size int32
	err := q.QueryRow(ctx, query, workspaceID, key).Scan(&size, &row.mime, &row.status, &row.deleted, &row.reservationID, &row.live, &row.uploadID)
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	row.size = int64(size)
	return &row, nil
}

func normalizeMime(mime string) string {
	if mime == "" {
		return "application/octet-stream"
	}
	return mime
}

func validateBlobInput(actorID, workspaceID, key string, size int64) error {
	if size > math.MaxInt32 {
		return errSizeRange
	}
	if size <= 0 || actorID == "" || workspaceID == "" || key == "" || strings.Contains(key, "/") {
		return errInvalidReservation
	}
	if storage.ValidateObjectKey(blobObjectKey(workspaceID, key)) != nil {
		return errInvalidReservation
	}
	return nil
}

// reserve creates or reuses a reservation. The caller holds the object lock.
func (s *Service) reserve(ctx context.Context, actorID, workspaceID, key string, size int64, mime string) (reservation, error) {
	for attempt := 0; attempt < 3; attempt++ {
		res, retry, err := s.reserveOnce(ctx, actorID, workspaceID, key, size, mime)
		if err != nil || !retry {
			return res, err
		}
	}
	return reservation{}, errReservationChanged
}

func (s *Service) reserveOnce(ctx context.Context, actorID, workspaceID, key string, size int64, mime string) (reservation, bool, error) {
	var res reservation
	var purge *blobRow
	var mismatch bool
	var ownerID string
	changed := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := requireUploader(ctx, tx, actorID, workspaceID); err != nil {
			return err
		}
		quota, err := s.lockQuota(ctx, tx, workspaceID)
		if err != nil {
			return err
		}
		ownerID = quota.ownerID
		row, err := loadBlobRow(ctx, tx, workspaceID, key, true)
		if err != nil {
			return err
		}
		expiry := quota.now.Add(reservationTTL)
		if row == nil {
			if err := quota.admit(size, size); err != nil {
				return err
			}
			res.outcome = outcomeCreated
			changed = true
			return tx.QueryRow(ctx, `INSERT INTO blobs (workspace_id, key, size, mime, status, reservation_id, reservation_expires_at)
				VALUES ($1, $2, $3, $4, 'pending', gen_random_uuid(), $5) RETURNING reservation_id::text`,
				workspaceID, key, size, mime, expiry).Scan(&res.id)
		}
		switch row.status {
		case "pending":
			if row.size != size {
				return errSizeMismatch
			}
			if row.mime != mime {
				return errMimeMismatch
			}
			if row.live && !row.deleted {
				res = reservation{id: row.reservationID, outcome: outcomeResumed, uploadID: row.uploadID}
				return nil
			}
			if err := quota.admit(size, size); err != nil {
				return err
			}
			if row.deleted {
				purge = row
				return nil
			}
			res.outcome = outcomeReplaced
			changed = true
			purge = row
			return tx.QueryRow(ctx, `UPDATE blobs SET reservation_id = gen_random_uuid(), reservation_expires_at = $3,
				upload_id = NULL, deleted_at = NULL WHERE workspace_id = $1 AND key = $2 RETURNING reservation_id::text`,
				workspaceID, key, expiry).Scan(&res.id)
		case "completed":
			if row.deleted {
				return errKeyDeleted
			}
			if row.size != size {
				return errSizeMismatch
			}
			if row.mime != mime {
				return errMimeMismatch
			}
			head, err := s.objects.HeadObject(ctx, storage.BlobScope, blobObjectKey(workspaceID, key))
			if err != nil {
				return err
			}
			if head != nil {
				if head.ContentLength == size && head.ContentType == mime {
					res = reservation{id: row.reservationID, outcome: outcomeAlreadyUploaded}
					return nil
				}
				mismatch = true
				_, err := tx.Exec(ctx, `UPDATE blobs SET deleted_at = COALESCE(deleted_at, clock_timestamp()), reservation_expires_at = NULL
					WHERE workspace_id = $1 AND key = $2`, workspaceID, key)
				return err
			}
			if err := quota.admit(size, 0); err != nil {
				return err
			}
			res.outcome = outcomeRepaired
			return tx.QueryRow(ctx, `UPDATE blobs SET status = 'pending', reservation_id = gen_random_uuid(),
				reservation_expires_at = $3, upload_id = NULL WHERE workspace_id = $1 AND key = $2 RETURNING reservation_id::text`,
				workspaceID, key, expiry).Scan(&res.id)
		default:
			return errNotReusable
		}
	})
	if err != nil {
		return reservation{}, false, err
	}
	if mismatch {
		delErr := s.objects.DeleteObject(ctx, storage.BlobScope, blobObjectKey(workspaceID, key))
		s.publish(workspaceID, ownerID)
		return reservation{}, false, errors.Join(errMetadataMismatch, delErr)
	}
	if purge != nil {
		s.discardStaging(ctx, workspaceID, key, purge)
		if res.id == "" {
			// Tombstoned pending row: remove it and retry as a fresh create.
			if err := s.objects.DeleteObject(ctx, storage.BlobScope, blobObjectKey(workspaceID, key)); err != nil {
				return reservation{}, false, err
			}
			if _, err := s.pool.Exec(ctx, `DELETE FROM blobs WHERE workspace_id = $1 AND key = $2 AND reservation_id = $3::uuid`,
				workspaceID, key, purge.reservationID); err != nil {
				return reservation{}, false, err
			}
			return reservation{}, true, nil
		}
	}
	if changed {
		s.publish(workspaceID, ownerID)
	}
	return res, false, nil
}

// discardStaging best-effort removes the staging object and multipart upload
// of an abandoned reservation.
func (s *Service) discardStaging(ctx context.Context, workspaceID, key string, row *blobRow) {
	staging := blobStagingKey(workspaceID, row.reservationID, key)
	if row.uploadID != nil && *row.uploadID != "" {
		_ = s.objects.AbortMultipartUpload(ctx, storage.BlobScope, staging, *row.uploadID)
	}
	_ = s.objects.DeleteObject(ctx, storage.BlobScope, staging)
}

// tombstoneReservation marks a pending reservation deleted and removes its objects.
func (s *Service) tombstoneReservation(ctx context.Context, workspaceID, key, reservationID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE blobs SET deleted_at = COALESCE(deleted_at, clock_timestamp()), reservation_expires_at = NULL
		WHERE workspace_id = $1 AND key = $2 AND reservation_id = $3::uuid AND status = 'pending'`, workspaceID, key, reservationID)
	err = errors.Join(err,
		s.objects.DeleteObject(ctx, storage.BlobScope, blobStagingKey(workspaceID, reservationID, key)),
		s.objects.DeleteObject(ctx, storage.BlobScope, blobObjectKey(workspaceID, key)))
	s.publishWorkspace(ctx, workspaceID)
	return err
}

// publishWorkspace publishes an invalidation, looking up the owner.
func (s *Service) publishWorkspace(ctx context.Context, workspaceID string) {
	if s.publisher == nil {
		return
	}
	owner, _ := activeOwner(ctx, s.pool, workspaceID)
	s.publish(workspaceID, owner)
}
