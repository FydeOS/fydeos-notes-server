// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package blobs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/fydeos/fydeos-notes-server/internal/storage"
	"github.com/jackc/pgx/v5"
)

func sniffMime(data []byte) string {
	if len(data) == 0 {
		return "application/octet-stream"
	}
	detected := http.DetectContentType(data[:min(len(data), 512)])
	mediaType, _, err := mime.ParseMediaType(detected)
	if err != nil || mediaType == "" {
		return "application/octet-stream"
	}
	return mediaType
}

func (s *Service) authorizeComment(ctx context.Context, tx pgx.Tx, actorID, workspaceID, docID string) error {
	auth, err := s.commentAuth.AuthorizeDocumentInTx(ctx, tx, actorID, workspaceID, docID)
	if err != nil {
		return err
	}
	if !auth.CommentsCreate {
		return errCommentCreateForbidden
	}
	return nil
}

func (s *Service) UploadCommentAttachment(ctx context.Context, actorID, workspaceID, docID, key, name string, body io.Reader) (string, error) {
	if actorID == "" || workspaceID == "" || docID == "" || key == "" || body == nil || strings.Contains(key, "/") {
		return "", errInvalidReservation
	}
	if storage.ValidateObjectKey(commentObjectKey(workspaceID, docID, key)) != nil {
		return "", errInvalidReservation
	}
	if s.commentAuth == nil {
		return "", errInvalidReservation
	}
	if name == "" {
		name = key
	}
	data, err := io.ReadAll(io.LimitReader(body, MaxCommentAttachmentSize+1))
	if err != nil {
		return "", err
	}
	if int64(len(data)) > MaxCommentAttachmentSize {
		return "", ErrCommentAttachmentQuotaExceeded
	}
	size := int64(len(data))
	contentType := sniffMime(data)
	finalKey := commentObjectKey(workspaceID, docID, key)
	err = s.withObjectLock(ctx, workspaceID, finalKey, func() error {
		reservationID, err := s.reserveComment(ctx, actorID, workspaceID, docID, key, name, contentType, size)
		if err != nil {
			return err
		}
		stagingKey := commentStagingKey(workspaceID, docID, reservationID, key)
		if err := s.storeComment(ctx, actorID, workspaceID, docID, key, reservationID, contentType, data); err != nil {
			_, dbErr := s.pool.Exec(ctx, `UPDATE comment_attachments SET deleted_at = COALESCE(deleted_at, clock_timestamp()),
				reservation_expires_at = clock_timestamp()
				WHERE workspace_id = $1 AND doc_id = $2 AND key = $3 AND reservation_id = $4::uuid AND status = 'pending' AND created_by = $5`,
				workspaceID, docID, key, reservationID, actorID)
			cleanup := errors.Join(dbErr,
				s.objects.DeleteObject(ctx, storage.BlobScope, stagingKey),
				s.objects.DeleteObject(ctx, storage.BlobScope, finalKey))
			s.publishWorkspace(ctx, workspaceID)
			return errors.Join(err, cleanup)
		}
		if err := s.objects.DeleteObject(ctx, storage.BlobScope, stagingKey); err != nil {
			slog.Warn("delete comment attachment staging object", "workspace", workspaceID, "error", err)
		}
		s.publishWorkspace(ctx, workspaceID)
		return nil
	})
	if err != nil {
		return "", err
	}
	return strings.TrimRight(s.publicBaseURL, "/") + "/api/workspaces/" + url.PathEscape(workspaceID) + "/docs/" +
		url.PathEscape(docID) + "/comment-attachments/" + url.PathEscape(key), nil
}

func (s *Service) reserveComment(ctx context.Context, actorID, workspaceID, docID, key, name, contentType string, size int64) (string, error) {
	var reservationID string
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := s.authorizeComment(ctx, tx, actorID, workspaceID, docID); err != nil {
			return err
		}
		quota, err := s.lockQuota(ctx, tx, workspaceID)
		if err != nil {
			return err
		}
		if err := quota.admit(size, size); err != nil {
			return ErrCommentAttachmentQuotaExceeded
		}
		var active bool
		err = tx.QueryRow(ctx, `SELECT deleted_at IS NULL AND (status::text = 'completed' OR COALESCE(reservation_expires_at > clock_timestamp(), false))
			FROM comment_attachments WHERE workspace_id = $1 AND doc_id = $2 AND key = $3 FOR UPDATE`, workspaceID, docID, key).Scan(&active)
		switch {
		case isNoRows(err):
		case err != nil:
			return err
		case active:
			return errNotReusable
		default:
			if _, err := tx.Exec(ctx, `DELETE FROM comment_attachments WHERE workspace_id = $1 AND doc_id = $2 AND key = $3`, workspaceID, docID, key); err != nil {
				return err
			}
		}
		return tx.QueryRow(ctx, `INSERT INTO comment_attachments
			(workspace_id, doc_id, key, size, mime, name, created_by, status, reservation_id, reservation_expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'pending', gen_random_uuid(), $8) RETURNING reservation_id::text`,
			workspaceID, docID, key, size, contentType, name, actorID, quota.now.Add(reservationTTL)).Scan(&reservationID)
	})
	return reservationID, err
}

func (s *Service) storeComment(ctx context.Context, actorID, workspaceID, docID, key, reservationID, contentType string, data []byte) error {
	size := int64(len(data))
	verify := func(meta storage.ObjectMetadata) error {
		if meta.ContentLength != size || meta.ContentType != contentType {
			return errMetadataMismatch
		}
		return nil
	}
	put := func(objectKey string) error {
		meta, err := s.objects.PutObject(ctx, storage.BlobScope, objectKey, bytes.NewReader(data), storage.PutOptions{ContentType: contentType, ContentLength: &size})
		if err != nil {
			return err
		}
		return verify(meta)
	}
	if err := put(commentStagingKey(workspaceID, docID, reservationID, key)); err != nil {
		return err
	}
	if err := put(commentObjectKey(workspaceID, docID, key)); err != nil {
		return err
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if err := s.authorizeComment(ctx, tx, actorID, workspaceID, docID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE comment_attachments SET status = 'completed', reservation_expires_at = NULL
			WHERE workspace_id = $1 AND doc_id = $2 AND key = $3 AND reservation_id = $4::uuid AND status = 'pending'
			AND deleted_at IS NULL AND created_by = $5 AND size = $6 AND mime = $7 AND reservation_expires_at > clock_timestamp()`,
			workspaceID, docID, key, reservationID, actorID, size, contentType)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errReservationChanged
		}
		return nil
	})
}

func (s *Service) GetCommentAttachment(ctx context.Context, workspaceID, docID, key string) (io.ReadCloser, storage.ObjectMetadata, error) {
	if workspaceID == "" || docID == "" || key == "" || strings.Contains(key, "/") {
		return nil, storage.ObjectMetadata{}, fs.ErrNotExist
	}
	objectKey := commentObjectKey(workspaceID, docID, key)
	if storage.ValidateObjectKey(objectKey) != nil {
		return nil, storage.ObjectMetadata{}, fs.ErrNotExist
	}
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM comment_attachments WHERE workspace_id = $1 AND doc_id = $2
		AND key = $3 AND status = 'completed' AND deleted_at IS NULL)`, workspaceID, docID, key).Scan(&exists); err != nil {
		return nil, storage.ObjectMetadata{}, err
	}
	if !exists {
		return nil, storage.ObjectMetadata{}, fs.ErrNotExist
	}
	return s.objects.GetObject(ctx, storage.BlobScope, objectKey)
}
