// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package workspaces

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const maxIDPage = 1000

// UserWorkspaceIDs lists workspaces the user belongs to or is invited to.
func (r *Repository) UserWorkspaceIDs(ctx context.Context, userID string) ([]string, error) {
	rows, err := r.db.Query(ctx, `
		SELECT workspace_id FROM workspace_members WHERE user_id=$1 AND state='active'
		UNION SELECT workspace_id FROM workspace_invitations WHERE invitee_user_id=$1
		ORDER BY 1`, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// UserWorkspacePage pages UserWorkspaceIDs by id.
func (r *Repository) UserWorkspacePage(ctx context.Context, userID, afterWorkspaceID string, limit int) ([]string, error) {
	if limit < 1 || limit > maxIDPage {
		return nil, fmt.Errorf("workspace page limit must be between 1 and %d", maxIDPage)
	}
	rows, err := r.db.Query(ctx, `
		SELECT id FROM (
			SELECT workspace_id AS id FROM workspace_members WHERE user_id=$1 AND state='active'
			UNION SELECT workspace_id FROM workspace_invitations WHERE invitee_user_id=$1
		) ids WHERE id > $2 ORDER BY id LIMIT $3`, userID, afterWorkspaceID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func collectGrantRefs(rows pgx.Rows, err error) ([]DocGrantRef, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (DocGrantRef, error) {
		var ref DocGrantRef
		err := row.Scan(&ref.WorkspaceID, &ref.DocID)
		return ref, err
	})
}

// DirectGrantDocIDsByUser lists docs the user holds a direct grant on.
func (r *Repository) DirectGrantDocIDsByUser(ctx context.Context, userID string) ([]DocGrantRef, error) {
	return collectGrantRefs(r.db.Query(ctx, `SELECT DISTINCT workspace_id, doc_id FROM doc_grants
		WHERE principal_type='user' AND principal_id=$1 ORDER BY 1, 2`, userID))
}

// DirectGrantDocPage pages DirectGrantDocIDsByUser by (workspace, doc).
func (r *Repository) DirectGrantDocPage(ctx context.Context, userID string, after *DocGrantRef, limit int) ([]DocGrantRef, error) {
	if limit < 1 || limit > maxIDPage {
		return nil, fmt.Errorf("grant page limit must be between 1 and %d", maxIDPage)
	}
	var aw, ad *string
	if after != nil {
		aw, ad = &after.WorkspaceID, &after.DocID
	}
	return collectGrantRefs(r.db.Query(ctx, `SELECT DISTINCT workspace_id, doc_id FROM doc_grants
		WHERE principal_type='user' AND principal_id=$1 AND ($2::text IS NULL OR (workspace_id, doc_id) > ($2::text, $3::text))
		ORDER BY 1, 2 LIMIT $4`, userID, aw, ad, limit))
}

func encodeGrantCursor(t time.Time) string {
	return base64.StdEncoding.EncodeToString([]byte(t.UTC().Format("2006-01-02T15:04:05.000Z")))
}

// DecodeDocGrantCursor parses a doc-grant page cursor.
func DecodeDocGrantCursor(cursor string) (*time.Time, error) {
	if cursor == "" {
		return nil, nil
	}
	raw, err := base64.StdEncoding.DecodeString(cursor)
	if err != nil {
		return nil, fmt.Errorf("decode doc grant cursor: %w", err)
	}
	t, err := time.Parse(time.RFC3339Nano, string(raw))
	if err != nil {
		return nil, fmt.Errorf("parse doc grant cursor: %w", err)
	}
	return &t, nil
}

func validatePage(first, offset int, after *time.Time) error {
	if first < 1 || first > 100 {
		return errors.New("first must be between 1 and 100")
	}
	if offset < 0 {
		return errors.New("offset must not be negative")
	}
	if after != nil && offset > 0 {
		return errors.New("after and offset cannot be combined")
	}
	return nil
}

// listGrants pages direct user grants on a doc; strict demands every
// grantee be an existing enabled user.
func (r *Repository) listGrants(ctx context.Context, workspaceID, docID string, first, offset int, after *time.Time, strict bool) (DocGrantPage, error) {
	page := DocGrantPage{Users: []DocGrantUser{}}
	if err := validatePage(first, offset, after); err != nil {
		return page, err
	}
	join := `JOIN users u ON u.id=g.principal_id`
	if strict {
		join = `LEFT JOIN users u ON u.id=g.principal_id`
	}
	if err := r.db.QueryRow(ctx, `SELECT count(*) FROM doc_grants g `+join+` WHERE g.workspace_id=$1 AND g.doc_id=$2 AND g.principal_type='user'`,
		workspaceID, docID).Scan(&page.TotalCount); err != nil {
		return page, err
	}
	skip := offset
	if after != nil {
		skip = 1
	}
	rows, err := r.db.Query(ctx, `
		SELECT g.principal_id, u.id IS NOT NULL AND NOT u.disabled, COALESCE(u.name,''), COALESCE(u.email,''), u.avatar_url, g.role, g.created_at
		FROM doc_grants g `+join+`
		WHERE g.workspace_id=$1 AND g.doc_id=$2 AND g.principal_type='user' AND ($3::timestamptz IS NULL OR g.created_at >= $3)
		ORDER BY g.created_at, g.principal_id OFFSET $4 LIMIT $5`, workspaceID, docID, after, skip, first)
	if err != nil {
		return page, fmt.Errorf("list doc grants: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var u DocGrantUser
		var usable bool
		if err := rows.Scan(&u.UserID, &usable, &u.Name, &u.Email, &u.AvatarURL, &u.Role, &u.CreatedAt); err != nil {
			return page, err
		}
		if strict && !usable {
			return DocGrantPage{}, ErrAuthorizationUnavailable
		}
		page.Users = append(page.Users, u)
	}
	if err := rows.Err(); err != nil {
		return page, err
	}
	page.HasNext = len(page.Users) == first
	if n := len(page.Users); n > 0 {
		page.EndCursor = ptr(encodeGrantCursor(page.Users[n-1].CreatedAt))
	}
	return page, nil
}

// ListDirectDocGrantUsers pages direct grants without a permission check.
func (r *Repository) ListDirectDocGrantUsers(ctx context.Context, workspaceID, docID string, first, offset int, after *time.Time) (DocGrantPage, error) {
	return r.listGrants(ctx, workspaceID, docID, first, offset, after, false)
}

// ReadDocShareState returns the doc's share state, or nil without a page.
func (r *Repository) ReadDocShareState(ctx context.Context, workspaceID, docID string) (*DocShareState, error) {
	var mode int
	var public bool
	var def *string
	err := r.db.QueryRow(ctx, `
		SELECT wp.mode, COALESCE(p.visibility='public' AND p.public_role='external', false), p.member_default_role
		FROM workspace_pages wp LEFT JOIN doc_access_policies p ON p.workspace_id=wp.workspace_id AND p.doc_id=wp.page_id
		WHERE wp.workspace_id=$1 AND wp.page_id=$2`, workspaceID, docID).Scan(&mode, &public, &def)
	if noRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	state := &DocShareState{Public: public, Mode: "Page", DefaultRole: displayDocRole(def)}
	if mode == 1 {
		state.Mode = "Edgeless"
	}
	return state, nil
}

// ReadDocumentMode returns the page mode, 0 when unknown.
func (r *Repository) ReadDocumentMode(ctx context.Context, workspace, doc string) (int, error) {
	var mode int
	err := r.db.QueryRow(ctx, `SELECT mode FROM workspace_pages WHERE workspace_id=$1 AND page_id=$2`, workspace, doc).Scan(&mode)
	if noRows(err) {
		return 0, nil
	}
	return mode, err
}
