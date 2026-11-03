// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package workspaces

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

const adminWorkspaceSQL = `
	SELECT w.id, p.visibility='public', w.created_at, w.name, w.avatar_key, w.enable_ai, p.sharing_enabled, p.url_preview_enabled, w.enable_doc_embedding,
	       o.id, o.name, o.email, o.avatar_url,
	       st.member_count, st.public_page_count, st.snapshot_count, st.snapshot_size, st.blob_count, st.blob_size
	FROM workspaces w
	JOIN workspace_access_policies p ON p.workspace_id=w.id
	LEFT JOIN LATERAL (
		SELECT u.id, u.name, u.email, u.avatar_url FROM workspace_members m JOIN users u ON u.id=m.user_id
		WHERE m.workspace_id=w.id AND m.role='owner' ORDER BY (m.state='active') DESC, m.created_at, m.id LIMIT 1
	) o ON true
	LEFT JOIN LATERAL (
		SELECT (SELECT count(*) FROM workspace_members m WHERE m.workspace_id=w.id AND m.state='active') AS member_count,
		       (SELECT count(*) FROM doc_access_policies d WHERE d.workspace_id=w.id AND d.visibility='public' AND d.public_role='external') AS public_page_count,
		       (SELECT count(*) FROM snapshots s WHERE s.workspace_id=w.id) AS snapshot_count,
		       (SELECT COALESCE(sum(COALESCE(s.size, octet_length(s.blob))),0)::bigint FROM snapshots s WHERE s.workspace_id=w.id) AS snapshot_size,
		       (SELECT count(*) FROM blobs b WHERE b.workspace_id=w.id AND b.deleted_at IS NULL AND b.status='completed') AS blob_count,
		       (SELECT COALESCE(sum(b.size),0)::bigint FROM blobs b WHERE b.workspace_id=w.id AND b.deleted_at IS NULL AND b.status='completed') AS blob_size
	) st ON true`

const adminWorkspaceFilterSQL = `
	WHERE ($1 = '' OR strpos(lower(w.id), lower($1)) > 0 OR strpos(lower(COALESCE(o.id,'')), lower($1)) > 0 OR strpos(lower(COALESCE(o.email,'')), lower($1)) > 0)
	  AND ($2::boolean IS NULL OR (p.visibility='public') = $2)
	  AND ($3::boolean IS NULL OR w.enable_ai = $3)
	  AND ($4::boolean IS NULL OR p.sharing_enabled = $4)
	  AND ($5::boolean IS NULL OR p.url_preview_enabled = $5)
	  AND ($6::boolean IS NULL OR w.enable_doc_embedding = $6)`

func scanAdminWorkspace(row pgx.Row) (AdminWorkspace, error) {
	var w AdminWorkspace
	var oid, oname, oemail *string
	var oavatar *string
	err := row.Scan(&w.ID, &w.Public, &w.CreatedAt, &w.Name, &w.AvatarKey, &w.EnableAI, &w.EnableSharing, &w.EnableURLPreview, &w.EnableDocEmbedding,
		&oid, &oname, &oemail, &oavatar, &w.MemberCount, &w.PublicPageCount, &w.SnapshotCount, &w.SnapshotSize, &w.BlobCount, &w.BlobSize)
	if err != nil {
		return w, err
	}
	if oid != nil {
		w.Owner = &AdminWorkspaceOwner{ID: *oid, Name: deref(oname), Email: deref(oemail), AvatarURL: oavatar}
	}
	return w, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func adminFilterArgs(f AdminWorkspaceFilter) []any {
	keyword := ""
	if f.Keyword != nil {
		keyword = strings.TrimSpace(*f.Keyword)
	}
	return []any{keyword, f.Public, f.EnableAI, f.EnableSharing, f.EnableURLPreview, f.EnableDocEmbedding}
}

var adminOrderColumns = map[string]string{
	"SnapshotSize": "st.snapshot_size", "BlobCount": "st.blob_count", "BlobSize": "st.blob_size",
	"SnapshotCount": "st.snapshot_count", "MemberCount": "st.member_count", "PublicPageCount": "st.public_page_count",
}

// ListAdminWorkspaces lists workspaces for the admin console.
func (r *Repository) ListAdminWorkspaces(ctx context.Context, filter AdminWorkspaceFilter) ([]AdminWorkspace, error) {
	order := "w.created_at DESC, w.id"
	if col, ok := adminOrderColumns[filter.OrderBy]; ok {
		order = col + " DESC NULLS LAST, w.created_at DESC, w.id"
	}
	args := adminFilterArgs(filter)
	var limit *int
	if filter.First > 0 {
		limit = &filter.First
	}
	args = append(args, max(filter.Skip, 0), limit)
	rows, err := r.db.Query(ctx, adminWorkspaceSQL+adminWorkspaceFilterSQL+` ORDER BY `+order+` OFFSET $7 LIMIT $8`, args...)
	if err != nil {
		return nil, fmt.Errorf("list admin workspaces: %w", err)
	}
	defer rows.Close()
	out := []AdminWorkspace{}
	for rows.Next() {
		w, err := scanAdminWorkspace(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// CountAdminWorkspaces counts workspaces matching the filter.
func (r *Repository) CountAdminWorkspaces(ctx context.Context, filter AdminWorkspaceFilter) (int, error) {
	var n int
	err := r.db.QueryRow(ctx, `SELECT count(*) FROM (`+adminWorkspaceSQL+adminWorkspaceFilterSQL+`) x`, adminFilterArgs(filter)...).Scan(&n)
	return n, err
}

// GetAdminWorkspace returns one workspace, or nil when absent.
func (r *Repository) GetAdminWorkspace(ctx context.Context, id string) (*AdminWorkspace, error) {
	w, err := scanAdminWorkspace(r.db.QueryRow(ctx, adminWorkspaceSQL+` WHERE w.id=$1`, id))
	if noRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &w, nil
}

// UpdateAdminWorkspace applies an admin-console edit.
func (r *Repository) UpdateAdminWorkspace(ctx context.Context, id string, update AdminWorkspaceUpdate) error {
	return r.inTx(ctx, nil, func(tx pgx.Tx) error {
		if err := lockExclusive(ctx, tx, domainLockKey(id)); err != nil {
			return err
		}
		if err := lockWorkspaceRow(ctx, tx, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE workspaces SET
			name=CASE WHEN $2 THEN $3 ELSE name END,
			avatar_key=CASE WHEN $4 THEN $5 ELSE avatar_key END,
			enable_ai=COALESCE($6, enable_ai),
			enable_doc_embedding=COALESCE($7, enable_doc_embedding)
			WHERE id=$1`, id, update.NameSet, update.Name, update.AvatarKeySet, update.AvatarKey, update.EnableAI, update.EnableDocEmbedding); err != nil {
			return fmt.Errorf("update admin workspace: %w", err)
		}
		if update.Public == nil && !update.EnableSharingSet && !update.EnableURLPreviewSet {
			return nil
		}
		var visibility *string
		if update.Public != nil {
			v := "private"
			if *update.Public {
				v = "public"
			}
			visibility = &v
		}
		// A Set flag with a nil value writes NULL, which the NOT NULL column
		// rejects and rolls the whole update back.
		_, err := tx.Exec(ctx, `
			INSERT INTO workspace_access_policies(workspace_id, visibility, sharing_enabled, url_preview_enabled)
			VALUES ($1, COALESCE($2,'private'), COALESCE($4,true), COALESCE($6,false))
			ON CONFLICT (workspace_id) DO UPDATE SET
			  visibility=COALESCE($2, workspace_access_policies.visibility),
			  sharing_enabled=CASE WHEN $3 THEN $4 ELSE workspace_access_policies.sharing_enabled END,
			  url_preview_enabled=CASE WHEN $5 THEN $6 ELSE workspace_access_policies.url_preview_enabled END,
			  updated_at=CURRENT_TIMESTAMP`, id, visibility, update.EnableSharingSet, update.EnableSharing, update.EnableURLPreviewSet, update.EnableURLPreview)
		return err
	})
}

// ListAdminWorkspaceSharedLinks lists a workspace's published docs.
func (r *Repository) ListAdminWorkspaceSharedLinks(ctx context.Context, workspaceID string) ([]AdminWorkspaceSharedLink, error) {
	rows, err := r.db.Query(ctx, `
		SELECT p.doc_id, wp.title, p.published_at FROM doc_access_policies p
		LEFT JOIN workspace_pages wp ON wp.workspace_id=p.workspace_id AND wp.page_id=p.doc_id
		WHERE p.workspace_id=$1 AND p.visibility='public' AND p.public_role='external'
		ORDER BY p.published_at DESC NULLS LAST, p.doc_id`, workspaceID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (AdminWorkspaceSharedLink, error) {
		var l AdminWorkspaceSharedLink
		err := row.Scan(&l.DocID, &l.Title, &l.PublishedAt)
		return l, err
	})
}
