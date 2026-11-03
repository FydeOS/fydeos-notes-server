// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package documents

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type rootDomainLockObserver struct {
	*pgxpool.Pool
	attempted chan struct{}
}

func (db rootDomainLockObserver) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return rootDomainLockTx{Tx: tx, attempted: db.attempted}, nil
}

type rootDomainLockTx struct {
	pgx.Tx
	attempted chan struct{}
}

func (tx rootDomainLockTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if len(args) > 0 && args[0] == "domain:workspace:lock-order-space" {
		select {
		case tx.attempted <- struct{}{}:
		default:
		}
	}
	return tx.Tx.QueryRow(ctx, sql, args...)
}

func TestRootSyncWaitsForACLTransactionBeforeGenerationFence(t *testing.T) {
	pool := workspaceDocumentTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `ALTER TABLE workspaces ADD COLUMN last_check_embeddings timestamptz; INSERT INTO workspaces(id) VALUES('lock-order-space'); INSERT INTO workspace_sync_permission_generations VALUES('lock-order-space',3)`); err != nil {
		t.Fatal(err)
	}
	acl, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = acl.Rollback(context.Background()) }()
	if _, err := acl.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('domain:workspace:lock-order-space',0)); SELECT id FROM workspaces WHERE id='lock-order-space' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	attempted := make(chan struct{}, 1)
	repository := NewWorkspaceRepository(rootDomainLockObserver{Pool: pool, attempted: attempted})
	root := rootPageUpdate(t, 8701, "doc-1")
	done := make(chan error, 1)
	go func() {
		_, err := repository.PushWorkspace(ctx, "user-1", "lock-order-space", "lock-order-space", root, 3)
		done <- err
	}()
	joined := false
	defer func() {
		_ = acl.Rollback(context.Background())
		cancel()
		if !joined {
			<-done
		}
	}()
	select {
	case <-attempted:
	case <-time.After(time.Second):
		t.Fatal("root sync did not acquire the domain lock before its generation fence")
	}
	// This must succeed while root sync is waiting, instead of forming the
	// workspace -> generation -> workspace cycle reported by review.
	if _, err := acl.Exec(ctx, `SELECT generation FROM workspace_sync_permission_generations WHERE workspace_id='lock-order-space' FOR UPDATE NOWAIT; UPDATE workspace_sync_permission_generations SET generation=4 WHERE workspace_id='lock-order-space'`); err != nil {
		t.Fatal(err)
	}
	if err := acl.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	err = <-done
	joined = true
	if !errors.Is(err, ErrWorkspacePermissionGenerationChanged) {
		t.Fatalf("stale root sync error=%v", err)
	}
	if _, err := repository.PushWorkspace(ctx, "user-1", "lock-order-space", "lock-order-space", root, 4); err != nil {
		t.Fatalf("retry at committed generation: %v", err)
	}
}
