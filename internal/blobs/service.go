// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package blobs

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/entitlements"
	"github.com/fydeos/fydeos-notes-server/internal/storage"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	reservationTTL   = time.Hour
	multipartPart    = 5 << 20
	multipartMinimum = 10 << 20
	maxPartNumber    = 10000
	invalidation     = "runtime-invalidation"
)

type servicePool interface {
	Begin(context.Context) (pgx.Tx, error)
	Acquire(context.Context) (*pgxpool.Conn, error)
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// queryer is the subset shared by pools and transactions.
type queryer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type CommentDocumentAuthorizer interface {
	AuthorizeDocumentInTx(context.Context, pgx.Tx, string, string, string) (workspaces.DocumentAuthorization, error)
}

type QuotaStateChangePublisher interface {
	PublishUserQuotaStateChanged(string, string)
	PublishWorkspaceQuotaStateChanged(string, string)
}

// Service manages blob uploads, deletion, comment attachments and cleanup.
type Service struct {
	pool          servicePool
	objects       *storage.Runtime
	deployment    entitlements.Deployment
	publicBaseURL string
	commentAuth   CommentDocumentAuthorizer
	publisher     QuotaStateChangePublisher
	lockTimeout   time.Duration
	// afterOwnerRead is a test hook run between reading and re-checking the owner.
	afterOwnerRead func(context.Context)
}

func NewService(pool servicePool, objectStorage *storage.Runtime, deployment string, publicBaseURL ...string) *Service {
	s := &Service{pool: pool, objects: objectStorage, deployment: entitlements.ParseDeployment(deployment), lockTimeout: 5 * time.Second}
	if len(publicBaseURL) > 0 {
		s.publicBaseURL = publicBaseURL[0]
	}
	return s
}

func (s *Service) SetCommentDocumentAuthorizer(authorizer CommentDocumentAuthorizer) {
	s.commentAuth = authorizer
}

func (s *Service) SetQuotaStateChangePublisher(publisher QuotaStateChangePublisher) {
	s.publisher = publisher
}

func (s *Service) publish(workspaceID, ownerID string) {
	if s.publisher == nil {
		return
	}
	s.publisher.PublishWorkspaceQuotaStateChanged(workspaceID, invalidation)
	if ownerID != "" {
		s.publisher.PublishUserQuotaStateChanged(ownerID, invalidation)
	}
}

// Object keys.

func blobObjectKey(workspaceID, key string) string { return workspaceID + "/" + key }

func blobStagingKey(workspaceID, reservationID, key string) string {
	return workspaceID + "/.reservations/" + reservationID + "/" + key
}

func commentObjectKey(workspaceID, docID, key string) string {
	return "comment-attachments/" + workspaceID + "/" + docID + "/" + key
}

func commentStagingKey(workspaceID, docID, reservationID, key string) string {
	return "comment-attachments/" + workspaceID + "/" + docID + "/.reservations/" + reservationID + "/" + key
}

// contentKey returns the unpadded base64url SHA-256 of data.
func contentKey(data []byte) string {
	sum := sha256.Sum256(data)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func keyMatches(key string, data []byte) bool {
	return strings.TrimRight(key, "=") == contentKey(data)
}

// Advisory locks.

type heldLocks struct {
	conn      *pgxpool.Conn
	workspace string
	object    string
}

func lockID(prefix, value string) string { return prefix + value }

// withObjectLock runs fn while holding a shared workspace lock and an
// exclusive object lock.
func (s *Service) withObjectLock(ctx context.Context, workspaceID, objectKey string, fn func() error) error {
	locks, err := s.acquireLocks(ctx, workspaceID, objectKey)
	if err != nil {
		return err
	}
	defer s.releaseLocks(locks)
	return fn()
}

func (s *Service) acquireLocks(ctx context.Context, workspaceID, objectKey string) (*heldLocks, error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	locks := &heldLocks{conn: conn}
	deadline := time.Now().Add(s.lockTimeout)
	try := func(query, name string) error {
		for {
			var ok bool
			if err := conn.QueryRow(ctx, query, name).Scan(&ok); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return err
			}
			if ok {
				return nil
			}
			if !time.Now().Before(deadline) {
				return errLockTimeout
			}
			timer := time.NewTimer(20 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	wsName := lockID("storage-workspace:", workspaceID)
	if err := try(`SELECT pg_try_advisory_lock_shared(hashtextextended($1, 0))`, wsName); err != nil {
		s.releaseLocks(locks)
		return nil, err
	}
	locks.workspace = wsName
	objName := lockID("storage-object:", objectKey)
	if err := try(`SELECT pg_try_advisory_lock(hashtextextended($1, 0))`, objName); err != nil {
		s.releaseLocks(locks)
		return nil, err
	}
	locks.object = objName
	return locks, nil
}

func (s *Service) releaseLocks(locks *heldLocks) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	healthy := true
	unlock := func(query, name string) {
		if name == "" || !healthy {
			return
		}
		var ok bool
		if err := locks.conn.QueryRow(ctx, query, name).Scan(&ok); err != nil || !ok {
			healthy = false
		}
	}
	unlock(`SELECT pg_advisory_unlock(hashtextextended($1, 0))`, locks.object)
	unlock(`SELECT pg_advisory_unlock_shared(hashtextextended($1, 0))`, locks.workspace)
	if !healthy {
		raw := locks.conn.Hijack()
		_ = raw.Close(ctx)
		return
	}
	locks.conn.Release()
}

// Transactions.

func (s *Service) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	return runTx(ctx, s.pool, pgx.TxOptions{}, fn)
}

func runTx(ctx context.Context, pool servicePool, opts pgx.TxOptions, fn func(pgx.Tx) error) error {
	var tx pgx.Tx
	var err error
	if beginner, ok := pool.(interface {
		BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
	}); ok {
		tx, err = beginner.BeginTx(ctx, opts)
	} else {
		tx, err = pool.Begin(ctx)
	}
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
