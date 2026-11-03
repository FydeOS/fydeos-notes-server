// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package blobs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/storage"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
	"github.com/jackc/pgx/v5"
)

const (
	manifestVersion     = 1
	defaultManifestPage = 25
	maxManifestPage     = 100
	manifestDocBatch    = 32
	maxOpenStreams      = 512
	streamChunk         = 256 << 10
	streamIdleLimit     = 60 * time.Second
)

type manifestAuthorizer interface {
	AuthorizeDocumentInTx(context.Context, pgx.Tx, string, string, string) (workspaces.DocumentAuthorization, error)
	AuthorizeWorkspaceReadInTx(context.Context, pgx.Tx, string, string) (bool, error)
	AuthorizeWorkspaceSyncInTx(context.Context, pgx.Tx, string, string, []string) (workspaces.WorkspaceSyncAccess, error)
}

// ManifestService lists and streams blobs referenced by documents.
type ManifestService struct {
	pool    servicePool
	auth    manifestAuthorizer
	objects *storage.Runtime
	streams chan struct{}
	now     func() time.Time
}

func NewManifestService(pool servicePool, authorizer manifestAuthorizer, objectStorage ...*storage.Runtime) *ManifestService {
	s := &ManifestService{pool: pool, auth: authorizer, streams: make(chan struct{}, maxOpenStreams), now: time.Now}
	if len(objectStorage) > 0 {
		s.objects = objectStorage[0]
	}
	return s
}

func (s *ManifestService) readTx(ctx context.Context, fn func(pgx.Tx) error) error {
	return runTx(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead}, fn)
}

func mapAuthError(err error) error {
	if errors.Is(err, workspaces.ErrNotFound) {
		return errors.Join(ErrManifestNotFound, err)
	}
	return err
}

func (s *ManifestService) authorize(ctx context.Context, tx pgx.Tx, actorID string, source BlobSource) error {
	if source.DocID == source.WorkspaceID {
		ok, err := s.auth.AuthorizeWorkspaceReadInTx(ctx, tx, actorID, source.WorkspaceID)
		if err != nil {
			return mapAuthError(err)
		}
		if !ok {
			return ErrManifestAccessDenied
		}
		return nil
	}
	auth, err := s.auth.AuthorizeDocumentInTx(ctx, tx, actorID, source.WorkspaceID, source.DocID)
	if err != nil {
		return mapAuthError(err)
	}
	allowed := auth.Read
	if source.Type == "history" {
		allowed = auth.HistoryRead
	}
	if !allowed {
		return ErrManifestAccessDenied
	}
	return nil
}

type completedBlob struct {
	key  string
	mime string
	size int64
}

func completedBlobs(ctx context.Context, q queryer, workspaceID string, keys map[string]bool) ([]completedBlob, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	list := make([]string, 0, len(keys))
	for key := range keys {
		list = append(list, key)
	}
	rows, err := q.Query(ctx, `SELECT key, mime, size FROM blobs WHERE workspace_id = $1 AND key = ANY($2)
		AND status = 'completed' AND deleted_at IS NULL ORDER BY key`, workspaceID, list)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (completedBlob, error) {
		var b completedBlob
		err := row.Scan(&b.key, &b.mime, &b.size)
		return b, err
	})
}

func (s *ManifestService) DocManifest(ctx context.Context, actorID string, source BlobSource) (BlobManifest, error) {
	if err := validateSource(source); err != nil {
		return BlobManifest{}, err
	}
	manifest := BlobManifest{Version: manifestVersion, Entries: []BlobManifestEntry{}}
	err := s.readTx(ctx, func(tx pgx.Tx) error {
		if err := s.authorize(ctx, tx, actorID, source); err != nil {
			return err
		}
		loaded, err := loadSource(ctx, tx, source, true)
		if err != nil {
			return err
		}
		keys, err := referencedKeys(loaded.doc, source.DocID == source.WorkspaceID)
		if err != nil {
			return err
		}
		found, err := completedBlobs(ctx, tx, source.WorkspaceID, keys)
		if err != nil {
			return err
		}
		for _, b := range found {
			manifest.Entries = append(manifest.Entries, BlobManifestEntry{Key: b.key, MIME: b.mime, Size: b.size, Source: source})
		}
		return nil
	})
	if err != nil {
		return BlobManifest{}, err
	}
	return manifest, nil
}

type manifestCursor struct {
	DocID string `json:"docId"`
	Key   string `json:"key"`
}

func decodeCursor(raw string) (manifestCursor, error) {
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return manifestCursor{}, ErrManifestCursor
	}
	var c manifestCursor
	if err := json.Unmarshal(data, &c); err != nil || c.DocID == "" {
		return manifestCursor{}, ErrManifestCursor
	}
	return c, nil
}

func encodeCursor(c manifestCursor) string {
	data, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(data)
}

func (s *ManifestService) ReadableWorkspaceManifest(ctx context.Context, actorID, workspaceID string, cursor *string, limit *int) (BlobManifest, error) {
	if workspaceID == "" {
		return BlobManifest{}, ErrManifestInvalidInput
	}
	pageSize := defaultManifestPage
	if limit != nil {
		pageSize = min(max(*limit, 1), maxManifestPage)
	}
	var after *manifestCursor
	if cursor != nil && *cursor != "" {
		c, err := decodeCursor(*cursor)
		if err != nil {
			return BlobManifest{}, err
		}
		after = &c
	}
	manifest := BlobManifest{Version: manifestVersion, Entries: []BlobManifestEntry{}}
	err := s.readTx(ctx, func(tx pgx.Tx) error {
		if err := s.requireSync(ctx, tx, actorID, workspaceID, []string{workspaceID}, nil); err != nil {
			return err
		}
		root, err := loadSource(ctx, tx, BlobSource{Type: "currentDoc", WorkspaceID: workspaceID, DocID: workspaceID}, true)
		if err != nil {
			return err
		}
		pages, err := rootPageIDs(root.doc)
		if err != nil {
			return err
		}
		seen := map[string]bool{workspaceID: true}
		ids := []string{workspaceID}
		for _, id := range pages {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		if after != nil {
			start := sort.SearchStrings(ids, after.DocID)
			ids = ids[start:]
		}
		for len(ids) > 0 && len(manifest.Entries) <= pageSize {
			batch := ids[:min(manifestDocBatch, len(ids))]
			ids = ids[len(batch):]
			var access workspaces.WorkspaceSyncAccess
			if err := s.requireSync(ctx, tx, actorID, workspaceID, batch, &access); err != nil {
				return err
			}
			for _, docID := range batch {
				if docID != workspaceID && !access.Documents[docID].Read {
					continue
				}
				source := BlobSource{Type: "currentDoc", WorkspaceID: workspaceID, DocID: docID}
				var loaded *loadedSource
				if docID == workspaceID {
					loaded = root
				} else if loaded, err = loadSource(ctx, tx, source, true); err != nil {
					if errors.Is(err, ErrManifestNotFound) {
						continue
					}
					return err
				}
				keys, err := referencedKeys(loaded.doc, docID == workspaceID)
				if err != nil {
					return err
				}
				found, err := completedBlobs(ctx, tx, workspaceID, keys)
				if err != nil {
					return err
				}
				for _, b := range found {
					if after != nil && docID == after.DocID && b.key <= after.Key {
						continue
					}
					manifest.Entries = append(manifest.Entries, BlobManifestEntry{Key: b.key, MIME: b.mime, Size: b.size, Source: source})
				}
				if len(manifest.Entries) > pageSize {
					break
				}
			}
		}
		return nil
	})
	if err != nil {
		return BlobManifest{}, err
	}
	if len(manifest.Entries) > pageSize {
		manifest.Entries = manifest.Entries[:pageSize]
		last := manifest.Entries[pageSize-1]
		manifest.NextCursor = encodeCursor(manifestCursor{DocID: last.Source.DocID, Key: last.Key})
	}
	return manifest, nil
}

func (s *ManifestService) requireSync(ctx context.Context, tx pgx.Tx, actorID, workspaceID string, docIDs []string, out *workspaces.WorkspaceSyncAccess) error {
	access, err := s.auth.AuthorizeWorkspaceSyncInTx(ctx, tx, actorID, workspaceID, docIDs)
	if err != nil {
		return mapAuthError(err)
	}
	if !access.CanSync {
		return ErrManifestWorkspaceSyncDenied
	}
	if out != nil {
		*out = access
	}
	return nil
}

func (s *ManifestService) OpenBlobV1(ctx context.Context, actorID string, source BlobSource, key string) (io.ReadCloser, BlobReadMetadata, error) {
	if s.objects == nil {
		return nil, BlobReadMetadata{}, errManifestNoStorage
	}
	if err := validateSource(source); err != nil {
		return nil, BlobReadMetadata{}, err
	}
	if key == "" || strings.Contains(key, "/") || storage.ValidateObjectKey(blobObjectKey(source.WorkspaceID, key)) != nil {
		return nil, BlobReadMetadata{}, ErrManifestInvalidInput
	}
	var stamp, mime string
	var size int64
	err := s.readTx(ctx, func(tx pgx.Tx) error {
		if err := s.authorize(ctx, tx, actorID, source); err != nil {
			return err
		}
		loaded, err := loadSource(ctx, tx, source, true)
		if err != nil {
			return err
		}
		keys, err := referencedKeys(loaded.doc, source.DocID == source.WorkspaceID)
		if err != nil {
			return err
		}
		if !keys[key] {
			return ErrManifestBlobNotReferenced
		}
		found, err := completedBlobs(ctx, tx, source.WorkspaceID, map[string]bool{key: true})
		if err != nil {
			return err
		}
		if len(found) == 0 {
			return ErrManifestBlobNotCompleted
		}
		stamp, mime, size = loaded.stamp, found[0].mime, found[0].size
		return nil
	})
	if err != nil {
		return nil, BlobReadMetadata{}, err
	}
	objectKey := blobObjectKey(source.WorkspaceID, key)
	head, err := s.objects.HeadObject(ctx, storage.BlobScope, objectKey)
	if err != nil {
		return nil, BlobReadMetadata{}, err
	}
	if head == nil {
		return nil, BlobReadMetadata{}, ErrManifestBlobObjectNotFound
	}
	if err := s.verify(ctx, actorID, source, key, stamp, size); err != nil {
		return nil, BlobReadMetadata{}, err
	}
	if head.ContentLength != size {
		return nil, BlobReadMetadata{}, errManifestSizeMismatch
	}
	select {
	case s.streams <- struct{}{}:
	default:
		return nil, BlobReadMetadata{}, errManifestStreamLimit
	}
	reader := &blobStream{
		ctx: ctx, service: s, actorID: actorID, source: source, key: key, objectKey: objectKey,
		stamp: stamp, size: size, lastFetch: s.now(),
	}
	return reader, BlobReadMetadata{MIME: mime, Size: size, LastModified: head.LastModified}, nil
}

// verify re-checks that the source is still authorized and unchanged and
// the blob row still has the expected size.
func (s *ManifestService) verify(ctx context.Context, actorID string, source BlobSource, key, stamp string, size int64) error {
	return s.readTx(ctx, func(tx pgx.Tx) error {
		if err := s.authorize(ctx, tx, actorID, source); err != nil {
			return err
		}
		loaded, err := loadSource(ctx, tx, source, false)
		if err != nil {
			return err
		}
		if loaded.stamp != stamp {
			return errManifestSourceChanged
		}
		found, err := completedBlobs(ctx, tx, source.WorkspaceID, map[string]bool{key: true})
		if err != nil {
			return err
		}
		if len(found) == 0 {
			return ErrManifestBlobNotCompleted
		}
		if found[0].size != size {
			return errManifestSourceChanged
		}
		return nil
	})
}

// blobStream reads a blob in verified ranged chunks.
type blobStream struct {
	ctx       context.Context
	service   *ManifestService
	actorID   string
	source    BlobSource
	key       string
	objectKey string
	stamp     string
	size      int64
	offset    int64
	buffer    []byte
	lastFetch time.Time

	mu     sync.Mutex
	closed bool
}

func (b *blobStream) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return 0, fs.ErrClosed
	}
	if len(b.buffer) == 0 {
		if b.offset >= b.size {
			return 0, io.EOF
		}
		if err := b.fetch(); err != nil {
			return 0, err
		}
	}
	n := copy(p, b.buffer)
	b.buffer = b.buffer[n:]
	return n, nil
}

func (b *blobStream) fetch() error {
	s := b.service
	if s.now().Sub(b.lastFetch) >= streamIdleLimit {
		return errManifestStreamExpired
	}
	if err := s.verify(b.ctx, b.actorID, b.source, b.key, b.stamp, b.size); err != nil {
		return err
	}
	length := min(int64(streamChunk), b.size-b.offset)
	chunk, err := s.objects.GetObjectRange(b.ctx, storage.BlobScope, b.objectKey, b.offset, length)
	if err != nil {
		return err
	}
	if int64(len(chunk)) != length {
		return io.ErrUnexpectedEOF
	}
	if err := s.verify(b.ctx, b.actorID, b.source, b.key, b.stamp, b.size); err != nil {
		return err
	}
	b.offset += length
	b.buffer = chunk
	b.lastFetch = s.now()
	return nil
}

func (b *blobStream) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.closed {
		b.closed = true
		<-b.service.streams
	}
	return nil
}
