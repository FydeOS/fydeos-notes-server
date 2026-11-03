// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package documents

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/crdt/yjs"
	"github.com/google/uuid"
)

func TestPostgresWorkspaceDocumentHistoryListAndRecovery(t *testing.T) {
	pool := workspaceDocumentTestPool(t)
	ctx := context.Background()
	workspaceID := "workspace-" + uuid.NewString()
	docID := "doc-" + uuid.NewString()
	actorID := "actor-" + uuid.NewString()
	previousEditorID := "editor-" + uuid.NewString()
	if _, err := pool.Exec(ctx, `
		CREATE TABLE users(id varchar PRIMARY KEY,name text NOT NULL,avatar_url text,disabled boolean NOT NULL DEFAULT false);
		ALTER TABLE snapshots ADD COLUMN state bytea;
		CREATE TABLE snapshot_histories(
			workspace_id varchar NOT NULL,guid varchar NOT NULL,timestamp timestamptz NOT NULL,
			blob bytea NOT NULL,state bytea,expired_at timestamptz NOT NULL,created_by varchar,
			PRIMARY KEY(workspace_id,guid,timestamp));
		CREATE TABLE doc_blob_ref_projections(
			workspace_id varchar NOT NULL,doc_id varchar NOT NULL,status text,indexed_at timestamptz,
			error_code text,error_summary text,updated_at timestamptz);
		CREATE TABLE embedding_sources(
			workspace_id varchar NOT NULL,source_kind text NOT NULL,source_key text NOT NULL,
			deleted_at timestamptz,updated_at timestamptz);`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id) VALUES($1)`, workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_sync_permission_generations(workspace_id,generation) VALUES($1,5)`, workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,name,avatar_url) VALUES($1,'Recovering editor','actor-avatar'),($2,'Previous editor','old-avatar')`, actorID, previousEditorID); err != nil {
		t.Fatal(err)
	}
	targetTimestamp := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Microsecond)
	targetBlob := fixtureUpdate(t, 9401, "restored")
	targetState := []byte("restored-state-vector")
	currentBase := fixtureUpdate(t, 9402, "current-base")
	pending := fixtureUpdate(t, 9403, "pending-update")
	snapshotUpdatedAt := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)
	pendingCreatedAt := snapshotUpdatedAt.Add(time.Hour)
	retention := 48 * time.Hour
	if _, err := pool.Exec(ctx, `INSERT INTO snapshots(workspace_id,guid,blob,state,created_at,updated_at,created_by,updated_by) VALUES($1,$2,$3,$4,$5,$6,$7,$7)`,
		workspaceID, docID, currentBase, []byte("current-state"), snapshotUpdatedAt.Add(-time.Hour), snapshotUpdatedAt, actorID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO updates(workspace_id,guid,blob,created_at,created_by) VALUES($1,$2,$3,$4,$5)`, workspaceID, docID, pending, pendingCreatedAt, actorID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO snapshot_histories(workspace_id,guid,timestamp,blob,state,expired_at,created_by) VALUES($1,$2,$3,$4,$5,$6,$7)`,
		workspaceID, docID, targetTimestamp, targetBlob, targetState, targetTimestamp.Add(retention), previousEditorID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO doc_blob_ref_projections(workspace_id,doc_id,status,indexed_at,error_code,error_summary) VALUES($1,$2,'fresh',now(),'parse_error','old failure')`, workspaceID, docID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO embedding_sources(workspace_id,source_kind,source_key) VALUES($1,'document',$2)`, workspaceID, docID); err != nil {
		t.Fatal(err)
	}

	repository := NewWorkspaceRepository(pool)
	recoveryStartedAt := time.Now().UTC()
	if err := repository.RecoverWorkspaceDoc(ctx, actorID, workspaceID, docID, targetTimestamp, 5); err != nil {
		t.Fatalf("recover document history: %v", err)
	}
	recoveryFinishedAt := time.Now().UTC()
	var restoredBlob, restoredState []byte
	var updatedBy string
	if err := pool.QueryRow(ctx, `SELECT blob,state,updated_by FROM snapshots WHERE workspace_id=$1 AND guid=$2`, workspaceID, docID).Scan(&restoredBlob, &restoredState, &updatedBy); err != nil {
		t.Fatal(err)
	}
	if string(restoredBlob) != string(targetBlob) || string(restoredState) != string(targetState) || updatedBy != actorID {
		t.Fatalf("restored snapshot blob/state/updater = %x/%q/%q", restoredBlob, restoredState, updatedBy)
	}
	var updatesRemaining int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM updates WHERE workspace_id=$1 AND guid=$2`, workspaceID, docID).Scan(&updatesRemaining); err != nil || updatesRemaining != 0 {
		t.Fatalf("pending updates after recovery = %d: %v", updatesRemaining, err)
	}

	archivedCurrent, err := yjs.MergeUpdates([][]byte{currentBase, pending})
	if err != nil {
		t.Fatal(err)
	}
	var archivedBlob []byte
	var archivedState []byte
	var archivedBy string
	var archivedExpiredAt time.Time
	if err := pool.QueryRow(ctx, `SELECT blob,state,created_by,expired_at FROM snapshot_histories WHERE workspace_id=$1 AND guid=$2 AND timestamp=$3`, workspaceID, docID, pendingCreatedAt).Scan(&archivedBlob, &archivedState, &archivedBy, &archivedExpiredAt); err != nil {
		t.Fatal(err)
	}
	if string(archivedBlob) != string(archivedCurrent) || archivedState != nil || archivedBy != actorID {
		t.Fatalf("archived current revision blob/state/editor = %x/%x/%q", archivedBlob, archivedState, archivedBy)
	}
	if archivedExpiredAt.Before(recoveryStartedAt.Add(retention)) || archivedExpiredAt.After(recoveryFinishedAt.Add(retention)) {
		t.Fatalf("archived expiration = %s, want recovery time + %s", archivedExpiredAt, retention)
	}
	currentDoc, err := yjs.Restore(archivedBlob, nil)
	if err != nil {
		t.Fatal(err)
	}
	expectedDoc, err := yjs.Restore(archivedCurrent, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(currentDoc.StateVector()), string(expectedDoc.StateVector()); got != want {
		t.Fatalf("archived current state vector = %x, want %x", got, want)
	}

	histories, err := repository.ListWorkspaceDocHistories(ctx, workspaceID, docID, time.Now().UTC().Add(time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(histories) != 2 {
		t.Fatalf("history count = %d, want 2: %+v", len(histories), histories)
	}
	if !histories[0].Timestamp.Equal(pendingCreatedAt) {
		t.Fatalf("latest history timestamp = %s, want %s", histories[0].Timestamp, pendingCreatedAt)
	}
	if histories[0].Editor == nil || histories[0].Editor.Name != "Recovering editor" || histories[1].Editor == nil || histories[1].Editor.Name != "Previous editor" {
		t.Fatalf("history editors = %+v/%+v", histories[0].Editor, histories[1].Editor)
	}
	limited, err := repository.ListWorkspaceDocHistories(ctx, workspaceID, docID, time.Now().UTC().Add(time.Hour), 1)
	if err != nil || len(limited) != 1 || !limited[0].Timestamp.Equal(pendingCreatedAt) {
		t.Fatalf("descending limited histories = %+v: %v", limited, err)
	}
	beforeLatest, err := repository.ListWorkspaceDocHistories(ctx, workspaceID, docID, pendingCreatedAt, 10)
	if err != nil || len(beforeLatest) != 1 || !beforeLatest[0].Timestamp.Equal(targetTimestamp) {
		t.Fatalf("strict before filter = %+v: %v", beforeLatest, err)
	}
	beforeOldest, err := repository.ListWorkspaceDocHistories(ctx, workspaceID, docID, targetTimestamp, 10)
	if err != nil || len(beforeOldest) != 0 {
		t.Fatalf("strict before at oldest history = %+v: %v", beforeOldest, err)
	}
	var projectionStatus string
	var projectionIndexedAt, embeddingDeletedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT status,indexed_at FROM doc_blob_ref_projections WHERE workspace_id=$1 AND doc_id=$2`, workspaceID, docID).Scan(&projectionStatus, &projectionIndexedAt); err != nil {
		t.Fatal(err)
	}
	if projectionStatus != "pending" || projectionIndexedAt != nil {
		t.Fatalf("blob reference projection after recovery = %q/%v", projectionStatus, projectionIndexedAt)
	}
	if err := pool.QueryRow(ctx, `SELECT deleted_at FROM embedding_sources WHERE workspace_id=$1 AND source_key=$2`, workspaceID, docID).Scan(&embeddingDeletedAt); err != nil || embeddingDeletedAt == nil {
		t.Fatalf("embedding source invalidation = %v: %v", embeddingDeletedAt, err)
	}

	if err := repository.RecoverWorkspaceDoc(ctx, actorID, workspaceID, docID, targetTimestamp.Add(-time.Second), 5); !errors.Is(err, ErrWorkspaceDocHistoryNotFound) {
		t.Fatalf("recover missing history error = %v", err)
	}
	if err := repository.RecoverWorkspaceDoc(ctx, actorID, workspaceID, docID, targetTimestamp, 4); !errors.Is(err, ErrWorkspacePermissionGenerationChanged) {
		t.Fatalf("recover stale generation error = %v", err)
	}
	var finalBlob []byte
	var historyCount int
	if err := pool.QueryRow(ctx, `SELECT blob FROM snapshots WHERE workspace_id=$1 AND guid=$2`, workspaceID, docID).Scan(&finalBlob); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM snapshot_histories WHERE workspace_id=$1 AND guid=$2`, workspaceID, docID).Scan(&historyCount); err != nil {
		t.Fatal(err)
	}
	if string(finalBlob) != string(targetBlob) || historyCount != 2 {
		t.Fatalf("failed recovery mutated state: current=%x histories=%d", finalBlob, historyCount)
	}
}
