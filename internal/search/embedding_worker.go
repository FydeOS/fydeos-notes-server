// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package search

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/byok"
	"github.com/fydeos/fydeos-notes-server/internal/storage"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ObjectReader reads artifact bodies from object storage.
type ObjectReader interface {
	GetObject(ctx context.Context, scope, key string) (io.ReadCloser, storage.ObjectMetadata, error)
}

// EmbeddingWorker probes embedding routes and embeds pending sources.
type EmbeddingWorker struct {
	db       embeddingDB
	targets  TargetResolver
	notifier progressNotifier
	objects  ObjectReader
	embed    embedFunc
	owner    string
}

func NewEmbeddingWorker(db embeddingDB, targets TargetResolver, notifier interface {
	PublishWorkspaceEmbeddingProgressChanged(string)
}, objects ...ObjectReader) *EmbeddingWorker {
	w := &EmbeddingWorker{db: db, targets: targets, notifier: notifier, embed: byok.EmbedText, owner: "embedding-worker-" + uuid.NewString()}
	for _, o := range objects {
		if o != nil {
			w.objects = o
			break
		}
	}
	return w
}

func (w *EmbeddingWorker) Run(ctx context.Context) error {
	if w == nil || w.db == nil || w.targets == nil {
		return errors.New("embedding worker is not configured")
	}
	return runLoop(ctx, time.Second, time.Second, 30*time.Second, func(ctx context.Context) (bool, error) {
		worked, err := w.RunOne(ctx)
		if err != nil && ctx.Err() == nil {
			log.Printf("embedding worker: %v", err)
		}
		return worked, err
	})
}

func (w *EmbeddingWorker) RunOne(ctx context.Context) (bool, error) {
	if w == nil || w.db == nil || w.targets == nil {
		return false, errors.New("embedding worker is not configured")
	}
	exists, err := embeddingTablesExist(ctx, w.db)
	if err != nil || !exists {
		return false, err
	}
	if worked, err := w.probeOne(ctx); worked || err != nil {
		return worked, err
	}
	return w.projectOne(ctx)
}

func (w *EmbeddingWorker) resolve(ctx context.Context, workspaceID, fingerprint string) (byok.ChatTarget, string, error) {
	owner, err := workspaceOwner(ctx, w.db, workspaceID)
	if err != nil {
		return byok.ChatTarget{}, codeProviderUnavailable, err
	}
	if owner == "" {
		return byok.ChatTarget{}, codeProviderUnavailable, errors.New("workspace has no active owner")
	}
	target, err := w.targets.ResolveServerTarget(ctx, workspaceID, owner, embeddingFeature)
	if err != nil {
		return byok.ChatTarget{}, codeProviderUnavailable, err
	}
	if routeFingerprint(target) != fingerprint {
		return byok.ChatTarget{}, codeRouteChanged, errors.New("embedding route changed")
	}
	return target, "", nil
}

func (w *EmbeddingWorker) probeOne(ctx context.Context) (bool, error) {
	var indexID, workspaceID, fingerprint string
	err := w.db.QueryRow(ctx, `
		UPDATE embedding_indexes i SET probe_lease_owner=$1, probe_lease_until=now() + interval '2 minutes', updated_at=now()
		FROM (
			SELECT i2.id FROM embedding_indexes i2
			JOIN embedding_workspace_states s ON s.active_index_id=i2.id AND s.runtime_state='active'
			WHERE i2.inactive_at IS NULL
				AND (i2.health_status='pending' OR (i2.health_status='retry_wait' AND (i2.next_probe_at IS NULL OR i2.next_probe_at <= now())))
				AND (i2.probe_lease_until IS NULL OR i2.probe_lease_until < now())
			ORDER BY i2.updated_at, i2.id
			LIMIT 1 FOR UPDATE OF i2 SKIP LOCKED
		) next
		WHERE i.id=next.id
		RETURNING i.id::text, i.workspace_id, i.fingerprint`, w.owner).Scan(&indexID, &workspaceID, &fingerprint)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim embedding probe: %w", err)
	}
	healthy := false
	if target, _, err := w.resolve(ctx, workspaceID, fingerprint); err == nil {
		vector, err := w.embed(ctx, target, "health")
		healthy = err == nil && len(vector) == byok.EmbeddingDimensions
	}
	if healthy {
		_, err = w.db.Exec(ctx, `UPDATE embedding_indexes SET health_status='ready', failure_count=0, next_probe_at=NULL,
			probe_lease_owner=NULL, probe_lease_until=NULL, last_error_code=NULL, updated_at=now()
			WHERE id=$1 AND probe_lease_owner=$2 AND inactive_at IS NULL`, indexID, w.owner)
		if err == nil && w.notifier != nil {
			w.notifier.PublishWorkspaceEmbeddingProgressChanged(workspaceID)
		}
	} else {
		_, err = w.db.Exec(ctx, `UPDATE embedding_indexes SET health_status='retry_wait', failure_count=failure_count+1,
			next_probe_at=now() + LEAST(21600, 5 * power(2, LEAST(failure_count, 12))) * interval '1 second',
			probe_lease_owner=NULL, probe_lease_until=NULL, last_error_code=$3, updated_at=now()
			WHERE id=$1 AND probe_lease_owner=$2`, indexID, w.owner, codeProviderUnavailable)
	}
	return true, err
}

type claimedProjection struct {
	sourceID    string
	indexID     string
	leaseToken  int64
	workspaceID string
	kind        string
	key         string
	content     string
	descriptor  string
	recipe      string
	scope       *string
	storageKey  *string
	fileName    *string
	mimeType    *string
	sizeBytes   *int64
	projection  *string
	fingerprint string
	epoch       int64
}

type embeddingChunk struct {
	text       string
	unitID     *string
	visibility *string
	blockID    *string
	elementID  *string
	frameID    *string
}

type processError struct {
	code      string
	retryable bool
	err       error
}

func (e *processError) Error() string { return e.code + ": " + e.err.Error() }

func (w *EmbeddingWorker) projectOne(ctx context.Context) (bool, error) {
	var p claimedProjection
	err := w.db.QueryRow(ctx, `
		UPDATE embedding_projections p
		SET status='running', lease_owner=$1, lease_token=p.lease_token+1, lease_until=now() + interval '5 minutes', updated_at=now()
		FROM (
			SELECT p2.source_id, p2.index_id FROM embedding_projections p2
			JOIN embedding_sources src ON src.id=p2.source_id AND src.deleted_at IS NULL
			JOIN embedding_indexes i ON i.id=p2.index_id AND i.health_status='ready' AND i.inactive_at IS NULL
			JOIN embedding_workspace_states s ON s.active_index_id=i.id AND s.runtime_state='active'
			WHERE (p2.status='pending'
				OR (p2.status='retry_wait' AND (p2.next_attempt_at IS NULL OR p2.next_attempt_at <= now()))
				OR (p2.status='running' AND (p2.lease_until IS NULL OR p2.lease_until < now())))
				AND NOT EXISTS (
					SELECT 1 FROM embedding_projections r JOIN embedding_sources rs ON rs.id=r.source_id
					WHERE rs.workspace_id=src.workspace_id AND r.status='running' AND r.lease_until >= now())
			ORDER BY p2.priority DESC, p2.next_attempt_at NULLS FIRST, p2.updated_at
			LIMIT 1 FOR UPDATE OF p2 SKIP LOCKED
		) next
		WHERE p.source_id=next.source_id AND p.index_id=next.index_id
		RETURNING p.source_id::text, p.index_id::text, p.lease_token`, w.owner).Scan(&p.sourceID, &p.indexID, &p.leaseToken)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim embedding projection: %w", err)
	}
	if err := w.db.QueryRow(ctx, `
		SELECT src.workspace_id, src.source_kind, src.source_key, src.content_revision, src.descriptor_revision,
			src.recipe_revision, src.storage_scope, src.storage_key, src.file_name, src.mime_type, src.size_bytes,
			src.document_projection::text, i.fingerprint, COALESCE(s.index_epoch, 0)
		FROM embedding_sources src
		JOIN embedding_indexes i ON i.id=$2
		LEFT JOIN embedding_workspace_states s ON s.workspace_id=src.workspace_id
		WHERE src.id=$1`, p.sourceID, p.indexID).Scan(&p.workspaceID, &p.kind, &p.key, &p.content, &p.descriptor,
		&p.recipe, &p.scope, &p.storageKey, &p.fileName, &p.mimeType, &p.sizeBytes, &p.projection, &p.fingerprint, &p.epoch); err != nil {
		return true, fmt.Errorf("load embedding source: %w", err)
	}
	if perr := w.process(ctx, p); perr != nil {
		if err := w.recordFailure(ctx, p, perr); err != nil {
			return true, err
		}
	}
	if w.notifier != nil {
		w.notifier.PublishWorkspaceEmbeddingProgressChanged(p.workspaceID)
	}
	return true, nil
}

func (w *EmbeddingWorker) process(ctx context.Context, p claimedProjection) *processError {
	if p.kind != "document" && p.kind != "artifact" {
		return &processError{codeUnsupportedSource, false, fmt.Errorf("unsupported source kind %q", p.kind)}
	}
	target, code, err := w.resolve(ctx, p.workspaceID, p.fingerprint)
	if err != nil {
		return &processError{code, true, err}
	}
	var chunks []embeddingChunk
	if p.kind == "document" {
		if p.projection == nil {
			return &processError{codeDocumentInvalid, false, errors.New("missing document projection")}
		}
		chunks, err = documentChunks([]byte(*p.projection))
		if err != nil {
			return &processError{codeDocumentInvalid, false, err}
		}
	} else {
		chunks, err = w.artifactChunks(ctx, p)
		if err != nil {
			code := codeArtifactExtraction
			if errors.Is(err, errUnsupportedFormat) {
				code = codeArtifactFormat
			}
			return &processError{code, false, err}
		}
	}
	vectors := make([]string, len(chunks))
	for i, chunk := range chunks {
		values, err := w.embed(ctx, target, chunk.text)
		if err != nil {
			return &processError{codeProviderUnavailable, true, err}
		}
		literal, err := vectorLiteral(values)
		if err != nil {
			return &processError{codeInvalidEmbedCount, true, err}
		}
		vectors[i] = literal
	}
	if err := w.commit(ctx, p, chunks, vectors); err != nil {
		return &processError{codeCommitFailed, true, err}
	}
	return nil
}

var errStaleProjection = errors.New("embedding projection is stale")

func (w *EmbeddingWorker) commit(ctx context.Context, p claimedProjection, chunks []embeddingChunk, vectors []string) error {
	tx, err := w.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	generation := uuid.NewString()
	batch := &pgx.Batch{}
	for i, chunk := range chunks {
		var docID, artifactID *string
		if p.kind == "document" {
			docID = &p.key
		} else {
			artifactID = &p.key
		}
		batch.Queue(`INSERT INTO embedding_chunks(generation_token, workspace_id, index_id, source_id, chunk_index, content,
				embedding, source_kind, doc_id, artifact_id, unit_id, visibility, block_id, element_id, frame_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7::text::public.vector,$8,$9,$10::uuid,$11,$12,$13,$14,$15)`,
			generation, p.workspaceID, p.indexID, p.sourceID, i, chunk.text, vectors[i], p.kind, docID, artifactID,
			chunk.unitID, chunk.visibility, chunk.blockID, chunk.elementID, chunk.frameID)
	}
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("insert embedding chunks: %w", err)
	}
	var (
		activeIndex *string
		epoch       int64
	)
	if err := tx.QueryRow(ctx, `SELECT active_index_id::text, index_epoch FROM embedding_workspace_states
		WHERE workspace_id=$1 FOR UPDATE`, p.workspaceID).Scan(&activeIndex, &epoch); err != nil {
		return err
	}
	var current bool
	if err := tx.QueryRow(ctx, `SELECT content_revision=$2 AND descriptor_revision=$3 AND recipe_revision=$4 AND deleted_at IS NULL
		FROM embedding_sources WHERE id=$1 FOR UPDATE`, p.sourceID, p.content, p.descriptor, p.recipe).Scan(&current); err != nil {
		return err
	}
	var leaseHeld bool
	if err := tx.QueryRow(ctx, `SELECT status='running' AND lease_token=$3 AND lease_until > now()
		FROM embedding_projections WHERE source_id=$1 AND index_id=$2 FOR UPDATE`, p.sourceID, p.indexID, p.leaseToken).Scan(&leaseHeld); err != nil {
		return err
	}
	if activeIndex == nil || *activeIndex != p.indexID || epoch != p.epoch || !current || !leaseHeld {
		return errStaleProjection
	}
	if _, err := tx.Exec(ctx, `UPDATE embedding_projections SET status='ready', applied_content_revision=$3,
			applied_descriptor_revision=$4, applied_recipe_revision=$5, active_generation_token=$6, attempt_count=0,
			lease_owner=NULL, lease_until=NULL, next_attempt_at=NULL, last_error_code=NULL, last_error_detail=NULL, updated_at=now()
		WHERE source_id=$1 AND index_id=$2`, p.sourceID, p.indexID, p.content, p.descriptor, p.recipe, generation); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (w *EmbeddingWorker) recordFailure(ctx context.Context, p claimedProjection, perr *processError) error {
	detail := truncateRunes(perr.err.Error(), 500)
	_, err := w.db.Exec(ctx, `UPDATE embedding_projections SET
			attempt_count=attempt_count+1,
			status=CASE WHEN $4 AND attempt_count+1 < 10 THEN 'retry_wait' ELSE 'failed' END,
			next_attempt_at=CASE WHEN $4 AND attempt_count+1 < 10
				THEN now() + LEAST(21600, 5 * power(2, LEAST(attempt_count, 12))) * interval '1 second' ELSE NULL END,
			lease_owner=NULL, lease_until=NULL, last_error_code=$5, last_error_detail=$6, updated_at=now()
		WHERE source_id=$1 AND index_id=$2 AND status='running' AND lease_token=$3`,
		p.sourceID, p.indexID, p.leaseToken, perr.retryable, perr.code, detail)
	if err != nil {
		return fmt.Errorf("record embedding failure: %w", err)
	}
	if perr.code == codeProviderUnavailable {
		_, err = w.db.Exec(ctx, `UPDATE embedding_indexes SET health_status='retry_wait', failure_count=failure_count+1,
			next_probe_at=now() + LEAST(21600, 5 * power(2, LEAST(failure_count, 12))) * interval '1 second',
			last_error_code=$2, updated_at=now()
			WHERE id=$1 AND health_status='ready'`, p.indexID, codeProviderUnavailable)
	}
	return err
}

// documentChunks splits a persisted document projection into embeddable chunks.
func documentChunks(raw []byte) ([]embeddingChunk, error) {
	if len(raw) > maxSourceTextBytes {
		return nil, errors.New("document projection is too large")
	}
	var source documentSource
	if err := json.Unmarshal(raw, &source); err != nil {
		return nil, fmt.Errorf("decode document projection: %w", err)
	}
	if source.DocID == "" {
		return nil, errors.New("document projection has no docId")
	}
	var chunks []embeddingChunk
	total := 0
	for _, unit := range source.Units {
		if strings.TrimSpace(unit.Text) == "" {
			continue
		}
		if unit.UnitID == "" || (unit.Visibility != "page" && unit.Visibility != "edgeless") {
			return nil, errors.New("document projection unit is invalid")
		}
		total += len(unit.Text)
		if total > maxSourceTextBytes {
			return nil, errors.New("document projection text is too large")
		}
		unitID, visibility := unit.UnitID, unit.Visibility
		var blockID *string
		if unit.BlockID != "" {
			b := unit.BlockID
			blockID = &b
		}
		for _, piece := range splitText(unit.Text) {
			chunks = append(chunks, embeddingChunk{text: piece, unitID: &unitID, visibility: &visibility,
				blockID: blockID, elementID: unit.ElementID, frameID: unit.FrameID})
			if len(chunks) > maxChunksPerSource {
				return nil, errors.New("document projection has too many chunks")
			}
		}
	}
	if len(chunks) == 0 {
		return nil, errors.New("document projection has no text")
	}
	return chunks, nil
}

func textChunks(parts []string) ([]embeddingChunk, error) {
	var chunks []embeddingChunk
	total := 0
	for _, part := range parts {
		total += len(part)
		if total > maxSourceTextBytes {
			return nil, errors.New("artifact text is too large")
		}
		for _, piece := range splitText(part) {
			chunks = append(chunks, embeddingChunk{text: piece})
			if len(chunks) > maxChunksPerSource {
				return nil, errors.New("artifact has too many chunks")
			}
		}
	}
	if len(chunks) == 0 {
		return nil, errors.New("artifact has no text")
	}
	return chunks, nil
}

func (w *EmbeddingWorker) artifactChunks(ctx context.Context, p claimedProjection) ([]embeddingChunk, error) {
	if w.objects == nil {
		return nil, errors.New("artifact storage is not configured")
	}
	if p.scope == nil || p.storageKey == nil || *p.scope == "" || *p.storageKey == "" {
		return nil, errors.New("artifact locator is incomplete")
	}
	reader, metadata, err := w.objects.GetObject(ctx, *p.scope, *p.storageKey)
	if err != nil {
		return nil, fmt.Errorf("read artifact: %w", err)
	}
	defer reader.Close()
	if metadata.ContentLength > maxArtifactBytes {
		return nil, errors.New("artifact exceeds size limit")
	}
	var body bytes.Buffer
	if _, err := io.Copy(&body, io.LimitReader(reader, maxArtifactBytes+1)); err != nil {
		return nil, fmt.Errorf("read artifact: %w", err)
	}
	if body.Len() > maxArtifactBytes {
		return nil, errors.New("artifact exceeds size limit")
	}
	if p.sizeBytes != nil && int64(body.Len()) != *p.sizeBytes {
		return nil, errors.New("artifact size does not match")
	}
	fileName, mimeType := "", ""
	if p.fileName != nil {
		fileName = *p.fileName
	}
	if p.mimeType != nil {
		mimeType = *p.mimeType
	}
	parts, err := ExtractArtifactText(body.Bytes(), fileName, mimeType)
	if err != nil {
		return nil, err
	}
	return textChunks(parts)
}
