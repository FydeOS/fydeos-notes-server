// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package workspaces

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

const snapshotAttempts = 3

// factsBundle is one consistent read of the access facts of a user in a
// workspace, plus per-document details beyond DocumentACL.
type factsBundle struct {
	snap          WorkspaceSyncSnapshot
	wsPublic      bool
	wsURLPreview  bool
	docURLPreview map[string]bool
	pageExists    map[string]bool
}

func (b factsBundle) doc(docID string, commercial bool) docFacts {
	acl, present := b.snap.Documents[docID]
	return docFacts{
		workspaceExists: b.snap.WorkspaceExists,
		memberRole:      b.snap.WorkspaceRole,
		memberActive:    b.snap.MemberActive,
		sharing:         b.snap.SharingEnabled,
		wsDefault:       b.snap.MemberDefaultDocRole,
		wsURLPreview:    b.wsURLPreview,
		commercial:      commercial,
		present:         present,
		acl:             acl,
		docURLPreview:   b.docURLPreview[docID],
		pageExists:      b.pageExists[docID],
	}
}

func readGeneration(ctx context.Context, q queryer, workspaceID string) (int64, error) {
	var g int64
	err := q.QueryRow(ctx, `SELECT COALESCE((SELECT generation FROM workspace_sync_permission_generations WHERE workspace_id=$1), 0)`, workspaceID).Scan(&g)
	if err != nil {
		return 0, fmt.Errorf("read permission generation: %w", err)
	}
	return g, nil
}

func (r *Repository) sqlCommercial(ctx context.Context, q queryer, workspaceID string) (bool, error) {
	if !r.sqlCommercialShortcut() {
		return false, nil
	}
	var ok bool
	err := q.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM entitlements
			WHERE target_type='workspace' AND target_id=$1
			  AND source IN ('cloud_subscription','admin_grant') AND plan='team'
			  AND (quantity IS NULL OR quantity BETWEEN 1 AND 100000)
			  AND (starts_at IS NULL OR starts_at <= now())
			  AND ((status='active' AND (expires_at IS NULL OR expires_at > now()))
			    OR (status='grace' AND grace_until IS NOT NULL AND grace_until > now())))`, workspaceID).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("read workspace commercial state: %w", err)
	}
	return ok, nil
}

// loadBundle reads the sync snapshot facts; it touches only the columns the
// snapshot needs.
func (r *Repository) loadBundle(ctx context.Context, q queryer, userID, workspaceID string, docIDs []string) (factsBundle, error) {
	b := factsBundle{docURLPreview: map[string]bool{}, pageExists: map[string]bool{}}
	b.snap.Documents = map[string]DocumentACL{}
	exists, err := workspaceExists(ctx, q, workspaceID)
	if err != nil {
		return b, fmt.Errorf("read workspace: %w", err)
	}
	if !exists {
		return b, nil
	}
	b.snap.WorkspaceExists = true
	var sharing *bool
	var wsDefault *string
	err = q.QueryRow(ctx, `SELECT (j->>'sharing_enabled')::boolean, j->>'member_default_doc_role'
		FROM (SELECT to_jsonb(p) AS j FROM workspace_access_policies p WHERE p.workspace_id=$1) x`, workspaceID).Scan(&sharing, &wsDefault)
	if err != nil && !noRows(err) {
		return b, fmt.Errorf("read workspace policy: %w", err)
	}
	b.snap.SharingEnabled = sharing == nil || *sharing
	b.snap.MemberDefaultDocRole = "manager"
	if wsDefault != nil && *wsDefault != "" {
		b.snap.MemberDefaultDocRole = *wsDefault
	}
	if b.snap.WorkspaceRole, b.snap.MemberActive, err = readMembership(ctx, q, workspaceID, userID); err != nil {
		return b, err
	}
	if b.snap.CommercialEntitlement, err = r.sqlCommercial(ctx, q, workspaceID); err != nil {
		return b, err
	}
	ids := sortedUnique(docIDs)
	if len(ids) == 0 {
		return b, nil
	}
	rows, err := q.Query(ctx, `
		SELECT d.id,
		       COALESCE(p.visibility='public', false),
		       COALESCE(p.public_role, ''),
		       (p.published_at IS NOT NULL AND EXISTS (SELECT 1 FROM workspace_pages wp WHERE wp.workspace_id=$1 AND wp.page_id=d.id AND wp.published_at IS NOT NULL)),
		       COALESCE(p.member_default_role, ''),
		       COALESCE((SELECT g.role FROM doc_grants g WHERE g.workspace_id=$1 AND g.doc_id=d.id AND g.principal_type='user' AND g.principal_id=$3 LIMIT 1), '')
		FROM unnest($2::text[]) AS d(id)
		LEFT JOIN doc_access_policies p ON p.workspace_id=$1 AND p.doc_id=d.id`,
		workspaceID, ids, userID)
	if err != nil {
		return b, fmt.Errorf("read document acl: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var acl DocumentACL
		if err := rows.Scan(&id, &acl.VisibilityPublic, &acl.PublicRole, &acl.Published, &acl.MemberDefaultRole, &acl.ExplicitUserRole); err != nil {
			return b, fmt.Errorf("scan document acl: %w", err)
		}
		if userID == "" {
			acl.ExplicitUserRole = ""
		}
		b.snap.Documents[id] = acl
	}
	return b, rows.Err()
}

func readMembership(ctx context.Context, q queryer, workspaceID, userID string) (string, bool, error) {
	if userID == "" {
		return "", false, nil
	}
	var role, state string
	err := q.QueryRow(ctx, `
		SELECT role, state FROM workspace_members WHERE workspace_id=$1 AND user_id=$2
		ORDER BY (state='active') DESC, updated_at DESC LIMIT 1`, workspaceID, userID).Scan(&role, &state)
	if noRows(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read workspace membership: %w", err)
	}
	return role, state == "active", nil
}

// workspaceExposure reads the public and URL-preview switches.
func workspaceExposure(ctx context.Context, q queryer, workspaceID string) (public, sharing, preview bool, err error) {
	// Read the row as JSON so deployments that lack optional policy columns
	// fall back to their defaults.
	var vis *string
	var sh, pv *bool
	err = q.QueryRow(ctx, `SELECT j->>'visibility', (j->>'sharing_enabled')::boolean, (j->>'url_preview_enabled')::boolean
		FROM (SELECT to_jsonb(p) AS j FROM workspace_access_policies p WHERE p.workspace_id=$1) x`, workspaceID).Scan(&vis, &sh, &pv)
	if noRows(err) {
		return false, true, false, nil
	}
	if err != nil {
		return false, false, false, fmt.Errorf("read workspace policy: %w", err)
	}
	return vis != nil && *vis == "public", sh == nil || *sh, pv != nil && *pv, nil
}

// loadPreviewExtras adds URL-preview and page-existence facts.
func loadPreviewExtras(ctx context.Context, q queryer, b *factsBundle, workspaceID string, docIDs []string) error {
	if !b.snap.WorkspaceExists {
		return nil
	}
	var err error
	if b.wsPublic, _, b.wsURLPreview, err = workspaceExposure(ctx, q, workspaceID); err != nil {
		return err
	}
	ids := sortedUnique(docIDs)
	if len(ids) == 0 {
		return nil
	}
	rows, err := q.Query(ctx, `SELECT page_id FROM workspace_pages WHERE workspace_id=$1 AND page_id = ANY($2::text[])`, workspaceID, ids)
	if err != nil {
		return fmt.Errorf("read workspace pages: %w", err)
	}
	pages, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, id := range pages {
		b.pageExists[id] = true
	}
	rows, err = q.Query(ctx, `SELECT doc_id FROM doc_access_policies p WHERE workspace_id=$1 AND doc_id = ANY($2::text[]) AND COALESCE((to_jsonb(p)->>'url_preview_enabled')::boolean, false)`, workspaceID, ids)
	if err != nil {
		return fmt.Errorf("read document preview policy: %w", err)
	}
	previews, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, id := range previews {
		b.docURLPreview[id] = true
	}
	return nil
}

func (r *Repository) readBundleFenced(ctx context.Context, q queryer, userID, workspaceID string, docIDs []string) (factsBundle, error) {
	for attempt := 0; attempt < snapshotAttempts; attempt++ {
		before, err := readGeneration(ctx, q, workspaceID)
		if err != nil {
			return factsBundle{}, err
		}
		b, err := r.loadBundle(ctx, q, userID, workspaceID, docIDs)
		if err != nil {
			return factsBundle{}, err
		}
		if !b.snap.WorkspaceExists {
			return factsBundle{}, ErrNotFound
		}
		after, err := readGeneration(ctx, q, workspaceID)
		if err != nil {
			return factsBundle{}, err
		}
		if before == after {
			b.snap.Generation = after
			return b, nil
		}
	}
	return factsBundle{}, ErrPermissionGenerationChanged
}

// ReadWorkspaceSyncSnapshot reads a generation-consistent access snapshot.
func (r *Repository) ReadWorkspaceSyncSnapshot(ctx context.Context, userID, workspaceID string, docIDs []string) (WorkspaceSyncSnapshot, error) {
	b, err := r.readBundleFenced(ctx, r.db, userID, workspaceID, docIDs)
	return b.snap, err
}

// ReadWorkspaceSyncSnapshotInTx reads the snapshot inside tx and pins the
// generation row with a share lock.
func (r *Repository) ReadWorkspaceSyncSnapshotInTx(ctx context.Context, tx pgx.Tx, userID, workspaceID string, docIDs []string) (WorkspaceSyncSnapshot, error) {
	b, err := r.readBundleFenced(ctx, tx, userID, workspaceID, docIDs)
	if err != nil {
		return WorkspaceSyncSnapshot{}, err
	}
	var locked int64
	err = tx.QueryRow(ctx, `SELECT generation FROM workspace_sync_permission_generations WHERE workspace_id=$1 FOR SHARE`, workspaceID).Scan(&locked)
	if err != nil && !noRows(err) {
		return WorkspaceSyncSnapshot{}, fmt.Errorf("lock permission generation: %w", err)
	}
	if locked != b.snap.Generation {
		return WorkspaceSyncSnapshot{}, ErrPermissionGenerationChanged
	}
	return b.snap, nil
}

// SyncPermissionGeneration returns the workspace's permission generation,
// creating the counter row when needed.
func (r *Repository) SyncPermissionGeneration(ctx context.Context, workspaceID string) (int64, error) {
	var g *int64
	err := r.db.QueryRow(ctx, `
		WITH ins AS (
			INSERT INTO workspace_sync_permission_generations(workspace_id, generation)
			SELECT id, 0 FROM workspaces WHERE id=$1
			ON CONFLICT (workspace_id) DO NOTHING
			RETURNING generation
		)
		SELECT COALESCE((SELECT generation FROM ins), (SELECT generation FROM workspace_sync_permission_generations WHERE workspace_id=$1))`, workspaceID).Scan(&g)
	if err != nil {
		return 0, fmt.Errorf("read permission generation: %w", err)
	}
	if g == nil {
		return 0, ErrNotFound
	}
	return *g, nil
}

// CanReadWorkspaceInTx reports whether the user may read the workspace.
func (r *Repository) CanReadWorkspaceInTx(ctx context.Context, tx pgx.Tx, userID, workspaceID string) (bool, error) {
	exists, err := workspaceExists(ctx, tx, workspaceID)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, ErrNotFound
	}
	role, active, err := readMembership(ctx, tx, workspaceID, userID)
	if err != nil {
		return false, err
	}
	if active && isMemberRole(role) {
		return true, nil
	}
	public, sharing, _, err := workspaceExposure(ctx, tx, workspaceID)
	return public && sharing, err
}

// CanManageWorkspaceUsers reports whether the user may manage members.
func (r *Repository) CanManageWorkspaceUsers(ctx context.Context, userID, workspaceID string) (bool, error) {
	ok, err := workspaceExists(ctx, r.db, workspaceID)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, ErrNotFound
	}
	role, err := activeRole(ctx, r.db, workspaceID, userID)
	if err != nil {
		return false, err
	}
	switch role {
	case "owner":
		return true, nil
	case "admin":
		return r.WorkspaceCommercialEntitlement(ctx, workspaceID)
	}
	return false, nil
}

// teamFlag evaluates whether the workspace is a team workspace and whether
// that is currently known.
func teamFlag(ctx context.Context, q queryer, workspaceID string) (team, known bool, err error) {
	var (
		rowKnown, stale, usesOwner *bool
		hasEntitlement             bool
	)
	err = q.QueryRow(ctx, `
		SELECT q.known, q.stale, q.uses_owner_quota,
		       EXISTS (SELECT 1 FROM entitlements e
		               WHERE e.target_type='workspace' AND e.target_id=$1 AND e.plan IN ('team','selfhost_team')
		                 AND e.status NOT IN ('revoked','expired')
		                 AND (e.starts_at IS NULL OR e.starts_at <= now())
		                 AND (e.expires_at IS NULL OR e.expires_at > now()))
		FROM (SELECT 1) one
		LEFT JOIN effective_workspace_quota_states q ON q.workspace_id=$1`, workspaceID).Scan(&rowKnown, &stale, &usesOwner, &hasEntitlement)
	if err != nil {
		return false, false, fmt.Errorf("read workspace team state: %w", err)
	}
	if rowKnown == nil {
		if !hasEntitlement {
			return false, true, nil
		}
		return false, false, nil
	}
	if !*rowKnown || *stale {
		return false, false, nil
	}
	return !*usesOwner, true, nil
}

// ReadWorkspaceAccessFacts collects what workspace-level authorization needs.
func (r *Repository) ReadWorkspaceAccessFacts(ctx context.Context, userID, workspaceID string) (WorkspaceAccessFacts, error) {
	var facts WorkspaceAccessFacts
	var err error
	if facts.WorkspaceExists, err = workspaceExists(ctx, r.db, workspaceID); err != nil || !facts.WorkspaceExists {
		return facts, err
	}
	if facts.WorkspaceRole, facts.MemberActive, err = readMembership(ctx, r.db, workspaceID, userID); err != nil {
		return WorkspaceAccessFacts{}, err
	}
	if facts.WorkspacePublic, facts.SharingEnabled, facts.URLPreviewEnabled, err = workspaceExposure(ctx, r.db, workspaceID); err != nil {
		return WorkspaceAccessFacts{}, err
	}
	if facts.CommercialEntitlement, err = r.WorkspaceCommercialEntitlement(ctx, workspaceID); err != nil {
		return WorkspaceAccessFacts{}, err
	}
	if facts.Team, facts.TeamKnown, err = teamFlag(ctx, r.db, workspaceID); err != nil {
		return WorkspaceAccessFacts{}, err
	}
	return facts, nil
}

// ReadWorkspaceConfig returns workspace feature switches with defaults.
func (r *Repository) ReadWorkspaceConfig(ctx context.Context, workspaceID string) (WorkspaceConfig, error) {
	var cfg WorkspaceConfig
	var ai, embedding, sharing, preview *bool
	err := r.db.QueryRow(ctx, `
		SELECT w.enable_ai, w.enable_doc_embedding, p.sharing_enabled, p.url_preview_enabled
		FROM workspaces w LEFT JOIN workspace_access_policies p ON p.workspace_id=w.id WHERE w.id=$1`, workspaceID).
		Scan(&ai, &embedding, &sharing, &preview)
	if noRows(err) {
		return cfg, ErrNotFound
	}
	if err != nil {
		return cfg, fmt.Errorf("read workspace config: %w", err)
	}
	cfg.EnableAI = ai != nil && *ai
	cfg.EnableDocEmbedding = embedding != nil && *embedding
	cfg.EnableSharing = sharing == nil || *sharing
	cfg.EnableURLPreview = preview != nil && *preview
	return cfg, nil
}

// HasCommonActiveWorkspace reports whether two users are both active in
// some workspace.
func (r *Repository) HasCommonActiveWorkspace(ctx context.Context, viewerID, targetID string) (bool, error) {
	var ok bool
	err := r.db.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM workspace_members a JOIN workspace_members b ON b.workspace_id=a.workspace_id
		               WHERE a.user_id=$1 AND a.state='active' AND b.user_id=$2 AND b.state='active')`, viewerID, targetID).Scan(&ok)
	return ok, err
}
