// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package documents

import (
	"context"
	"errors"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/crdt/yjs"
	"github.com/jackc/pgx/v5"
)

var ErrWorkspaceDocHistoryNotFound = errors.New("workspace document history not found")

type WorkspaceDocHistoryEditor struct {
	ID        string
	Name      string
	AvatarURL *string
}

type WorkspaceDocHistory struct {
	Timestamp time.Time
	Editor    *WorkspaceDocHistoryEditor
}

func (r *WorkspaceRepository) ListWorkspaceDocHistories(ctx context.Context, workspaceID, docID string, before time.Time, take int) ([]WorkspaceDocHistory, error) {
	rows, err := r.db.Query(ctx, `
		SELECT history.timestamp, editor.id, editor.name, editor.avatar_url
		FROM snapshot_histories history
		LEFT JOIN users editor ON editor.id=history.created_by AND NOT editor.disabled
		WHERE history.workspace_id=$1 AND history.guid=$2 AND history.timestamp < $3
		ORDER BY history.timestamp DESC LIMIT $4`, workspaceID, docID, before, take)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]WorkspaceDocHistory, 0)
	for rows.Next() {
		var history WorkspaceDocHistory
		var editorID, editorName *string
		var avatarURL *string
		if err := rows.Scan(&history.Timestamp, &editorID, &editorName, &avatarURL); err != nil {
			return nil, err
		}
		if editorID != nil && editorName != nil {
			history.Editor = &WorkspaceDocHistoryEditor{ID: *editorID, Name: *editorName, AvatarURL: avatarURL}
		}
		result = append(result, history)
	}
	return result, rows.Err()
}

func (r *WorkspaceRepository) RecoverWorkspaceDoc(ctx context.Context, actorID, workspaceID, docID string, timestamp time.Time, expectedGeneration int64) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockWorkspaceStorageShared(ctx, tx, workspaceID); err != nil {
		return err
	}
	if err := lockWorkspaceDocumentUpdate(ctx, tx, workspaceID, docID); err != nil {
		return err
	}
	if err := lockWorkspaceDomain(ctx, tx, workspaceID); err != nil {
		return err
	}
	if err := lockWorkspacePermissionGeneration(ctx, tx, workspaceID, expectedGeneration); err != nil {
		return err
	}

	var currentBlob []byte
	var currentUpdatedAt time.Time
	err = tx.QueryRow(ctx, `
		SELECT blob, updated_at FROM snapshots
		WHERE workspace_id=$1 AND guid=$2 FOR UPDATE`, workspaceID, docID).Scan(&currentBlob, &currentUpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrWorkspaceDocNotFound
	}
	if err != nil {
		return err
	}

	updates, err := tx.Query(ctx, `
		SELECT blob, created_at FROM updates
		WHERE workspace_id=$1 AND guid=$2 ORDER BY created_at FOR UPDATE`, workspaceID, docID)
	if err != nil {
		return err
	}
	currentUpdates := make([][]byte, 0)
	for updates.Next() {
		var blob []byte
		var createdAt time.Time
		if err := updates.Scan(&blob, &createdAt); err != nil {
			updates.Close()
			return err
		}
		currentUpdates = append(currentUpdates, blob)
		if createdAt.After(currentUpdatedAt) {
			currentUpdatedAt = createdAt
		}
	}
	if err := updates.Err(); err != nil {
		updates.Close()
		return err
	}
	updates.Close()
	currentUpdates = append([][]byte{currentBlob}, currentUpdates...)
	mergedCurrent, err := yjs.MergeUpdates(currentUpdates)
	if err != nil {
		return err
	}

	var targetBlob, targetState []byte
	var retentionSeconds int64
	err = tx.QueryRow(ctx, `
		SELECT blob, state, EXTRACT(EPOCH FROM expired_at - timestamp)::bigint
		FROM snapshot_histories
		WHERE workspace_id=$1 AND guid=$2 AND timestamp=$3 FOR UPDATE`, workspaceID, docID, timestamp).
		Scan(&targetBlob, &targetState, &retentionSeconds)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrWorkspaceDocHistoryNotFound
	}
	if err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO snapshot_histories
			(workspace_id,guid,timestamp,blob,state,expired_at,created_by)
		VALUES ($1,$2,$3,$4,NULL,clock_timestamp()+make_interval(secs => $5),$6)
		ON CONFLICT (workspace_id,guid,timestamp) DO NOTHING`, workspaceID, docID, currentUpdatedAt, mergedCurrent, retentionSeconds, actorID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM updates WHERE workspace_id=$1 AND guid=$2`, workspaceID, docID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE snapshots SET blob=$3,state=$4,updated_at=clock_timestamp(),updated_by=$5
		WHERE workspace_id=$1 AND guid=$2`, workspaceID, docID, targetBlob, targetState, actorID); err != nil {
		return err
	}
	if err := invalidateWorkspaceDocumentProjection(ctx, tx, workspaceID, docID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
