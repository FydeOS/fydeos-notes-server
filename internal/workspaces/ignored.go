// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package workspaces

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

const ignoredDocsSQL = `
	SELECT i.doc_id, i.created_at, s.created_at, s.updated_at, NULLIF(wp.title, ''), cu.name, cu.avatar_url, uu.name
	FROM ai_workspace_ignored_docs i
	LEFT JOIN workspace_pages wp ON wp.workspace_id=i.workspace_id AND wp.page_id=i.doc_id
	LEFT JOIN snapshots s ON s.workspace_id=i.workspace_id AND s.guid=i.doc_id
	LEFT JOIN users cu ON cu.id=s.created_by
	LEFT JOIN users uu ON uu.id=s.updated_by
	WHERE i.workspace_id=$1
	ORDER BY i.created_at DESC, i.doc_id`

func scanIgnored(rows pgx.Rows, err error) ([]IgnoredWorkspaceDoc, error) {
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (IgnoredWorkspaceDoc, error) {
		var d IgnoredWorkspaceDoc
		err := row.Scan(&d.DocID, &d.CreatedAt, &d.DocCreatedAt, &d.DocUpdatedAt, &d.Title, &d.CreatedBy, &d.CreatedByAvatar, &d.UpdatedBy)
		return d, err
	})
	if out == nil && err == nil {
		out = []IgnoredWorkspaceDoc{}
	}
	return out, err
}

func (r *Repository) ListIgnoredWorkspaceDocs(ctx context.Context, workspaceID string, first, offset int) ([]IgnoredWorkspaceDoc, int, error) {
	if first < 1 || first > 100 {
		return nil, 0, errors.New("first must be between 1 and 100")
	}
	if offset < 0 {
		return nil, 0, errors.New("offset must not be negative")
	}
	var total int
	if err := r.db.QueryRow(ctx, `SELECT count(*) FROM ai_workspace_ignored_docs WHERE workspace_id=$1`, workspaceID).Scan(&total); err != nil {
		return nil, 0, err
	}
	docs, err := scanIgnored(r.db.Query(ctx, ignoredDocsSQL+` OFFSET $2 LIMIT $3`, workspaceID, offset, first))
	return docs, total, err
}

func (r *Repository) ListAllIgnoredWorkspaceDocs(ctx context.Context, workspaceID string) ([]IgnoredWorkspaceDoc, error) {
	return scanIgnored(r.db.Query(ctx, ignoredDocsSQL, workspaceID))
}

func (r *Repository) UpdateIgnoredWorkspaceDocs(ctx context.Context, workspaceID string, add, remove []string) (int, error) {
	changed := 0
	err := r.inTx(ctx, nil, func(tx pgx.Tx) error {
		if rm := sortedUnique(remove); len(rm) > 0 {
			tag, err := tx.Exec(ctx, `DELETE FROM ai_workspace_ignored_docs WHERE workspace_id=$1 AND doc_id = ANY($2::text[])`, workspaceID, rm)
			if err != nil {
				return err
			}
			changed += int(tag.RowsAffected())
		}
		removed := map[string]bool{}
		for _, id := range remove {
			removed[id] = true
		}
		var kept []string
		for _, id := range add {
			if !removed[id] {
				kept = append(kept, id)
			}
		}
		if ad := sortedUnique(kept); len(ad) > 0 {
			tag, err := tx.Exec(ctx, `INSERT INTO ai_workspace_ignored_docs(workspace_id, doc_id) SELECT $1, unnest($2::text[]) ON CONFLICT DO NOTHING`, workspaceID, ad)
			if err != nil {
				return err
			}
			changed += int(tag.RowsAffected())
		}
		return nil
	})
	return changed, err
}
