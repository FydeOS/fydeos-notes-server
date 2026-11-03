// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

// Package blobs stores workspace blobs and comment attachments, enforces
// storage quotas, and serves blob manifests for documents.
package blobs

import (
	"errors"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/storage"
)

// MaxCommentAttachmentSize is the largest accepted comment attachment.
const MaxCommentAttachmentSize int64 = 10 << 20

// Blob is a completed, live workspace blob.
type Blob struct {
	Key       string
	MIME      string
	Size      int32
	CreatedAt time.Time
}

// UploadMethod tells the client how to send blob bytes.
type UploadMethod string

const (
	UploadMethodGraphQL   UploadMethod = "GRAPHQL"
	UploadMethodPresigned UploadMethod = "PRESIGNED"
	UploadMethodMultipart UploadMethod = "MULTIPART"
)

// UploadInit describes how the client should upload a reserved blob.
type UploadInit struct {
	Method          UploadMethod
	BlobKey         string
	AlreadyUploaded *bool
	UploadURL       *string
	Headers         map[string]string
	ExpiresAt       *time.Time
	UploadID        *string
	PartSize        *int32
	UploadedParts   []storage.MultipartPart
}

// UploadPart is a URL for one multipart part.
type UploadPart struct {
	UploadURL string
	Headers   map[string]string
	ExpiresAt time.Time
}

// BlobSource identifies the document state that references blobs.
type BlobSource struct {
	Type        string `json:"type"`
	WorkspaceID string `json:"workspaceId"`
	DocID       string `json:"docId"`
	TimestampMS *int64 `json:"timestampMs,omitempty"`
}

type BlobManifestEntry struct {
	Key    string     `json:"key"`
	MIME   string     `json:"mime"`
	Size   int64      `json:"size"`
	Source BlobSource `json:"source"`
}

type BlobManifest struct {
	Version    int                 `json:"version"`
	Entries    []BlobManifestEntry `json:"entries"`
	NextCursor string              `json:"nextCursor,omitempty"`
}

type BlobReadMetadata struct {
	MIME         string
	Size         int64
	LastModified time.Time
}

// Errors whose text reaches clients.
var (
	ErrBlobUploadInvalid              = errors.New("invalid blob upload")
	ErrCommentAttachmentQuotaExceeded = errors.New("comment_attachment_quota_exceeded")

	errUploadForbidden        = errors.New("workspace_upload_forbidden")
	errBlobLimit              = errors.New("blob_limit")
	errStorageLimit           = errors.New("storage_limit")
	errSizeRange              = errors.New("blob size exceeds database range")
	errBlobNotFound           = errors.New("Blob not found")
	errReservationChanged     = errors.New("Blob reservation changed")
	errKeyMismatch            = errors.New("Blob key mismatch")
	errInvalidReservation     = errors.New("invalid storage reservation")
	errKeyDeleted             = errors.New("storage key is deleted")
	errSizeMismatch           = errors.New("blob size mismatch")
	errMimeMismatch           = errors.New("blob mime mismatch")
	errNotReusable            = errors.New("storage key is not reusable")
	errMetadataMismatch       = errors.New("Blob metadata mismatch")
	errWorkspaceNotFound      = errors.New("workspace not found")
	errManageForbidden        = errors.New("workspace_blobs_manage_forbidden")
	errInventoryTooLarge      = errors.New("managed_blob_inventory_too_large")
	errLockTimeout            = errors.New("storage_lifecycle_lock_timeout")
	errQuotaSubjectChanged    = errors.New("storage_quota_subject_changed")
	errCommentCreateForbidden = errors.New("document_comments_create_forbidden")
)

// Manifest errors.
var (
	ErrManifestInvalidInput        = errors.New("invalid blob manifest input")
	ErrManifestCursor              = errors.New("invalid blob manifest cursor")
	ErrManifestNotFound            = errors.New("blob manifest source not found")
	ErrManifestAccessDenied        = errors.New("blob manifest access denied")
	ErrManifestWorkspaceSyncDenied = errors.New("blob manifest workspace sync denied")
	ErrManifestBlobNotReferenced   = errors.New("blob is not referenced by source")
	ErrManifestBlobNotCompleted    = errors.New("blob upload is not completed")
	ErrManifestBlobObjectNotFound  = errors.New("blob object not found")

	errManifestProjection    = errors.New("blob manifest projection failed")
	errManifestTooLarge      = errors.New("blob manifest source too large")
	errManifestSourceChanged = errors.New("blob source changed")
	errManifestStreamLimit   = errors.New("too many open blob streams")
	errManifestStreamExpired = errors.New("blob stream expired")
	errManifestSizeMismatch  = errors.New("blob object size mismatch")
	errManifestNoStorage     = errors.New("blob storage is not configured")
)
