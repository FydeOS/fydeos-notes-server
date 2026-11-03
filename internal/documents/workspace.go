// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package documents

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/crdt/yjs"
	"github.com/fydeos/fydeos-notes-server/internal/entitlements"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

var (
	ErrWorkspaceNotFound                    = errors.New("workspace not found")
	ErrWorkspaceDocNotFound                 = errors.New("workspace document not found")
	ErrWorkspacePermissionGenerationChanged = errors.New("workspace permission generation changed")
	ErrWorkspaceRootCommandRequired         = errors.New("workspace root document requires a lifecycle command")
	ErrWorkspaceLockTimeout                 = errors.New("workspace document lock timed out")
	ErrWorkspaceRootNotFound                = errors.New("workspace root document not found")
	ErrWorkspaceQuotaReadonly               = errors.New("workspace is readonly due to quota limits")
	ErrWorkspaceQuotaStateUnknown           = errors.New("workspace quota state is unknown or stale")
)

type WorkspaceDocLifecycle string

const (
	WorkspaceDocTrash   WorkspaceDocLifecycle = "trash"
	WorkspaceDocRestore WorkspaceDocLifecycle = "restore"
	WorkspaceDocDelete  WorkspaceDocLifecycle = "delete"
)

type WorkspaceDocLifecycleResult struct {
	RootUpdate []byte
	Timestamp  int64
}

type WorkspacePushResult struct {
	Timestamp                int64
	WorkspaceMetadataChanged bool
}

type workspaceDocumentDB interface {
	Begin(context.Context) (pgx.Tx, error)
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type WorkspaceRepository struct {
	db              workspaceDocumentDB
	quotaDeployment entitlements.Deployment
}

func NewWorkspaceRepository(db workspaceDocumentDB) *WorkspaceRepository {
	return NewWorkspaceRepositoryWithEntitlementConfig(db, "selfhosted")
}

func NewWorkspaceRepositoryWithEntitlementConfig(db workspaceDocumentDB, deployment string) *WorkspaceRepository {
	return &WorkspaceRepository{
		db:              db,
		quotaDeployment: entitlements.ParseDeployment(deployment),
	}
}

func (r *WorkspaceRepository) CurrentPermissionGeneration(ctx context.Context, workspaceID string) (int64, error) {
	var exists bool
	if err := r.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspaces WHERE id=$1)`, workspaceID).Scan(&exists); err != nil {
		return 0, err
	}
	if !exists {
		return 0, ErrWorkspaceNotFound
	}
	if _, err := r.db.Exec(ctx, `
		INSERT INTO workspace_sync_permission_generations(workspace_id, generation)
		VALUES ($1, 0) ON CONFLICT (workspace_id) DO NOTHING`, workspaceID); err != nil {
		return 0, err
	}
	var generation int64
	err := r.db.QueryRow(ctx, `SELECT generation FROM workspace_sync_permission_generations WHERE workspace_id=$1`, workspaceID).Scan(&generation)
	return generation, err
}

func (r *WorkspaceRepository) DiffWorkspace(ctx context.Context, workspaceID, docID string, stateVector []byte) ([]byte, []byte, int64, error) {
	rows, err := r.db.Query(ctx, `
		SELECT blob, timestamp FROM (
			SELECT blob, updated_at AS timestamp FROM snapshots WHERE workspace_id=$1 AND guid=$2
			UNION ALL
			SELECT blob, created_at AS timestamp FROM updates WHERE workspace_id=$1 AND guid=$2
		) source ORDER BY timestamp ASC`, workspaceID, docID)
	if err != nil {
		return nil, nil, 0, err
	}
	defer rows.Close()
	updates := make([][]byte, 0, 2)
	var timestamp time.Time
	for rows.Next() {
		var blob []byte
		var updatedAt time.Time
		if err := rows.Scan(&blob, &updatedAt); err != nil {
			return nil, nil, 0, err
		}
		updates = append(updates, blob)
		if updatedAt.After(timestamp) {
			timestamp = updatedAt
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, 0, err
	}
	if len(updates) == 0 {
		return nil, nil, 0, ErrWorkspaceDocNotFound
	}
	merged, err := yjs.MergeUpdates(updates)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("merge workspace document updates: %w", err)
	}
	doc, err := yjs.Restore(merged, nil)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("restore workspace document: %w", err)
	}
	missing := merged
	if stateVector != nil {
		missing, err = doc.Diff(stateVector)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("diff workspace document: %w", err)
		}
	}
	return missing, doc.StateVector(), timestamp.UnixMilli(), nil
}

func (r *WorkspaceRepository) WorkspaceTimestamps(ctx context.Context, workspaceID string, after *int64) (map[string]int64, error) {
	var cursor *time.Time
	if after != nil {
		value := time.UnixMilli(*after)
		cursor = &value
	}
	rows, err := r.db.Query(ctx, `
		SELECT guid, max(timestamp) FROM (
			SELECT guid, updated_at AS timestamp FROM snapshots WHERE workspace_id=$1
			UNION ALL
			SELECT guid, created_at AS timestamp FROM updates WHERE workspace_id=$1
		) source
		WHERE $2::timestamptz IS NULL OR timestamp > $2
		GROUP BY guid`, workspaceID, cursor)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]int64)
	for rows.Next() {
		var docID string
		var timestamp time.Time
		if err := rows.Scan(&docID, &timestamp); err != nil {
			return nil, err
		}
		result[docID] = timestamp.UnixMilli()
	}
	return result, rows.Err()
}

func (r *WorkspaceRepository) PushWorkspace(ctx context.Context, actorID, workspaceID, docID string, update []byte, expectedGeneration int64) (WorkspacePushResult, error) {
	if len(update) == 0 || (len(update) == 2 && update[0] == 0 && update[1] == 0) {
		return WorkspacePushResult{}, nil
	}
	if len(update) > MaxUpdateBytes || yjs.ValidateUpdate(update) != nil {
		return WorkspacePushResult{}, ErrInvalidDoc
	}
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return WorkspacePushResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockWorkspaceStorageShared(ctx, tx, workspaceID); err != nil {
		return WorkspacePushResult{}, err
	}
	if err := lockWorkspaceDocumentUpdate(ctx, tx, workspaceID, docID); err != nil {
		return WorkspacePushResult{}, err
	}
	if workspaceID == docID {
		return pushWorkspaceRootUpdate(ctx, tx, actorID, workspaceID, update, expectedGeneration)
	}
	var exists bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM snapshots WHERE workspace_id=$1 AND guid=$2)
		    OR EXISTS(SELECT 1 FROM updates WHERE workspace_id=$1 AND guid=$2)`, workspaceID, docID).Scan(&exists); err != nil {
		return WorkspacePushResult{}, err
	}
	if !exists {
		if err := lockWorkspacePermissionGeneration(ctx, tx, workspaceID, expectedGeneration); err != nil {
			return WorkspacePushResult{}, err
		}
		var timestamp time.Time
		if err := tx.QueryRow(ctx, `
			SELECT GREATEST(
				date_trunc('milliseconds', clock_timestamp()),
				COALESCE(max(created_at) + interval '1 millisecond', 'epoch'::timestamptz)
			) FROM updates WHERE workspace_id=$1 AND guid=$2`, workspaceID, docID).Scan(&timestamp); err != nil {
			return WorkspacePushResult{}, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO snapshots(workspace_id,guid,blob,size,updated_at,created_by,updated_by)
			VALUES($1,$2,$3,$4,$5,$6,$6)`, workspaceID, docID, update, len(update), timestamp, actorID); err != nil {
			return WorkspacePushResult{}, err
		}
		if err := invalidateWorkspaceDocumentProjection(ctx, tx, workspaceID, docID); err != nil {
			return WorkspacePushResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return WorkspacePushResult{}, err
		}
		return WorkspacePushResult{Timestamp: timestamp.UnixMilli()}, nil
	}
	if err := lockWorkspacePermissionGeneration(ctx, tx, workspaceID, expectedGeneration); err != nil {
		return WorkspacePushResult{}, err
	}
	var timestamp time.Time
	if err := tx.QueryRow(ctx, `
		SELECT GREATEST(
			date_trunc('milliseconds', clock_timestamp()),
			COALESCE(max(created_at) + interval '1 millisecond', 'epoch'::timestamptz)
		) FROM updates WHERE workspace_id=$1 AND guid=$2`, workspaceID, docID).Scan(&timestamp); err != nil {
		return WorkspacePushResult{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO updates(workspace_id,guid,blob,created_at,created_by) VALUES($1,$2,$3,$4,$5)`, workspaceID, docID, update, timestamp, actorID); err != nil {
		return WorkspacePushResult{}, err
	}
	if err := invalidateWorkspaceDocumentProjection(ctx, tx, workspaceID, docID); err != nil {
		return WorkspacePushResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return WorkspacePushResult{}, err
	}
	return WorkspacePushResult{Timestamp: timestamp.UnixMilli()}, nil
}

func (r *WorkspaceRepository) ApplyWorkspaceDocLifecycle(ctx context.Context, actorID, workspaceID, docID string, lifecycle WorkspaceDocLifecycle, expectedGeneration int64) (WorkspaceDocLifecycleResult, error) {
	if lifecycle != WorkspaceDocTrash && lifecycle != WorkspaceDocRestore && lifecycle != WorkspaceDocDelete {
		return WorkspaceDocLifecycleResult{}, ErrInvalidDoc
	}
	if workspaceID == docID {
		return WorkspaceDocLifecycleResult{}, ErrInvalidDoc
	}
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return WorkspaceDocLifecycleResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockWorkspaceStorageShared(ctx, tx, workspaceID); err != nil {
		return WorkspaceDocLifecycleResult{}, err
	}
	lockedDocIDs := []string{workspaceID, docID}
	if lockedDocIDs[1] < lockedDocIDs[0] {
		lockedDocIDs[0], lockedDocIDs[1] = lockedDocIDs[1], lockedDocIDs[0]
	}
	for _, lockedDocID := range lockedDocIDs {
		if err := lockWorkspaceDocumentUpdate(ctx, tx, workspaceID, lockedDocID); err != nil {
			return WorkspaceDocLifecycleResult{}, err
		}
	}
	if err := lockWorkspaceDomain(ctx, tx, workspaceID); err != nil {
		return WorkspaceDocLifecycleResult{}, err
	}
	if lifecycle == WorkspaceDocRestore {
		if err := r.CheckWorkspaceMutationQuotaInTx(ctx, tx, workspaceID); err != nil {
			return WorkspaceDocLifecycleResult{}, err
		}
	}
	if err := lockWorkspacePermissionGeneration(ctx, tx, workspaceID, expectedGeneration); err != nil {
		return WorkspaceDocLifecycleResult{}, err
	}

	var snapshot []byte
	err = tx.QueryRow(ctx, `SELECT blob FROM snapshots WHERE workspace_id=$1 AND guid=$1 FOR UPDATE`, workspaceID).Scan(&snapshot)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkspaceDocLifecycleResult{}, ErrWorkspaceRootNotFound
	}
	if err != nil {
		return WorkspaceDocLifecycleResult{}, err
	}
	rows, err := tx.Query(ctx, `SELECT blob FROM updates WHERE workspace_id=$1 AND guid=$1 ORDER BY created_at FOR SHARE`, workspaceID)
	if err != nil {
		return WorkspaceDocLifecycleResult{}, err
	}
	rootUpdates := make([][]byte, 0)
	for rows.Next() {
		var update []byte
		if err := rows.Scan(&update); err != nil {
			rows.Close()
			return WorkspaceDocLifecycleResult{}, err
		}
		rootUpdates = append(rootUpdates, update)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return WorkspaceDocLifecycleResult{}, err
	}
	rows.Close()
	rootUpdate, err := mutateWorkspaceRootLifecycle(snapshot, rootUpdates, docID, lifecycle)
	if err != nil {
		return WorkspaceDocLifecycleResult{}, err
	}
	var timestamp time.Time
	if err := tx.QueryRow(ctx, `
		SELECT GREATEST(
			date_trunc('milliseconds', clock_timestamp()),
			COALESCE(max(created_at) + interval '1 millisecond', 'epoch'::timestamptz)
		) FROM updates WHERE workspace_id=$1 AND guid=$1`, workspaceID).Scan(&timestamp); err != nil {
		return WorkspaceDocLifecycleResult{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO updates(workspace_id,guid,blob,created_at,created_by) VALUES($1,$1,$2,$3,$4)`, workspaceID, rootUpdate, timestamp, actorID); err != nil {
		return WorkspaceDocLifecycleResult{}, err
	}
	if err := invalidateWorkspaceDocumentProjection(ctx, tx, workspaceID, workspaceID); err != nil {
		return WorkspaceDocLifecycleResult{}, err
	}
	if lifecycle == WorkspaceDocDelete {
		for _, statement := range []string{
			`DELETE FROM replies WHERE workspace_id=$1 AND doc_id=$2`,
			`DELETE FROM comments WHERE workspace_id=$1 AND doc_id=$2`,
			`DELETE FROM updates WHERE workspace_id=$1 AND guid=$2`,
			`DELETE FROM snapshot_histories WHERE workspace_id=$1 AND guid=$2`,
			`DELETE FROM snapshots WHERE workspace_id=$1 AND guid=$2`,
			`DELETE FROM doc_grants WHERE workspace_id=$1 AND doc_id=$2`,
			`DELETE FROM doc_access_policies WHERE workspace_id=$1 AND doc_id=$2`,
			`DELETE FROM workspace_pages WHERE workspace_id=$1 AND page_id=$2`,
		} {
			if _, err := tx.Exec(ctx, statement, workspaceID, docID); err != nil {
				return WorkspaceDocLifecycleResult{}, err
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return WorkspaceDocLifecycleResult{}, err
	}
	return WorkspaceDocLifecycleResult{RootUpdate: rootUpdate, Timestamp: timestamp.UnixMilli()}, nil
}

// CheckWorkspaceMutationQuotaInTx checks canonical usage under the owner/workspace
// locks. Call before locking permission generations or writing domain rows.
func (r *WorkspaceRepository) CheckWorkspaceMutationQuotaInTx(ctx context.Context, tx pgx.Tx, workspaceID string) error {
	var ownerID string
	if err := tx.QueryRow(ctx, `SELECT user_id FROM workspace_members WHERE workspace_id=$1 AND role='owner' AND state='active' LIMIT 1`, workspaceID).Scan(&ownerID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrWorkspaceQuotaStateUnknown
		}
		return err
	}
	var lockedID string
	// Serialize quota admission without blocking FK KEY SHARE checks by document
	// updates that already hold the permission-generation fence. FOR UPDATE here
	// would form an owner-row -> generation -> owner-row deadlock with sync.
	if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 FOR NO KEY UPDATE`, ownerID).Scan(&lockedID); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, workspaceID).Scan(&lockedID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrWorkspaceNotFound
		}
		return err
	}
	var confirmedOwnerID string
	if err := tx.QueryRow(ctx, `SELECT user_id FROM workspace_members WHERE workspace_id=$1 AND role='owner' AND state='active' LIMIT 1`, workspaceID).Scan(&confirmedOwnerID); err != nil {
		return err
	}
	if confirmedOwnerID != ownerID {
		return ErrWorkspaceQuotaStateUnknown
	}
	now, err := loadQuotaDecisionTime(ctx, tx)
	if err != nil {
		return err
	}
	ownerGrant, err := r.loadQuotaGrant(ctx, tx, "user", ownerID, workspaceID, now)
	if err != nil {
		return err
	}
	workspaceGrant, err := r.loadQuotaGrant(ctx, tx, "workspace", workspaceID, workspaceID, now)
	if err != nil {
		return err
	}
	grant := ownerGrant
	workspaceIDs := []string{workspaceID}
	if workspaceGrant.Commercial {
		grant = workspaceGrant
	} else {
		ownedWorkspaceIDs, err := loadOwnedWorkspaceIDs(ctx, tx, ownerID)
		if err != nil {
			return err
		}
		workspaceIDs = workspaceIDs[:0]
		currentWorkspaceIncluded := false
		for _, ownedWorkspaceID := range ownedWorkspaceIDs {
			quota, err := r.loadQuotaGrant(ctx, tx, "workspace", ownedWorkspaceID, ownedWorkspaceID, now)
			if err != nil {
				return err
			}
			if !quota.Commercial {
				workspaceIDs = append(workspaceIDs, ownedWorkspaceID)
				if ownedWorkspaceID == workspaceID {
					currentWorkspaceIncluded = true
				}
			}
		}
		if !currentWorkspaceIncluded {
			return ErrWorkspaceQuotaStateUnknown
		}
	}

	var chargedSeats int64
	if err := tx.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM workspace_members WHERE workspace_id=$1 AND state='active')
		     + (SELECT count(*) FROM workspace_invitations WHERE workspace_id=$1
		        AND status::text = ANY($2::text[]))`, workspaceID,
		[]string{"pending", "waiting_review", "waiting_seat"}).Scan(&chargedSeats); err != nil {
		return err
	}
	var usedStorage int64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE((SELECT SUM(size)::bigint FROM blobs WHERE workspace_id=ANY($1) AND deleted_at IS NULL
		                 AND (status='completed' OR (status='pending' AND reservation_expires_at > $2))),0)
		     + COALESCE((SELECT SUM(size)::bigint FROM comment_attachments WHERE workspace_id=ANY($1) AND deleted_at IS NULL
		                 AND (status='completed' OR (status='pending' AND reservation_expires_at > $2))),0)`, workspaceIDs, now).Scan(&usedStorage); err != nil {
		return err
	}
	if chargedSeats > int64(grant.Limits.SeatLimit) || usedStorage > grant.Limits.StorageQuota {
		return ErrWorkspaceQuotaReadonly
	}
	return nil
}

func (r *WorkspaceRepository) loadQuotaGrant(ctx context.Context, tx pgx.Tx, targetType, targetID, workspaceID string, now time.Time) (entitlements.Grant, error) {
	rows, err := tx.Query(ctx, `
		SELECT source, status, plan, quantity, starts_at, expires_at, grace_until, signed_payload
		FROM entitlements WHERE target_type=$1 AND target_id=$2 ORDER BY updated_at DESC`, targetType, targetID)
	if err != nil {
		return entitlements.Grant{}, err
	}
	facts := make([]entitlements.Fact, 0)
	for rows.Next() {
		var fact entitlements.Fact
		var quantity pgtype.Int4
		var startsAt, expiresAt, graceUntil pgtype.Timestamptz
		if err := rows.Scan(&fact.Source, &fact.Status, &fact.Plan, &quantity, &startsAt, &expiresAt, &graceUntil, &fact.SignedPayload); err != nil {
			rows.Close()
			return entitlements.Grant{}, err
		}
		if quantity.Valid {
			value := quantity.Int32
			fact.Quantity = &value
		}
		if startsAt.Valid {
			value := startsAt.Time
			fact.StartsAt = &value
		}
		if expiresAt.Valid {
			value := expiresAt.Time
			fact.ExpiresAt = &value
		}
		if graceUntil.Valid {
			value := graceUntil.Time
			fact.GraceUntil = &value
		}
		facts = append(facts, fact)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return entitlements.Grant{}, err
	}
	rows.Close()
	return entitlements.Resolve(entitlements.Context{
		Deployment:  r.quotaDeployment,
		TargetType:  targetType,
		WorkspaceID: workspaceID,
		Now:         now,
	}, facts), nil
}

func loadQuotaDecisionTime(ctx context.Context, tx pgx.Tx) (time.Time, error) {
	var now time.Time
	err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now)
	return now, err
}

func loadOwnedWorkspaceIDs(ctx context.Context, tx pgx.Tx, ownerID string) ([]string, error) {
	rows, err := tx.Query(ctx, `
		SELECT workspace_id FROM workspace_members
		WHERE user_id=$1 AND role='owner' AND state='active' ORDER BY workspace_id`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	workspaceIDs := make([]string, 0)
	for rows.Next() {
		var workspaceID string
		if err := rows.Scan(&workspaceID); err != nil {
			return nil, err
		}
		workspaceIDs = append(workspaceIDs, workspaceID)
	}
	return workspaceIDs, rows.Err()
}

func pushWorkspaceRootUpdate(ctx context.Context, tx pgx.Tx, actorID, workspaceID string, update []byte, expectedGeneration int64) (WorkspacePushResult, error) {
	// Root updates also write workspace metadata. Serialize with ACL commands
	// before taking the generation fence to avoid reversing their lock order.
	if err := lockWorkspaceDomain(ctx, tx, workspaceID); err != nil {
		return WorkspacePushResult{}, err
	}
	if err := lockWorkspacePermissionGeneration(ctx, tx, workspaceID, expectedGeneration); err != nil {
		return WorkspacePushResult{}, err
	}
	var snapshot []byte
	err := tx.QueryRow(ctx, `SELECT blob FROM snapshots WHERE workspace_id=$1 AND guid=$1 FOR UPDATE`, workspaceID).Scan(&snapshot)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return WorkspacePushResult{}, err
	}
	hasSnapshot := err == nil
	rows, err := tx.Query(ctx, `SELECT blob FROM updates WHERE workspace_id=$1 AND guid=$1 ORDER BY created_at FOR SHARE`, workspaceID)
	if err != nil {
		return WorkspacePushResult{}, err
	}
	updates := make([][]byte, 0)
	for rows.Next() {
		var blob []byte
		if err := rows.Scan(&blob); err != nil {
			rows.Close()
			return WorkspacePushResult{}, err
		}
		updates = append(updates, blob)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return WorkspacePushResult{}, err
	}
	rows.Close()
	merged, err := validateWorkspaceRootAppend(snapshot, updates, update)
	if err != nil {
		return WorkspacePushResult{}, err
	}
	previousUpdates := make([][]byte, 0, len(updates)+1)
	if len(snapshot) > 0 {
		previousUpdates = append(previousUpdates, snapshot)
	}
	previousUpdates = append(previousUpdates, updates...)
	var previousMetadata WorkspaceMetadata
	previousFound := false
	if len(previousUpdates) > 0 {
		previousMetadata, previousFound, err = ProjectWorkspaceMetadata(previousUpdates)
		if err != nil {
			return WorkspacePushResult{}, fmt.Errorf("project previous workspace metadata: %w", err)
		}
	}
	currentMetadata, currentFound, err := ProjectWorkspaceMetadata([][]byte{merged})
	if err != nil {
		return WorkspacePushResult{}, fmt.Errorf("project current workspace metadata: %w", err)
	}
	metadataChanged := currentFound && (!previousFound || currentMetadata.Name != previousMetadata.Name || currentMetadata.AvatarKey != previousMetadata.AvatarKey)
	var timestamp time.Time
	if err := tx.QueryRow(ctx, `
		SELECT GREATEST(
			date_trunc('milliseconds', clock_timestamp()),
			COALESCE(max(created_at) + interval '1 millisecond', 'epoch'::timestamptz)
		) FROM updates WHERE workspace_id=$1 AND guid=$1`, workspaceID).Scan(&timestamp); err != nil {
		return WorkspacePushResult{}, err
	}
	if !hasSnapshot {
		_, err = tx.Exec(ctx, `
			INSERT INTO snapshots(workspace_id,guid,blob,size,updated_at,created_by,updated_by)
			VALUES($1,$1,$2,$3,$4,$5,$5)`, workspaceID, merged, len(merged), timestamp, actorID)
	} else {
		_, err = tx.Exec(ctx, `
			INSERT INTO updates(workspace_id,guid,blob,created_at,created_by)
			VALUES($1,$1,$2,$3,$4)`, workspaceID, update, timestamp, actorID)
	}
	if err != nil {
		return WorkspacePushResult{}, err
	}
	if metadataChanged {
		if _, err := tx.Exec(ctx, `UPDATE workspaces SET name=$2,avatar_key=$3 WHERE id=$1`, workspaceID, currentMetadata.Name, currentMetadata.AvatarKey); err != nil {
			return WorkspacePushResult{}, err
		}
	}
	if err := invalidateWorkspaceDocumentProjection(ctx, tx, workspaceID, workspaceID); err != nil {
		return WorkspacePushResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return WorkspacePushResult{}, err
	}
	return WorkspacePushResult{Timestamp: timestamp.UnixMilli(), WorkspaceMetadataChanged: metadataChanged}, nil
}

func invalidateWorkspaceDocumentProjection(ctx context.Context, tx pgx.Tx, workspaceID, docID string) error {
	var projectionTableExists bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass('doc_blob_ref_projections') IS NOT NULL`).Scan(&projectionTableExists); err != nil {
		return err
	}
	if projectionTableExists {
		if _, err := tx.Exec(ctx, `
			UPDATE doc_blob_ref_projections
			SET status='pending', indexed_at=NULL, error_code=NULL, error_summary=NULL, updated_at=clock_timestamp()
			WHERE workspace_id=$1 AND doc_id=$2`, workspaceID, docID); err != nil {
			return err
		}
	}
	var embeddingTableExists bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass('embedding_sources') IS NOT NULL`).Scan(&embeddingTableExists); err != nil {
		return err
	}
	if embeddingTableExists {
		if _, err := tx.Exec(ctx, `
			UPDATE embedding_sources SET deleted_at=clock_timestamp(), updated_at=clock_timestamp()
			WHERE workspace_id=$1 AND source_kind='document' AND source_key=$2`, workspaceID, docID); err != nil {
			return err
		}
	}
	if workspaceID == docID {
		var workspaceColumnExists bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM information_schema.columns
				WHERE table_schema=current_schema() AND table_name='workspaces' AND column_name='last_check_embeddings'
			)`).Scan(&workspaceColumnExists); err != nil {
			return err
		}
		if workspaceColumnExists {
			if _, err := tx.Exec(ctx, `UPDATE workspaces SET last_check_embeddings='1970-01-01T00:00:00Z' WHERE id=$1`, workspaceID); err != nil {
				return err
			}
		}
	}
	return nil
}

// LockWorkspaceDocumentProjection orders derived metadata writes with sync and
// document deletion and publication. Acquire it before locking a projection
// queue row; source triggers acquire that row while the writer holds these
// locks. The domain lock also orders page/dirty-statistics writes with ACL and
// publication transactions (which acquire it before touching those rows).
func LockWorkspaceDocumentProjection(ctx context.Context, tx pgx.Tx, workspaceID, docID string) error {
	if err := lockWorkspaceStorageShared(ctx, tx, workspaceID); err != nil {
		return err
	}
	if err := lockWorkspaceDocumentUpdate(ctx, tx, workspaceID, docID); err != nil {
		return err
	}
	return lockWorkspaceDomain(ctx, tx, workspaceID)
}

func lockWorkspaceStorageShared(ctx context.Context, tx pgx.Tx, workspaceID string) error {
	key := "storage-workspace:" + workspaceID
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		var locked bool
		if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock_shared(hashtextextended($1,0))`, key).Scan(&locked); err != nil {
			return err
		}
		if locked {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return ErrWorkspaceLockTimeout
		case <-ticker.C:
		}
	}
}

func lockWorkspaceDocumentUpdate(ctx context.Context, tx pgx.Tx, workspaceID, docID string) error {
	key := fmt.Sprintf("workspace-doc-update:%s/%s", workspaceID, docID)
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		var locked bool
		if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1,0))`, key).Scan(&locked); err != nil {
			return err
		}
		if locked {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func lockWorkspaceDomain(ctx context.Context, tx pgx.Tx, workspaceID string) error {
	key := "domain:workspace:" + workspaceID
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		var locked bool
		if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1,0))`, key).Scan(&locked); err != nil {
			return err
		}
		if locked {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func lockWorkspacePermissionGeneration(ctx context.Context, tx pgx.Tx, workspaceID string, expected int64) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO workspace_sync_permission_generations(workspace_id,generation)
		VALUES($1,0) ON CONFLICT (workspace_id) DO NOTHING`, workspaceID); err != nil {
		return err
	}
	var generation int64
	if err := tx.QueryRow(ctx, `SELECT generation FROM workspace_sync_permission_generations WHERE workspace_id=$1 FOR SHARE`, workspaceID).Scan(&generation); err != nil {
		return err
	}
	if generation != expected {
		return ErrWorkspacePermissionGenerationChanged
	}
	return nil
}
