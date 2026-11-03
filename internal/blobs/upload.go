// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package blobs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/storage"
	"github.com/jackc/pgx/v5"
)

const (
	proxyUploadPath    = "/api/storage/upload"
	proxyMultipartPath = "/api/storage/multipart"
)

func (s *Service) CreateBlobUpload(ctx context.Context, actorID, workspaceID, key string, size int64, mime string) (UploadInit, error) {
	if err := validateBlobInput(actorID, workspaceID, key, size); err != nil {
		return UploadInit{}, err
	}
	mime = normalizeMime(mime)
	var init UploadInit
	err := s.withObjectLock(ctx, workspaceID, blobObjectKey(workspaceID, key), func() error {
		res, err := s.reserve(ctx, actorID, workspaceID, key, size, mime)
		if err != nil {
			return err
		}
		if res.outcome == outcomeAlreadyUploaded {
			done := true
			init = UploadInit{Method: UploadMethodGraphQL, BlobKey: key, AlreadyUploaded: &done}
			return nil
		}
		var created string
		init, created = s.chooseUploadMethod(ctx, workspaceID, key, size, mime, res)
		var recorded *string
		if init.UploadID != nil {
			recorded = init.UploadID
		}
		tag, err := s.pool.Exec(ctx, `UPDATE blobs SET upload_id = $4 WHERE workspace_id = $1 AND key = $2
			AND reservation_id = $3::uuid AND status = 'pending' AND deleted_at IS NULL
			AND reservation_expires_at > clock_timestamp()`, workspaceID, key, res.id, recorded)
		if err == nil && tag.RowsAffected() == 0 {
			err = errReservationChanged
		}
		if err != nil {
			if created != "" {
				_ = s.objects.AbortMultipartUpload(ctx, storage.BlobScope, blobStagingKey(workspaceID, res.id, key), created)
			}
			return err
		}
		return nil
	})
	return init, err
}

// chooseUploadMethod picks the upload path; it returns the id of a newly
// created multipart upload, if any.
func (s *Service) chooseUploadMethod(ctx context.Context, workspaceID, key string, size int64, mime string, res reservation) (UploadInit, string) {
	init := UploadInit{Method: UploadMethodGraphQL, BlobKey: key}
	settings, ok := s.objects.UploadURLSettings(storage.BlobScope)
	if !ok {
		return init, ""
	}
	staging := blobStagingKey(workspaceID, res.id, key)
	partSize := int32(multipartPart)
	if res.uploadID != nil && *res.uploadID != "" {
		if parts, err := s.objects.ListMultipartUploadParts(ctx, storage.BlobScope, staging, *res.uploadID); err == nil {
			if parts == nil {
				parts = []storage.MultipartPart{}
			}
			id := *res.uploadID
			init.Method, init.UploadID, init.PartSize, init.UploadedParts = UploadMethodMultipart, &id, &partSize, parts
			return init, ""
		}
	}
	if size >= multipartMinimum {
		upload, err := s.objects.CreateMultipartUpload(ctx, storage.BlobScope, staging, storage.PutOptions{ContentType: mime}, settings.PresignTTL)
		if err == nil {
			id := upload.UploadID
			expires := upload.ExpiresAt
			init.Method, init.UploadID, init.PartSize, init.UploadedParts = UploadMethodMultipart, &id, &partSize, []storage.MultipartPart{}
			init.ExpiresAt = &expires
			return init, id
		}
	}
	if settings.ProxyUpload {
		expires := time.Now().Add(reservationTTL)
		token, signed, err := s.objects.SignUploadToken(storage.BlobScope, proxyUploadPath, []any{workspaceID, key, mime, size}, expires.Unix())
		if err != nil || !signed {
			return init, ""
		}
		link, err := storage.BuildProxyUploadURL(s.proxyPrefix(settings), proxyUploadPath, map[string]string{
			"workspaceId": workspaceID, "key": key, "contentType": mime,
			"contentLength": strconv.FormatInt(size, 10), "expiresAt": strconv.FormatInt(expires.Unix(), 10), "token": token,
		})
		if err != nil {
			return init, ""
		}
		init.Method, init.UploadURL, init.ExpiresAt = UploadMethodPresigned, &link, &expires
		return init, ""
	}
	request, err := s.objects.PresignPut(ctx, storage.BlobScope, staging, storage.PutOptions{ContentType: mime, ContentLength: &size}, settings.PresignTTL)
	if err != nil {
		return init, ""
	}
	if request, err = storage.ApplyPresignedURLPrefix(request, settings.URLPrefix); err != nil {
		return init, ""
	}
	link, expires := request.URL, request.ExpiresAt
	init.Method, init.UploadURL, init.ExpiresAt, init.Headers = UploadMethodPresigned, &link, &expires, flattenHeaders(request.Headers)
	return init, ""
}

func (s *Service) proxyPrefix(settings storage.UploadURLSettings) string {
	if settings.URLPrefix != "" {
		return settings.URLPrefix
	}
	return s.publicBaseURL
}

func flattenHeaders(headers map[string][]string) map[string]string {
	out := make(map[string]string, len(headers))
	for name, values := range headers {
		out[name] = strings.Join(values, ",")
	}
	return out
}

func (s *Service) BlobUploadPartURL(ctx context.Context, actorID, workspaceID, key, uploadID string, partNumber int32) (UploadPart, error) {
	if actorID == "" || workspaceID == "" || key == "" || strings.Contains(key, "/") || uploadID == "" || partNumber < 1 || partNumber > maxPartNumber {
		return UploadPart{}, ErrBlobUploadInvalid
	}
	if err := requireUploader(ctx, s.pool, actorID, workspaceID); err != nil {
		return UploadPart{}, err
	}
	row, err := loadBlobRow(ctx, s.pool, workspaceID, key, false)
	if err != nil {
		return UploadPart{}, err
	}
	if row == nil || row.status != "pending" || row.deleted || row.uploadID == nil || *row.uploadID != uploadID {
		return UploadPart{}, ErrBlobUploadInvalid
	}
	offset := int64(partNumber-1) * multipartPart
	if offset >= row.size {
		return UploadPart{}, ErrBlobUploadInvalid
	}
	length := min(int64(multipartPart), row.size-offset)
	settings, ok := s.objects.UploadURLSettings(storage.BlobScope)
	if !ok {
		return UploadPart{}, ErrBlobUploadInvalid
	}
	if settings.ProxyUpload {
		expires := time.Now().Add(reservationTTL)
		token, signed, err := s.objects.SignUploadToken(storage.BlobScope, proxyMultipartPath,
			[]any{workspaceID, key, uploadID, partNumber, length}, expires.Unix())
		if err != nil || !signed {
			return UploadPart{}, ErrBlobUploadInvalid
		}
		link, err := storage.BuildProxyUploadURL(s.proxyPrefix(settings), proxyMultipartPath, map[string]string{
			"workspaceId": workspaceID, "key": key, "uploadId": uploadID,
			"partNumber": strconv.FormatInt(int64(partNumber), 10), "contentLength": strconv.FormatInt(length, 10),
			"expiresAt": strconv.FormatInt(expires.Unix(), 10), "token": token,
		})
		if err != nil {
			return UploadPart{}, ErrBlobUploadInvalid
		}
		return UploadPart{UploadURL: link, ExpiresAt: expires}, nil
	}
	request, err := s.objects.PresignUploadPart(ctx, storage.BlobScope, blobStagingKey(workspaceID, row.reservationID, key), uploadID, partNumber, length, settings.PresignTTL)
	if err != nil {
		if errors.Is(err, storage.ErrOperationUnsupported) {
			return UploadPart{}, ErrBlobUploadInvalid
		}
		return UploadPart{}, err
	}
	if request, err = storage.ApplyPresignedURLPrefix(request, settings.URLPrefix); err != nil {
		return UploadPart{}, ErrBlobUploadInvalid
	}
	return UploadPart{UploadURL: request.URL, Headers: flattenHeaders(request.Headers), ExpiresAt: request.ExpiresAt}, nil
}

func (s *Service) SetBlob(ctx context.Context, actorID, workspaceID, key, mime string, body io.Reader) (string, error) {
	if body == nil {
		return "", errInvalidReservation
	}
	data, err := io.ReadAll(io.LimitReader(body, math.MaxInt32+1))
	if err != nil {
		return "", err
	}
	size := int64(len(data))
	if err := validateBlobInput(actorID, workspaceID, key, size); err != nil {
		return "", err
	}
	mime = normalizeMime(mime)
	err = s.withObjectLock(ctx, workspaceID, blobObjectKey(workspaceID, key), func() error {
		res, err := s.reserve(ctx, actorID, workspaceID, key, size, mime)
		if err != nil {
			return err
		}
		if res.outcome == outcomeAlreadyUploaded {
			return nil
		}
		staging := blobStagingKey(workspaceID, res.id, key)
		if _, err := s.objects.PutObject(ctx, storage.BlobScope, staging, bytes.NewReader(data), storage.PutOptions{ContentType: mime, ContentLength: &size}); err != nil {
			return errors.Join(err, s.tombstoneReservation(ctx, workspaceID, key, res.id))
		}
		return s.finalize(ctx, actorID, workspaceID, key, res.id)
	})
	if err != nil {
		return "", err
	}
	return key, nil
}

func (s *Service) CompleteBlobUpload(ctx context.Context, actorID, workspaceID, key string, uploadID *string, parts []storage.CompletedPart) (string, error) {
	if actorID == "" || workspaceID == "" || key == "" || strings.Contains(key, "/") {
		return "", ErrBlobUploadInvalid
	}
	if err := requireUploader(ctx, s.pool, actorID, workspaceID); err != nil {
		return "", err
	}
	err := s.withObjectLock(ctx, workspaceID, blobObjectKey(workspaceID, key), func() error {
		row, err := loadBlobRow(ctx, s.pool, workspaceID, key, false)
		if err != nil {
			return err
		}
		if row == nil {
			return errBlobNotFound
		}
		if row.status == "completed" && !row.deleted {
			return nil
		}
		if row.status != "pending" || row.deleted || !row.live {
			return ErrBlobUploadInvalid
		}
		hasID := uploadID != nil && *uploadID != ""
		if row.uploadID != nil && *row.uploadID != "" {
			if !hasID || *uploadID != *row.uploadID || len(parts) == 0 {
				return ErrBlobUploadInvalid
			}
			staging := blobStagingKey(workspaceID, row.reservationID, key)
			head, err := s.objects.HeadObject(ctx, storage.BlobScope, staging)
			if err != nil {
				return err
			}
			if head == nil {
				uploaded, err := s.objects.ListMultipartUploadParts(ctx, storage.BlobScope, staging, *row.uploadID)
				if err != nil {
					return err
				}
				if err := validateParts(parts, uploaded, row.size); err != nil {
					return err
				}
				if err := s.objects.CompleteMultipartUpload(ctx, storage.BlobScope, staging, *row.uploadID, parts); err != nil {
					return err
				}
			}
		} else if hasID || len(parts) > 0 {
			return ErrBlobUploadInvalid
		}
		return s.finalize(ctx, actorID, workspaceID, key, row.reservationID)
	})
	if err != nil {
		return "", err
	}
	return key, nil
}

func validateParts(requested []storage.CompletedPart, uploaded []storage.MultipartPart, size int64) error {
	byNumber := make(map[int32]storage.MultipartPart, len(uploaded))
	for _, part := range uploaded {
		if part.PartNumber < 1 || part.PartNumber > maxPartNumber || part.Size < 0 {
			return errMetadataMismatch
		}
		if _, dup := byNumber[part.PartNumber]; dup {
			return errMetadataMismatch
		}
		byNumber[part.PartNumber] = part
	}
	seen := make(map[int32]bool, len(requested))
	var total int64
	for _, part := range requested {
		if seen[part.PartNumber] {
			return errMetadataMismatch
		}
		seen[part.PartNumber] = true
		have, ok := byNumber[part.PartNumber]
		if !ok || strings.Trim(have.ETag, `"`) != strings.Trim(part.ETag, `"`) {
			return errMetadataMismatch
		}
		total += have.Size
		if total > size {
			return errMetadataMismatch
		}
	}
	if total != size {
		return errMetadataMismatch
	}
	return nil
}

// readObject reads an object bounded to limit+1 bytes; it returns nil data
// when the object does not exist.
func (s *Service) readObject(ctx context.Context, objectKey string, limit int64) ([]byte, *storage.ObjectMetadata, error) {
	body, meta, err := s.objects.GetObject(ctx, storage.BlobScope, objectKey)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, nil, err
	}
	return data, &meta, nil
}

// finalize verifies the uploaded bytes and marks the reservation completed.
// The caller holds the object lock.
func (s *Service) finalize(ctx context.Context, actorID, workspaceID, key, reservationID string) error {
	row, err := loadBlobRow(ctx, s.pool, workspaceID, key, false)
	if err != nil {
		return err
	}
	if row == nil || row.reservationID != reservationID || row.status != "pending" || row.deleted || !row.live {
		return errReservationChanged
	}
	stagingKey := blobStagingKey(workspaceID, reservationID, key)
	finalKey := blobObjectKey(workspaceID, key)
	fail := func(cause error) error {
		return errors.Join(cause, s.tombstoneReservation(ctx, workspaceID, key, reservationID))
	}
	check := func(data []byte, meta *storage.ObjectMetadata) error {
		if int64(len(data)) != row.size || meta.ContentType != row.mime {
			return errMetadataMismatch
		}
		if !keyMatches(key, data) {
			return errKeyMismatch
		}
		return nil
	}
	staged, stagedMeta, err := s.readObject(ctx, stagingKey, row.size)
	if err != nil {
		return err
	}
	final, finalMeta, err := s.readObject(ctx, finalKey, row.size)
	if err != nil {
		return err
	}
	if stagedMeta == nil && finalMeta == nil {
		return errBlobNotFound
	}
	if stagedMeta != nil {
		if err := check(staged, stagedMeta); err != nil {
			return fail(err)
		}
	}
	if finalMeta != nil {
		if err := check(final, finalMeta); err != nil {
			return fail(err)
		}
		if stagedMeta != nil && !bytes.Equal(staged, final) {
			return fail(errMetadataMismatch)
		}
	} else {
		size := row.size
		written, err := s.objects.PutObject(ctx, storage.BlobScope, finalKey, bytes.NewReader(staged), storage.PutOptions{ContentType: row.mime, ContentLength: &size})
		if err != nil {
			return fail(err)
		}
		if written.ContentLength != row.size || written.ContentType != row.mime {
			return fail(errMetadataMismatch)
		}
	}
	var ownerID string
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if err := requireUploader(ctx, tx, actorID, workspaceID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE blobs SET status = 'completed', reservation_expires_at = NULL, upload_id = NULL
			WHERE workspace_id = $1 AND key = $2 AND reservation_id = $3::uuid AND status = 'pending'
			AND deleted_at IS NULL AND reservation_expires_at > clock_timestamp() AND size = $4 AND mime = $5`,
			workspaceID, key, reservationID, row.size, row.mime)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errReservationChanged
		}
		ownerID, err = activeOwner(ctx, tx, workspaceID)
		return err
	})
	if err != nil {
		return err
	}
	if stagedMeta != nil {
		_ = s.objects.DeleteObject(ctx, storage.BlobScope, stagingKey)
	}
	s.publish(workspaceID, ownerID)
	return nil
}

func (s *Service) AbortBlobUpload(ctx context.Context, actorID, workspaceID, key, uploadID string) (bool, error) {
	if actorID == "" || workspaceID == "" || key == "" || strings.Contains(key, "/") || uploadID == "" {
		return false, ErrBlobUploadInvalid
	}
	if err := requireUploader(ctx, s.pool, actorID, workspaceID); err != nil {
		return false, err
	}
	aborted := false
	err := s.withObjectLock(ctx, workspaceID, blobObjectKey(workspaceID, key), func() error {
		row, err := loadBlobRow(ctx, s.pool, workspaceID, key, false)
		if err != nil {
			return err
		}
		if row == nil || row.status != "pending" || row.deleted || row.uploadID == nil || *row.uploadID != uploadID {
			return nil
		}
		staging := blobStagingKey(workspaceID, row.reservationID, key)
		err = s.objects.AbortMultipartUpload(ctx, storage.BlobScope, staging, uploadID)
		switch {
		case err == nil:
			aborted = true
		case errors.Is(err, storage.ErrOperationUnsupported):
		default:
			return err
		}
		if _, err := s.pool.Exec(ctx, `UPDATE blobs SET deleted_at = COALESCE(deleted_at, clock_timestamp()), reservation_expires_at = NULL
			WHERE workspace_id = $1 AND key = $2 AND reservation_id = $3::uuid`, workspaceID, key, row.reservationID); err != nil {
			return err
		}
		err = errors.Join(s.objects.DeleteObject(ctx, storage.BlobScope, staging),
			s.objects.DeleteObject(ctx, storage.BlobScope, blobObjectKey(workspaceID, key)))
		s.publishWorkspace(ctx, workspaceID)
		return err
	})
	return aborted, err
}
