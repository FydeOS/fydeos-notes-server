// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package workspaces

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// CreateForUser creates a private workspace owned by userID.
func (r *Repository) CreateForUser(ctx context.Context, userID string) (Workspace, error) {
	id := uuid.NewString()
	err := r.inTx(ctx, nil, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO workspaces(id) VALUES ($1)`, id); err != nil {
			return fmt.Errorf("insert workspace: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO workspace_access_policies(workspace_id, visibility, sharing_enabled, url_preview_enabled) VALUES ($1,'private',true,false)`, id); err != nil {
			return fmt.Errorf("insert workspace policy: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO workspace_members(id, workspace_id, user_id, role, state, source) VALUES ($3,$1,$2,'owner','active','legacy')`, id, userID, newRowID()); err != nil {
			return fmt.Errorf("insert workspace owner: %w", err)
		}
		return nil
	})
	if err != nil {
		return Workspace{}, err
	}
	return Workspace{ID: id, OwnerID: userID}, nil
}

const workspaceSummarySQL = `
	SELECT w.id,
	       EXISTS (SELECT 1 FROM snapshots s WHERE s.workspace_id=w.id AND s.guid=w.id)
	         OR EXISTS (SELECT 1 FROM updates u WHERE u.workspace_id=w.id AND u.guid=w.id),
	       (SELECT o.user_id FROM workspace_members o WHERE o.workspace_id=w.id AND o.role='owner' AND o.state='active' ORDER BY o.created_at, o.id LIMIT 1)
	FROM workspaces w`

func (r *Repository) scanSummaries(ctx context.Context, q queryer, sql string, args ...any) ([]Workspace, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}
	type row struct {
		ws    Workspace
		owner *string
	}
	var items []row
	for rows.Next() {
		var it row
		if err := rows.Scan(&it.ws.ID, &it.ws.Initialized, &it.owner); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan workspace: %w", err)
		}
		items = append(items, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Workspace, 0, len(items))
	for _, it := range items {
		if it.owner == nil {
			return nil, ErrOwnerMissing
		}
		it.ws.OwnerID = *it.owner
		team, known, err := teamFlag(ctx, q, it.ws.ID)
		if err != nil {
			return nil, err
		}
		if !known {
			return nil, ErrQuotaStateUnknown
		}
		it.ws.Team = team
		out = append(out, it.ws)
	}
	return out, nil
}

// GetForUser returns a workspace the user is an active member of.
func (r *Repository) GetForUser(ctx context.Context, workspaceID, userID string) (Workspace, error) {
	items, err := r.scanSummaries(ctx, r.db, workspaceSummarySQL+`
		JOIN workspace_members m ON m.workspace_id=w.id AND m.user_id=$2 AND m.state='active'
		WHERE w.id=$1`, workspaceID, userID)
	if err != nil {
		return Workspace{}, err
	}
	if len(items) == 0 {
		exists, err := workspaceExists(ctx, r.db, workspaceID)
		if err != nil {
			return Workspace{}, err
		}
		if exists {
			return Workspace{}, ErrAccessDenied
		}
		return Workspace{}, ErrNotFound
	}
	return items[0], nil
}

// readWorkspaceAny returns the workspace summary without a membership check.
func (r *Repository) readWorkspaceAny(ctx context.Context, workspaceID string) (Workspace, error) {
	items, err := r.scanSummaries(ctx, r.db, workspaceSummarySQL+` WHERE w.id=$1`, workspaceID)
	if err != nil {
		return Workspace{}, err
	}
	if len(items) == 0 {
		return Workspace{}, ErrNotFound
	}
	return items[0], nil
}

// ListForUser lists the user's active workspaces by creation.
func (r *Repository) ListForUser(ctx context.Context, userID string) ([]Workspace, error) {
	return r.scanSummaries(ctx, r.db, workspaceSummarySQL+`
		JOIN workspace_members m ON m.workspace_id=w.id AND m.user_id=$1 AND m.state='active'
		ORDER BY w.created_at, w.id`, userID)
}

// ListOwnedForUser lists workspaces the user actively owns, by id.
func (r *Repository) ListOwnedForUser(ctx context.Context, userID string) ([]OwnedWorkspace, error) {
	rows, err := r.db.Query(ctx, `SELECT workspace_id FROM workspace_members WHERE user_id=$1 AND role='owner' AND state='active' ORDER BY workspace_id`, userID)
	if err != nil {
		return nil, fmt.Errorf("list owned workspaces: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	out := make([]OwnedWorkspace, 0, len(ids))
	for _, id := range ids {
		team, known, err := teamFlag(ctx, r.db, id)
		if err != nil {
			return nil, err
		}
		if !known {
			return nil, ErrQuotaStateUnknown
		}
		out = append(out, OwnedWorkspace{ID: id, Team: team})
	}
	return out, nil
}

// UpdateForUser changes the provided workspace settings.
func (r *Repository) UpdateForUser(ctx context.Context, workspaceID, userID string, update SettingsUpdate) (Workspace, error) {
	err := r.inTx(ctx, nil, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspaces WHERE id=$1)`, workspaceID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
		role, err := activeRole(ctx, tx, workspaceID, userID)
		if err != nil {
			return err
		}
		if role != "owner" && role != "admin" {
			return ErrAccessDenied
		}
		if update.EnableAI != nil || update.EnableDocEmbedding != nil {
			if _, err := tx.Exec(ctx, `UPDATE workspaces SET enable_ai=COALESCE($2, enable_ai), enable_doc_embedding=COALESCE($3, enable_doc_embedding) WHERE id=$1`,
				workspaceID, update.EnableAI, update.EnableDocEmbedding); err != nil {
				return fmt.Errorf("update workspace: %w", err)
			}
		}
		if update.EnableSharing != nil || update.EnableURLPreview != nil || update.Public != nil {
			var visibility *string
			if update.Public != nil {
				v := "private"
				if *update.Public {
					v = "public"
				}
				visibility = &v
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO workspace_access_policies(workspace_id, visibility, sharing_enabled, url_preview_enabled)
				VALUES ($1, COALESCE($2,'private'), COALESCE($3,true), COALESCE($4,false))
				ON CONFLICT (workspace_id) DO UPDATE SET
				  visibility=COALESCE($2, workspace_access_policies.visibility),
				  sharing_enabled=COALESCE($3, workspace_access_policies.sharing_enabled),
				  url_preview_enabled=COALESCE($4, workspace_access_policies.url_preview_enabled),
				  updated_at=CURRENT_TIMESTAMP`,
				workspaceID, visibility, update.EnableSharing, update.EnableURLPreview); err != nil {
				return fmt.Errorf("update workspace policy: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return Workspace{}, err
	}
	return r.GetForUser(ctx, workspaceID, userID)
}

// DeleteForUser deletes an owned workspace, queues storage cleanup, and
// returns the users that had a stake in it.
func (r *Repository) DeleteForUser(ctx context.Context, workspaceID, userID string) ([]string, error) {
	var users []string
	err := r.inTx(ctx, nil, func(tx pgx.Tx) error {
		deadline := time.Now().Add(5 * time.Second)
		for {
			ok, err := tryLockExclusive(ctx, tx, storageLockKey(workspaceID))
			if err != nil {
				return err
			}
			if ok {
				break
			}
			if time.Now().After(deadline) {
				return ErrStorageLockTimeout
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(25 * time.Millisecond):
			}
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspaces WHERE id=$1)`, workspaceID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
		role, err := activeRole(ctx, tx, workspaceID, userID)
		if err != nil {
			return err
		}
		if role != "owner" {
			return ErrAccessDenied
		}
		rows, err := tx.Query(ctx, `
			SELECT user_id FROM workspace_members WHERE workspace_id=$1
			UNION SELECT user_id FROM ai_sessions_metadata WHERE workspace_id=$1
			ORDER BY 1`, workspaceID)
		if err != nil {
			return fmt.Errorf("collect workspace users: %w", err)
		}
		if users, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return err
		}
		if users == nil {
			users = []string{}
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO workspace_storage_cleanup_jobs(workspace_id, user_ids, attempt, available_at, lease_token, lease_until, last_error, updated_at)
			VALUES ($1, $2, 0, clock_timestamp(), NULL, NULL, NULL, clock_timestamp())
			ON CONFLICT (workspace_id) DO UPDATE SET
			  user_ids=ARRAY(SELECT DISTINCT u FROM unnest(workspace_storage_cleanup_jobs.user_ids || EXCLUDED.user_ids) AS u ORDER BY u),
			  attempt=0, available_at=EXCLUDED.available_at, lease_token=NULL, lease_until=NULL, last_error=NULL,
			  updated_at=EXCLUDED.updated_at`, workspaceID, users); err != nil {
			return fmt.Errorf("queue workspace storage cleanup: %w", err)
		}
		for _, stmt := range []string{
			`DELETE FROM snapshots WHERE workspace_id=$1`,
			`DELETE FROM updates WHERE workspace_id=$1`,
			`DELETE FROM snapshot_histories WHERE workspace_id=$1`,
			`DELETE FROM workspaces WHERE id=$1`,
		} {
			if _, err := tx.Exec(ctx, stmt, workspaceID); err != nil {
				return fmt.Errorf("delete workspace data: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return users, nil
}

// UpsertRootDocument writes the workspace root snapshot unless a newer one
// is already stored.
func (r *Repository) UpsertRootDocument(ctx context.Context, workspaceID, userID string, blob []byte) error {
	now := r.clock().UTC().Truncate(time.Millisecond)
	var by *string
	if userID != "" {
		by = &userID
	}
	return r.inTx(ctx, nil, func(tx pgx.Tx) error {
		if err := setLockTimeout(ctx, tx, 5*time.Second); err != nil {
			return err
		}
		if err := lockExclusive(ctx, tx, domainLockKey(workspaceID)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO snapshots(workspace_id, guid, blob, size, created_at, updated_at, created_by, updated_by)
			VALUES ($1, $1, $2, $3, $4, $4, $5, $5)
			ON CONFLICT (workspace_id, guid) DO UPDATE SET
			  blob=EXCLUDED.blob, size=EXCLUDED.size, updated_at=EXCLUDED.updated_at, updated_by=EXCLUDED.updated_by
			WHERE snapshots.updated_at <= EXCLUDED.updated_at`, workspaceID, blob, int64(len(blob)), now, by)
		if err != nil {
			return fmt.Errorf("upsert root document: %w", err)
		}
		return nil
	})
}

// ReadPresentationMetadata returns the public flag and creation time.
func (r *Repository) ReadPresentationMetadata(ctx context.Context, workspaceID string) (PresentationMetadata, error) {
	var m PresentationMetadata
	err := r.db.QueryRow(ctx, `
		SELECT COALESCE(p.visibility='public', false), w.created_at
		FROM workspaces w LEFT JOIN workspace_access_policies p ON p.workspace_id=w.id WHERE w.id=$1`, workspaceID).Scan(&m.Public, &m.CreatedAt)
	if noRows(err) {
		return m, ErrNotFound
	}
	return m, err
}

// MemberCount counts active members plus invitation rows.
func (r *Repository) MemberCount(ctx context.Context, workspaceID string) (int, error) {
	var n int
	err := r.db.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM workspace_members WHERE workspace_id=$1 AND state='active')
		     + (SELECT count(*) FROM workspace_invitations WHERE workspace_id=$1)`, workspaceID).Scan(&n)
	return n, err
}
