// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package search

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/byok"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// EmbeddingCoordinator keeps each workspace's active embedding index in line
// with its configured embedding route.
type EmbeddingCoordinator struct {
	db      embeddingDB
	targets TargetResolver
}

func NewEmbeddingCoordinator(db embeddingDB, targets TargetResolver) *EmbeddingCoordinator {
	return &EmbeddingCoordinator{db: db, targets: targets}
}

func (c *EmbeddingCoordinator) Run(ctx context.Context) error {
	if c == nil || c.db == nil || c.targets == nil {
		return errors.New("embedding coordinator is not configured")
	}
	for {
		if err := c.RunOnce(ctx); err != nil && ctx.Err() == nil {
			log.Printf("embedding coordinator: %v", err)
		}
		if !sleepCtx(ctx, 30*time.Second) {
			return nil
		}
	}
}

type coordinatedWorkspace struct {
	id      string
	enabled bool
	owner   string
}

func (c *EmbeddingCoordinator) RunOnce(ctx context.Context) error {
	if c == nil || c.db == nil || c.targets == nil {
		return errors.New("embedding coordinator is not configured")
	}
	exists, err := embeddingTablesExist(ctx, c.db)
	if err != nil || !exists {
		return err
	}
	rows, err := c.db.Query(ctx, `
		SELECT w.id, w.enable_doc_embedding, COALESCE((
			SELECT m.user_id FROM workspace_members m
			WHERE m.workspace_id=w.id AND m.role='owner' AND m.state='active'
			ORDER BY m.created_at, m.user_id LIMIT 1), '')
		FROM workspaces w
		LEFT JOIN embedding_workspace_states s ON s.workspace_id=w.id
		WHERE w.enable_doc_embedding OR (s.workspace_id IS NOT NULL AND s.runtime_state <> 'disabled')
		ORDER BY w.id`)
	if err != nil {
		return fmt.Errorf("list embedding workspaces: %w", err)
	}
	var list []coordinatedWorkspace
	for rows.Next() {
		var w coordinatedWorkspace
		if err := rows.Scan(&w.id, &w.enabled, &w.owner); err != nil {
			rows.Close()
			return err
		}
		list = append(list, w)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, w := range list {
		var target *byok.ChatTarget
		if w.enabled && w.owner != "" {
			resolved, err := c.targets.ResolveServerTarget(ctx, w.id, w.owner, embeddingFeature)
			switch {
			case err == nil:
				target = &resolved
			case routeUnavailable(err):
			default:
				return fmt.Errorf("resolve embedding route for %s: %w", w.id, err)
			}
		}
		if err := c.reconcile(ctx, w, target); err != nil {
			return fmt.Errorf("reconcile embedding workspace %s: %w", w.id, err)
		}
	}
	return nil
}

func (c *EmbeddingCoordinator) reconcile(ctx context.Context, w coordinatedWorkspace, target *byok.ChatTarget) error {
	tx, err := c.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `INSERT INTO embedding_workspace_states(workspace_id, runtime_state)
		VALUES ($1, 'unavailable') ON CONFLICT (workspace_id) DO NOTHING`, w.id); err != nil {
		return err
	}
	var (
		currentIndex  *string
		currentState  string
		currentReason *string
	)
	if err := tx.QueryRow(ctx, `SELECT active_index_id::text, runtime_state, reason_code
		FROM embedding_workspace_states WHERE workspace_id=$1 FOR UPDATE`, w.id).Scan(&currentIndex, &currentState, &currentReason); err != nil {
		return err
	}
	var (
		wantIndex  *string
		wantState  string
		wantReason *string
	)
	switch {
	case !w.enabled:
		wantState, wantReason = "disabled", strPtr("workspace_embedding_disabled")
	case target == nil:
		wantState, wantReason = "unavailable", strPtr("embedding_route_unavailable")
	default:
		id, err := ensureIndex(ctx, tx, w.id, *target)
		if err != nil {
			return err
		}
		wantIndex, wantState = &id, "active"
	}
	indexChanged := !equalPtr(currentIndex, wantIndex)
	if !indexChanged && currentState == wantState && equalPtr(currentReason, wantReason) {
		return tx.Commit(ctx)
	}
	if indexChanged {
		if currentIndex != nil {
			if _, err := tx.Exec(ctx, `UPDATE embedding_indexes SET inactive_at=now(), updated_at=now()
				WHERE id=$1 AND inactive_at IS NULL`, *currentIndex); err != nil {
				return err
			}
		}
		if wantIndex != nil {
			if _, err := tx.Exec(ctx, `
				INSERT INTO embedding_projections(source_id, index_id, status, priority)
				SELECT id, $2, 'pending', CASE WHEN source_kind='artifact' THEN 200 ELSE 100 END
				FROM embedding_sources WHERE workspace_id=$1 AND deleted_at IS NULL
				ON CONFLICT (source_id, index_id) DO NOTHING`, w.id, *wantIndex); err != nil {
				return err
			}
		}
	}
	epochStep := 0
	if indexChanged {
		epochStep = 1
	}
	if _, err := tx.Exec(ctx, `UPDATE embedding_workspace_states
		SET active_index_id=$2, runtime_state=$3, reason_code=$4, index_epoch=index_epoch+$5, changed_at=now()
		WHERE workspace_id=$1`, w.id, wantIndex, wantState, wantReason, epochStep); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ensureIndex finds or creates the workspace index for target and makes sure
// it is not marked inactive.
func ensureIndex(ctx context.Context, tx pgx.Tx, workspaceID string, target byok.ChatTarget) (string, error) {
	fingerprint := routeFingerprint(target)
	var (
		id       string
		inactive bool
	)
	err := tx.QueryRow(ctx, `SELECT id::text, inactive_at IS NOT NULL FROM embedding_indexes
		WHERE workspace_id=$1 AND fingerprint=$2 FOR UPDATE`, workspaceID, fingerprint).Scan(&id, &inactive)
	if errors.Is(err, pgx.ErrNoRows) {
		id = uuid.NewString()
		_, err = tx.Exec(ctx, `INSERT INTO embedding_indexes(id, workspace_id, fingerprint, route_source, provider,
				model_id, endpoint_fingerprint, dimensions, distance_metric, contract_version, health_status)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'cosine',$9,'pending')`,
			id, workspaceID, fingerprint, routeSource(target.Source), target.Provider, target.ModelID,
			endpointFingerprint(target), byok.EmbeddingDimensions, embeddingContract)
		return id, err
	}
	if err != nil {
		return "", err
	}
	if inactive {
		_, err = tx.Exec(ctx, `UPDATE embedding_indexes SET inactive_at=NULL, activated_at=now(), updated_at=now() WHERE id=$1`, id)
	}
	return id, err
}

func strPtr(s string) *string { return &s }

func equalPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
