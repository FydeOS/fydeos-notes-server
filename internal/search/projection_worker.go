// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/documents"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// DocumentDiffReader loads the merged state of a workspace document.
type DocumentDiffReader interface {
	DiffWorkspace(ctx context.Context, workspaceID, docID string, stateVector []byte) ([]byte, []byte, int64, error)
}

// ProjectionWorker turns queued document changes into search projection rows.
type ProjectionWorker struct {
	db       txDB
	reader   DocumentDiffReader
	notifier progressNotifier
}

func NewProjectionWorker(db txDB, reader DocumentDiffReader, notifier ...interface {
	PublishWorkspaceEmbeddingProgressChanged(string)
}) *ProjectionWorker {
	w := &ProjectionWorker{db: db, reader: reader}
	for _, n := range notifier {
		if n != nil {
			w.notifier = n
			break
		}
	}
	return w
}

func (w *ProjectionWorker) Run(ctx context.Context) error {
	if w == nil || w.db == nil || w.reader == nil {
		return errors.New("search projection worker is not configured")
	}
	return runLoop(ctx, 250*time.Millisecond, 250*time.Millisecond, 10*time.Second, func(ctx context.Context) (bool, error) {
		worked, err := w.RunOne(ctx)
		if err != nil && ctx.Err() == nil {
			log.Printf("search projection worker: %v", err)
		}
		return worked, err
	})
}

type projectionJob struct {
	workspaceID string
	docID       string
	token       string
	attempts    int
}

func (w *ProjectionWorker) RunOne(ctx context.Context) (bool, error) {
	if w == nil || w.db == nil || w.reader == nil {
		return false, errors.New("search projection worker is not configured")
	}
	job := projectionJob{token: uuid.NewString()}
	err := w.db.QueryRow(ctx, `
		UPDATE search_projection_jobs j
		SET claim_token=$1, claimed_until=now() + interval '30 seconds', updated_at=now()
		FROM (
			SELECT workspace_id, doc_id FROM search_projection_jobs
			WHERE available_at <= now() AND (claim_token IS NULL OR claimed_until < now())
			ORDER BY available_at, workspace_id, doc_id
			LIMIT 1 FOR UPDATE SKIP LOCKED
		) next
		WHERE j.workspace_id=next.workspace_id AND j.doc_id=next.doc_id
		RETURNING j.workspace_id, j.doc_id, j.attempts`, job.token).Scan(&job.workspaceID, &job.docID, &job.attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim search projection job: %w", err)
	}
	if err := w.process(ctx, job); err != nil {
		log.Printf("search projection %s/%s failed: %v", job.workspaceID, job.docID, err)
		if retryErr := w.retry(ctx, job); retryErr != nil {
			return true, errors.Join(err, retryErr)
		}
	}
	return true, nil
}

func (w *ProjectionWorker) retry(ctx context.Context, job projectionJob) error {
	delay := backoffSeconds(job.attempts, 9, 3600)
	_, err := w.db.Exec(ctx, `
		UPDATE search_projection_jobs
		SET attempts=attempts+1, claim_token=NULL, claimed_until=NULL, last_error='projection_failed',
			available_at=now() + $4 * interval '1 second', updated_at=now()
		WHERE workspace_id=$1 AND doc_id=$2 AND claim_token=$3`, job.workspaceID, job.docID, job.token, delay)
	return err
}

func (w *ProjectionWorker) process(ctx context.Context, job projectionJob) error {
	merged, _, updatedAtMilli, err := w.reader.DiffWorkspace(ctx, job.workspaceID, job.docID, nil)
	if errors.Is(err, documents.ErrWorkspaceDocNotFound) {
		return w.storeDeleted(ctx, job)
	}
	if err != nil {
		return err
	}
	projection, ok, err := documents.ProjectSearchContentInWorkspace([][]byte{merged}, job.workspaceID, job.docID)
	if err != nil {
		return err
	}
	if !ok {
		projection = documents.SearchProjection{}
	}
	return w.store(ctx, job, merged, projection, ok, time.UnixMilli(updatedAtMilli))
}

// beginJobTx opens the store transaction and confirms the claim is still held.
func (w *ProjectionWorker) beginJobTx(ctx context.Context, job projectionJob) (pgx.Tx, bool, error) {
	tx, err := w.db.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	if err := documents.LockWorkspaceDocumentProjection(ctx, tx, job.workspaceID, job.docID); err != nil {
		_ = tx.Rollback(ctx)
		return nil, false, err
	}
	var held bool
	err = tx.QueryRow(ctx, `SELECT true FROM search_projection_jobs
		WHERE workspace_id=$1 AND doc_id=$2 AND claim_token=$3 FOR UPDATE`, job.workspaceID, job.docID, job.token).Scan(&held)
	if errors.Is(err, pgx.ErrNoRows) {
		_ = tx.Rollback(ctx)
		return nil, false, nil
	}
	if err != nil {
		_ = tx.Rollback(ctx)
		return nil, false, err
	}
	return tx, true, nil
}

func (w *ProjectionWorker) finish(ctx context.Context, tx pgx.Tx, job projectionJob) error {
	if _, err := tx.Exec(ctx, `DELETE FROM search_projection_jobs WHERE workspace_id=$1 AND doc_id=$2 AND claim_token=$3`,
		job.workspaceID, job.docID, job.token); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if w.notifier != nil {
		w.notifier.PublishWorkspaceEmbeddingProgressChanged(job.workspaceID)
	}
	return nil
}

func nonNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func (w *ProjectionWorker) store(ctx context.Context, job projectionJob, merged []byte, projection documents.SearchProjection, isDoc bool, updatedAt time.Time) error {
	tx, held, err := w.beginJobTx(ctx, job)
	if err != nil || !held {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `DELETE FROM search_projection_units WHERE workspace_id=$1 AND doc_id=$2`, job.workspaceID, job.docID); err != nil {
		return err
	}
	if len(projection.Units) > 0 {
		batch := &pgx.Batch{}
		for _, u := range projection.Units {
			batch.Queue(`INSERT INTO search_projection_units(workspace_id, doc_id, block_id, unit_id, flavour, title, summary,
				content, blob, ref_doc_id, ref, parent_flavour, parent_block_id, additional, markdown_preview, updated_at)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
				ON CONFLICT (workspace_id, doc_id, unit_id) DO NOTHING`,
				job.workspaceID, job.docID, u.BlockID, u.UnitID, u.Flavour, projection.Title, projection.Summary,
				u.Text, nonNil(u.Blob), nonNil(u.RefDocIDs), nonNil(u.References), u.ParentFlavour, u.ParentBlockID,
				u.Additional, u.MarkdownPreview, updatedAt)
		}
		if err := tx.SendBatch(ctx, batch).Close(); err != nil {
			return fmt.Errorf("insert search units: %w", err)
		}
	}
	if isDoc {
		if _, err := tx.Exec(ctx, `
			INSERT INTO workspace_pages(workspace_id, page_id, title, summary)
			SELECT $1::text, $2::text, $3::text, $4::text WHERE EXISTS (SELECT 1 FROM workspaces WHERE id=$1::text)
			ON CONFLICT (workspace_id, page_id) DO UPDATE SET title=EXCLUDED.title, summary=EXCLUDED.summary`,
			job.workspaceID, job.docID, projection.Title, projection.Summary); err != nil {
			return fmt.Errorf("upsert workspace page: %w", err)
		}
	}
	if err := syncDocumentSource(ctx, tx, job.workspaceID, job.docID, merged, projection); err != nil {
		return fmt.Errorf("sync embedding source: %w", err)
	}
	return w.finish(ctx, tx, job)
}

func (w *ProjectionWorker) storeDeleted(ctx context.Context, job projectionJob) error {
	tx, held, err := w.beginJobTx(ctx, job)
	if err != nil || !held {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `DELETE FROM search_projection_units WHERE workspace_id=$1 AND doc_id=$2`, job.workspaceID, job.docID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM workspace_pages WHERE workspace_id=$1 AND page_id=$2`, job.workspaceID, job.docID); err != nil {
		return err
	}
	exists, err := embeddingTablesExist(ctx, tx)
	if err != nil {
		return err
	}
	if exists {
		if _, err := tx.Exec(ctx, `UPDATE embedding_sources SET deleted_at=now(), updated_at=now()
			WHERE workspace_id=$1 AND source_kind='document' AND source_key=$2 AND deleted_at IS NULL`, job.workspaceID, job.docID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM embedding_projections WHERE source_id IN (
			SELECT id FROM embedding_sources WHERE workspace_id=$1 AND source_kind='document' AND source_key=$2)`, job.workspaceID, job.docID); err != nil {
			return err
		}
	}
	return w.finish(ctx, tx, job)
}

type documentSourceUnit struct {
	UnitID     string  `json:"unitId"`
	Visibility string  `json:"visibility"`
	Text       string  `json:"text"`
	BlockID    string  `json:"blockId"`
	ElementID  *string `json:"elementId"`
	FrameID    *string `json:"frameId"`
}

type documentSource struct {
	DocID      string               `json:"docId"`
	Revision   string               `json:"revision"`
	SourceHash string               `json:"sourceHash"`
	Units      []documentSourceUnit `json:"units"`
}

func buildDocumentSource(docID string, merged []byte, projection documents.SearchProjection) (string, []byte) {
	hash := sha256Hex(merged)
	source := documentSource{DocID: docID, Revision: hash, SourceHash: hash, Units: make([]documentSourceUnit, 0, len(projection.Units))}
	for _, u := range projection.Units {
		visibility := "page"
		if u.Flavour == "affine:surface" {
			visibility = "edgeless"
		}
		source.Units = append(source.Units, documentSourceUnit{UnitID: u.UnitID, Visibility: visibility, Text: u.Text, BlockID: u.BlockID})
	}
	data, _ := json.Marshal(source)
	return hash, data
}

// syncDocumentSource keeps the document's embedding source current inside the
// projection transaction. Unchanged sources are left untouched.
func syncDocumentSource(ctx context.Context, tx pgx.Tx, workspaceID, docID string, merged []byte, projection documents.SearchProjection) error {
	exists, err := embeddingTablesExist(ctx, tx)
	if err != nil || !exists {
		return err
	}
	hash, payload := buildDocumentSource(docID, merged, projection)
	var (
		sourceID string
		same     bool
	)
	err = tx.QueryRow(ctx, `SELECT id::text,
			content_revision=$3 AND descriptor_revision=$3 AND recipe_revision=$4
			AND document_projection IS NOT DISTINCT FROM $5::jsonb AND deleted_at IS NULL
		FROM embedding_sources WHERE workspace_id=$1 AND source_kind='document' AND source_key=$2 FOR UPDATE`,
		workspaceID, docID, hash, documentRecipeRevision, string(payload)).Scan(&sourceID, &same)
	changed := !same
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		sourceID = uuid.NewString()
		changed = true
		if _, err := tx.Exec(ctx, `INSERT INTO embedding_sources(id, workspace_id, source_kind, source_key,
				content_revision, descriptor_revision, recipe_revision, document_projection)
			VALUES ($1,$2,'document',$3,$4,$4,$5,$6::jsonb)`,
			sourceID, workspaceID, docID, hash, documentRecipeRevision, string(payload)); err != nil {
			return err
		}
	case err != nil:
		return err
	case changed:
		if _, err := tx.Exec(ctx, `UPDATE embedding_sources SET content_revision=$2, descriptor_revision=$2,
				recipe_revision=$3, document_projection=$4::jsonb, deleted_at=NULL, updated_at=now()
			WHERE id=$1`, sourceID, hash, documentRecipeRevision, string(payload)); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO embedding_projections(source_id, index_id, status, priority)
		SELECT $1, i.id, 'pending', 100
		FROM embedding_workspace_states s JOIN embedding_indexes i ON i.id=s.active_index_id
		WHERE s.workspace_id=$2 AND s.runtime_state='active' AND i.inactive_at IS NULL
		ON CONFLICT (source_id, index_id) DO UPDATE SET
			status='pending', lease_owner=NULL, lease_until=NULL, lease_token=embedding_projections.lease_token+1,
			next_attempt_at=NULL, attempt_count=0, last_error_code=NULL, last_error_detail=NULL, updated_at=now()
		WHERE $3`, sourceID, workspaceID, changed)
	return err
}
