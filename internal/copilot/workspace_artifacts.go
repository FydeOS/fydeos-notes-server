// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package copilot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/search"
	"github.com/fydeos/fydeos-notes-server/internal/storage"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const maxWorkspaceArtifactBytes int64 = 50 << 20

type workspaceArtifactDB interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type workspaceArtifactObjects interface {
	PutObject(context.Context, string, string, io.Reader, storage.PutOptions) (storage.ObjectMetadata, error)
	GetObject(context.Context, string, string) (io.ReadCloser, storage.ObjectMetadata, error)
}

type WorkspaceArtifact struct {
	WorkspaceID     string
	ArtifactID      string
	ContentHash     string
	FileName        string
	EmbeddingStatus string
	MediaType       string
	Size            int64
	CreatedAt       time.Time
}

type WorkspaceArtifacts struct {
	db      workspaceArtifactDB
	objects workspaceArtifactObjects
}

func NewWorkspaceArtifacts(db workspaceArtifactDB, objects workspaceArtifactObjects) *WorkspaceArtifacts {
	return &WorkspaceArtifacts{db: db, objects: objects}
}

func (s *WorkspaceArtifacts) EmbeddingAvailable(parent context.Context) bool {
	if s == nil || s.db == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(parent, time.Second)
	defer cancel()
	var available bool
	if err := s.db.QueryRow(ctx, `SELECT to_regclass('embedding_sources') IS NOT NULL AND to_regclass('embedding_indexes') IS NOT NULL`).Scan(&available); err != nil {
		return false
	}
	return available
}

func (s *WorkspaceArtifacts) Add(ctx context.Context, workspaceID, fileName, declaredMIME string, source io.Reader) (WorkspaceArtifact, error) {
	if s == nil || s.db == nil || s.objects == nil {
		return WorkspaceArtifact{}, errors.New("workspace artifacts are unavailable")
	}
	name := strings.TrimSpace(fileName)
	if name == "" || len(name) > 255 || strings.ContainsAny(name, "\\/\x00") {
		return WorkspaceArtifact{}, errors.New("workspace artifact filename is invalid")
	}
	if source == nil {
		return WorkspaceArtifact{}, errors.New("workspace artifact body is required")
	}
	body, err := io.ReadAll(io.LimitReader(source, maxWorkspaceArtifactBytes+1))
	if err != nil {
		return WorkspaceArtifact{}, err
	}
	if len(body) == 0 || int64(len(body)) > maxWorkspaceArtifactBytes {
		return WorkspaceArtifact{}, errors.New("workspace artifact size is invalid")
	}
	contentHashBytes := sha256.Sum256(body)
	contentHash := base64.RawURLEncoding.EncodeToString(contentHashBytes[:])
	mediaType := canonicalArtifactMediaType(body, declaredMIME)
	extractedRunes := extractedArtifactTextRunes(body, name, mediaType)
	key := "artifacts/" + workspaceID + "/" + contentHash
	var artifactID string
	var status string
	err = s.db.QueryRow(ctx, `INSERT INTO workspace_artifacts(
		id,workspace_id,content_hash,display_name,file_name,canonical_media_type,size_bytes,storage_scope,storage_key,status,extracted_text_runes,
		library_owned,reservation_expires_at,created_at,updated_at)
		VALUES(gen_random_uuid(),$1,$2,$3,$3,$4,$5,'copilot',$6,'reserving',$7,true,clock_timestamp()+interval '24 hours',clock_timestamp(),clock_timestamp())
		ON CONFLICT(workspace_id,content_hash) DO UPDATE SET
			library_owned=true,display_name=EXCLUDED.display_name,file_name=coalesce(workspace_artifacts.file_name,EXCLUDED.file_name),
			extracted_text_runes=coalesce(EXCLUDED.extracted_text_runes,workspace_artifacts.extracted_text_runes),updated_at=clock_timestamp()
		WHERE workspace_artifacts.status<>'deleting'
		RETURNING id::text,status`, workspaceID, contentHash, name, mediaType, len(body), key, extractedRunes).Scan(&artifactID, &status)
	if err != nil {
		return WorkspaceArtifact{}, fmt.Errorf("reserve workspace artifact: %w", err)
	}
	if status != "ready" {
		length := int64(len(body))
		if _, err := s.objects.PutObject(ctx, storage.CopilotScope, key, bytes.NewReader(body), storage.PutOptions{ContentType: mediaType, ContentLength: &length}); err != nil {
			return WorkspaceArtifact{}, err
		}
		stored, _, err := s.objects.GetObject(ctx, storage.CopilotScope, key)
		if err != nil {
			return WorkspaceArtifact{}, err
		}
		verified, readErr := io.ReadAll(io.LimitReader(stored, maxWorkspaceArtifactBytes+1))
		closeErr := stored.Close()
		if readErr != nil || closeErr != nil {
			return WorkspaceArtifact{}, errors.Join(readErr, closeErr)
		}
		verifiedHash := sha256.Sum256(verified)
		if !bytes.Equal(verifiedHash[:], contentHashBytes[:]) {
			return WorkspaceArtifact{}, errors.New("workspace artifact object checksum mismatch")
		}
		if _, err := s.db.Exec(ctx, `UPDATE workspace_artifacts SET status='ready',ready_at=coalesce(ready_at,clock_timestamp()),reservation_expires_at=NULL,updated_at=clock_timestamp() WHERE id=$1 AND status='reserving'`, artifactID); err != nil {
			return WorkspaceArtifact{}, fmt.Errorf("complete workspace artifact: %w", err)
		}
	}
	if err := s.registerEmbeddingSource(ctx, workspaceID, artifactID, contentHash, name, mediaType, int64(len(body)), storage.CopilotScope, key); err != nil {
		return WorkspaceArtifact{}, fmt.Errorf("register workspace artifact embedding source: %w", err)
	}
	return s.Get(ctx, workspaceID, artifactID)
}

func (s *WorkspaceArtifacts) EnsureBlobAlias(ctx context.Context, workspaceID, blobID, declaredMIME string) error {
	if s == nil || s.db == nil || s.objects == nil {
		return errors.New("workspace artifacts are unavailable")
	}
	if workspaceID == "" || blobID == "" || strings.ContainsAny(workspaceID+blobID, "\x00\\") {
		return errors.New("workspace blob reference is invalid")
	}
	objectKey := workspaceID + "/" + blobID
	body, _, err := s.objects.GetObject(ctx, storage.BlobScope, objectKey)
	if err != nil {
		return fmt.Errorf("read legacy context blob: %w", err)
	}
	content, readErr := io.ReadAll(io.LimitReader(body, maxWorkspaceArtifactBytes+1))
	closeErr := body.Close()
	if readErr != nil || closeErr != nil {
		return errors.Join(readErr, closeErr)
	}
	if err := validateArtifactBody(content); err != nil {
		return err
	}
	contentHashBytes := sha256.Sum256(content)
	contentHash := base64.RawURLEncoding.EncodeToString(contentHashBytes[:])
	mediaType := canonicalDeclaredArtifactMediaType(declaredMIME)
	extractedRunes := extractedArtifactTextRunes(content, "", mediaType)
	var artifactID, status, storageScope, storageKey string
	err = s.db.QueryRow(ctx, `INSERT INTO workspace_artifacts(
		id,workspace_id,content_hash,canonical_media_type,size_bytes,storage_scope,storage_key,status,extracted_text_runes,
		library_owned,reservation_expires_at,created_at,updated_at)
		VALUES(gen_random_uuid(),$1,$2,$3,$4,'blob',$5,'reserving',$6,false,clock_timestamp()+interval '24 hours',clock_timestamp(),clock_timestamp())
		ON CONFLICT(workspace_id,content_hash) DO UPDATE SET
			library_owned=workspace_artifacts.library_owned OR EXCLUDED.library_owned,
			display_name=coalesce(workspace_artifacts.display_name,EXCLUDED.display_name),
			file_name=coalesce(workspace_artifacts.file_name,EXCLUDED.file_name),
			reservation_expires_at=CASE WHEN workspace_artifacts.status='ready' THEN NULL ELSE EXCLUDED.reservation_expires_at END,
			extracted_text_runes=coalesce(EXCLUDED.extracted_text_runes,workspace_artifacts.extracted_text_runes),
			updated_at=clock_timestamp()
		WHERE workspace_artifacts.status<>'deleting'
		RETURNING id::text,status,storage_scope,storage_key`, workspaceID, contentHash, mediaType, len(content), objectKey, extractedRunes).
		Scan(&artifactID, &status, &storageScope, &storageKey)
	if err != nil {
		return fmt.Errorf("register legacy context blob artifact: %w", err)
	}
	if status == "ready" {
		if err := s.registerEmbeddingSource(ctx, workspaceID, artifactID, contentHash, "", mediaType, int64(len(content)), storageScope, storageKey); err != nil {
			return fmt.Errorf("register legacy artifact embedding source: %w", err)
		}
		return nil
	}
	if storageScope == storage.CopilotScope {
		length := int64(len(content))
		if _, err := s.objects.PutObject(ctx, storageScope, storageKey, bytes.NewReader(content), storage.PutOptions{ContentType: mediaType, ContentLength: &length}); err != nil {
			return fmt.Errorf("write legacy context artifact object: %w", err)
		}
	}
	stored, _, err := s.objects.GetObject(ctx, storageScope, storageKey)
	if err != nil {
		return fmt.Errorf("verify legacy context artifact object: %w", err)
	}
	verified, readErr := io.ReadAll(io.LimitReader(stored, maxWorkspaceArtifactBytes+1))
	closeErr = stored.Close()
	if readErr != nil || closeErr != nil {
		return errors.Join(readErr, closeErr)
	}
	if err := validateArtifactBody(verified); err != nil {
		return err
	}
	verifiedHash := sha256.Sum256(verified)
	if !bytes.Equal(verifiedHash[:], contentHashBytes[:]) {
		return errors.New("legacy context artifact object checksum mismatch")
	}
	if _, err := s.db.Exec(ctx, `UPDATE workspace_artifacts SET status='ready',ready_at=coalesce(ready_at,clock_timestamp()),reservation_expires_at=NULL,updated_at=clock_timestamp() WHERE id=$1 AND status='reserving'`, artifactID); err != nil {
		return fmt.Errorf("complete legacy context artifact: %w", err)
	}
	if err := s.registerEmbeddingSource(ctx, workspaceID, artifactID, contentHash, "", mediaType, int64(len(content)), storageScope, storageKey); err != nil {
		return fmt.Errorf("register legacy artifact embedding source: %w", err)
	}
	return nil
}

func (s *WorkspaceArtifacts) registerEmbeddingSource(ctx context.Context, workspaceID, artifactID, contentHash, fileName, mediaType string, size int64, storageScope, storageKey string) error {
	if !s.EmbeddingAvailable(ctx) {
		return nil
	}
	_, err := s.db.Exec(ctx, `
		WITH source AS (
			INSERT INTO embedding_sources(
				id,workspace_id,source_kind,source_key,content_revision,descriptor_revision,
				recipe_revision,storage_scope,storage_key,file_name,mime_type,size_bytes,deleted_at
			) VALUES(gen_random_uuid(),$1,'artifact',$2,$3,$4,'artifact-extraction-v1',$5,$6,NULLIF($7,''),$8,$9,NULL)
			ON CONFLICT(workspace_id,source_kind,source_key) DO UPDATE SET
				content_revision=EXCLUDED.content_revision,descriptor_revision=EXCLUDED.descriptor_revision,
				recipe_revision=EXCLUDED.recipe_revision,storage_scope=EXCLUDED.storage_scope,
				storage_key=EXCLUDED.storage_key,file_name=coalesce(EXCLUDED.file_name,embedding_sources.file_name),
				mime_type=EXCLUDED.mime_type,size_bytes=EXCLUDED.size_bytes,deleted_at=NULL,updated_at=clock_timestamp()
			RETURNING id,workspace_id
		)
		INSERT INTO embedding_projections(source_id,index_id,status,priority)
		SELECT source.id,state.active_index_id,'pending',200
		FROM source JOIN embedding_workspace_states state ON state.workspace_id=source.workspace_id
		WHERE state.active_index_id IS NOT NULL
		ON CONFLICT(source_id,index_id) DO UPDATE SET
			status=CASE WHEN embedding_projections.status='running' THEN 'running' ELSE 'pending' END,
			next_attempt_at=NULL,updated_at=clock_timestamp()
		WHERE embedding_projections.status<>'ready'`,
		workspaceID, artifactID, contentHash,
		fmt.Sprintf("%s:%d:%s", mediaType, size, fileName), storageScope, storageKey, fileName, mediaType, size)
	return err
}

func (s *WorkspaceArtifacts) Get(ctx context.Context, workspaceID, artifactID string) (WorkspaceArtifact, error) {
	var item WorkspaceArtifact
	item.EmbeddingStatus = "processing"
	err := s.db.QueryRow(ctx, `SELECT workspace_id,id::text,content_hash,display_name,canonical_media_type,size_bytes,created_at
		FROM workspace_artifacts WHERE workspace_id=$1 AND id::text=$2 AND library_owned AND status='ready'`, workspaceID, artifactID).
		Scan(&item.WorkspaceID, &item.ArtifactID, &item.ContentHash, &item.FileName, &item.MediaType, &item.Size, &item.CreatedAt)
	if err != nil {
		return WorkspaceArtifact{}, err
	}
	statuses, err := s.embeddingStatuses(ctx, workspaceID, []string{item.ArtifactID})
	if err != nil {
		return WorkspaceArtifact{}, err
	}
	if status, ok := statuses[item.ArtifactID]; ok {
		item.EmbeddingStatus = status
	}
	return item, nil
}

type WorkspaceArtifactContent struct {
	Content    string
	Revision   string
	Name       string
	MediaType  string
	Truncated  bool
	NextCursor string
}

func (s *WorkspaceArtifacts) ReadExtracted(ctx context.Context, workspaceID, artifactID string, maxChars int, cursor string) (WorkspaceArtifactContent, error) {
	return s.ReadExtractedScoped(ctx, workspaceID, "", artifactID, maxChars, cursor, ArtifactRetrievalScope{Mode: "workspace"})
}

func (s *WorkspaceArtifacts) ReadExtractedScoped(ctx context.Context, workspaceID, messageID, artifactID string, maxChars int, cursor string, retrieval ArtifactRetrievalScope) (WorkspaceArtifactContent, error) {
	if s == nil || s.db == nil || workspaceID == "" || artifactID == "" || maxChars < 1 || maxChars > 100_000 || len(cursor) > 128 {
		return WorkspaceArtifactContent{}, errors.New("workspace artifact read input is invalid")
	}
	if retrieval.Mode != "required" {
		retrieval = ArtifactRetrievalScope{Mode: "workspace"}
	}
	if retrieval.Mode == "required" && (messageID == "" || !containsArtifactID(retrieval.RequiredArtifactIDs, artifactID)) {
		return WorkspaceArtifactContent{}, errors.New("artifact is outside the required retrieval scope")
	}
	startOffset, err := parseArtifactReadCursor(cursor)
	if err != nil {
		return WorkspaceArtifactContent{}, err
	}
	var result WorkspaceArtifactContent
	var storageScope, storageKey, fileName string
	var size int64
	var extractedTextRunes *int64
	if err := s.db.QueryRow(ctx, `
		SELECT artifact.content_hash,coalesce(artifact.display_name,artifact.file_name,''),artifact.canonical_media_type,
		       artifact.storage_scope,artifact.storage_key,artifact.file_name,artifact.size_bytes,artifact.extracted_text_runes
		FROM workspace_artifacts artifact
		WHERE artifact.workspace_id=$1 AND artifact.id::text=$2 AND artifact.status='ready'
		  AND (($4='workspace' AND artifact.library_owned) OR ($4='required' AND EXISTS(
		      SELECT 1 FROM ai_message_artifacts link WHERE link.workspace_id=artifact.workspace_id
		       AND link.artifact_id=artifact.id AND link.message_id=$3 AND link.role='attachment')))`, workspaceID, artifactID, messageID, retrieval.Mode).
		Scan(&result.Revision, &result.Name, &result.MediaType, &storageScope, &storageKey, &fileName, &size, &extractedTextRunes); err != nil {
		return WorkspaceArtifactContent{}, err
	}
	if extractedTextRunes != nil && int64(startOffset) > *extractedTextRunes || extractedTextRunes == nil && int64(startOffset) > size {
		return WorkspaceArtifactContent{}, errors.New("workspace artifact cursor is out of range")
	}
	if extractedTextRunes == nil && cursor != "" {
		return WorkspaceArtifactContent{}, errors.New("workspace artifact cursor is unavailable")
	}
	if s.objects == nil || size < 1 || size > maxWorkspaceArtifactBytes {
		return WorkspaceArtifactContent{}, errors.New("workspace artifact object is unavailable")
	}
	reader, metadata, err := s.objects.GetObject(ctx, storageScope, storageKey)
	if err != nil {
		return WorkspaceArtifactContent{}, err
	}
	defer reader.Close()
	if metadata.ContentLength > maxWorkspaceArtifactBytes {
		return WorkspaceArtifactContent{}, errors.New("workspace artifact object exceeds size limit")
	}
	body, err := io.ReadAll(io.LimitReader(reader, maxWorkspaceArtifactBytes+1))
	if err != nil || int64(len(body)) != size || int64(len(body)) > maxWorkspaceArtifactBytes {
		return WorkspaceArtifactContent{}, errors.New("workspace artifact object is invalid")
	}
	parts, err := search.ExtractArtifactText(body, fileName, result.MediaType)
	if err != nil {
		return WorkspaceArtifactContent{}, fmt.Errorf("extract workspace artifact: %w", err)
	}
	canonical := strings.Join(parts, "\n")
	runes := []rune(canonical)
	if extractedTextRunes != nil && int64(len(runes)) != *extractedTextRunes {
		return WorkspaceArtifactContent{}, errors.New("workspace artifact extracted content changed")
	}
	if extractedTextRunes == nil {
		_, _ = s.db.Exec(ctx, `UPDATE workspace_artifacts SET extracted_text_runes=$4 WHERE workspace_id=$1 AND id::text=$2 AND content_hash=$3 AND extracted_text_runes IS NULL`, workspaceID, artifactID, result.Revision, len(runes))
	}
	if startOffset > len(runes) {
		return WorkspaceArtifactContent{}, errors.New("workspace artifact cursor is out of range")
	}
	end := min(startOffset+maxChars, len(runes))
	result.Content = string(runes[startOffset:end])
	result.Truncated = end < len(runes)
	if result.Truncated {
		result.NextCursor = strconv.Itoa(end)
	}
	return result, nil
}

func extractedArtifactTextRunes(body []byte, fileName, mediaType string) *int64 {
	parts, err := search.ExtractArtifactText(body, fileName, mediaType)
	if err != nil {
		return nil
	}
	length := int64(len([]rune(strings.Join(parts, "\n"))))
	return &length
}

func parseArtifactReadCursor(cursor string) (int, error) {
	if cursor == "" {
		return 0, nil
	}
	offset, err := strconv.Atoi(cursor)
	if err != nil || offset < 0 {
		return 0, errors.New("workspace artifact cursor is invalid")
	}
	return offset, nil
}

func containsArtifactID(ids []string, artifactID string) bool {
	for _, id := range ids {
		if id == artifactID {
			return true
		}
	}
	return false
}

func (s *WorkspaceArtifacts) List(ctx context.Context, workspaceID string, first, offset int) ([]WorkspaceArtifact, int, error) {
	if s == nil || s.db == nil {
		return nil, 0, errors.New("workspace artifacts are unavailable")
	}
	if first < 1 {
		first = 10
	}
	if first > 100 {
		first = 100
	}
	if offset < 0 {
		offset = 0
	}
	var total int
	if err := s.db.QueryRow(ctx, `SELECT count(*)::int FROM workspace_artifacts WHERE workspace_id=$1 AND library_owned AND status='ready'`, workspaceID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.Query(ctx, `SELECT workspace_id,id::text,content_hash,display_name,canonical_media_type,size_bytes,created_at
		FROM workspace_artifacts WHERE workspace_id=$1 AND library_owned AND status='ready'
		ORDER BY created_at DESC,id DESC LIMIT $2 OFFSET $3`, workspaceID, first, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := make([]WorkspaceArtifact, 0, first)
	for rows.Next() {
		item := WorkspaceArtifact{EmbeddingStatus: "processing"}
		if err := rows.Scan(&item.WorkspaceID, &item.ArtifactID, &item.ContentHash, &item.FileName, &item.MediaType, &item.Size, &item.CreatedAt); err != nil {
			return nil, 0, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	ids := make([]string, len(items))
	for i := range items {
		ids[i] = items[i].ArtifactID
	}
	statuses, err := s.embeddingStatuses(ctx, workspaceID, ids)
	if err != nil {
		return nil, 0, err
	}
	for i := range items {
		if status, ok := statuses[items[i].ArtifactID]; ok {
			items[i].EmbeddingStatus = status
		}
	}
	return items, total, nil
}

func (s *WorkspaceArtifacts) embeddingStatuses(ctx context.Context, workspaceID string, artifactIDs []string) (map[string]string, error) {
	statuses := make(map[string]string, len(artifactIDs))
	if len(artifactIDs) == 0 || !s.EmbeddingAvailable(ctx) {
		return statuses, nil
	}
	rows, err := s.db.Query(ctx, `
		SELECT artifact.id::text,
			CASE
				WHEN projection.status='ready' AND projection.applied_content_revision=source.content_revision THEN 'ready'
				WHEN projection.status='failed' THEN 'failed'
				ELSE 'processing'
			END
		FROM workspace_artifacts artifact
		LEFT JOIN embedding_sources source ON source.workspace_id=artifact.workspace_id
			AND source.source_kind='artifact' AND source.source_key=artifact.id::text AND source.deleted_at IS NULL
		LEFT JOIN embedding_workspace_states state ON state.workspace_id=artifact.workspace_id
		LEFT JOIN embedding_projections projection ON projection.source_id=source.id AND projection.index_id=state.active_index_id
		WHERE artifact.workspace_id=$1 AND artifact.id::text=ANY($2::text[])`, workspaceID, artifactIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var artifactID, status string
		if err := rows.Scan(&artifactID, &status); err != nil {
			return nil, err
		}
		statuses[artifactID] = status
	}
	return statuses, rows.Err()
}

func (s *WorkspaceArtifacts) Remove(ctx context.Context, workspaceID, artifactID string) (bool, error) {
	if s == nil || s.db == nil {
		return false, errors.New("workspace artifacts are unavailable")
	}
	command, err := s.db.Exec(ctx, `UPDATE workspace_artifacts SET library_owned=false,updated_at=clock_timestamp()
		WHERE workspace_id=$1 AND id::text=$2 AND status='ready' AND library_owned`, workspaceID, artifactID)
	if err != nil {
		return false, err
	}
	if command.RowsAffected() == 0 {
		return false, errors.New("workspace artifact not found")
	}
	return true, nil
}

func canonicalArtifactMediaType(body []byte, declared string) string {
	mediaType := http.DetectContentType(body)
	if parsed, _, err := mime.ParseMediaType(mediaType); err == nil && parsed != "application/octet-stream" {
		return strings.ToLower(parsed)
	}
	if parsed, _, err := mime.ParseMediaType(declared); err == nil && parsed != "" {
		return strings.ToLower(parsed)
	}
	return "application/octet-stream"
}

func canonicalDeclaredArtifactMediaType(declared string) string {
	mediaType, _, _ := strings.Cut(declared, ";")
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	if mediaType == "" {
		return "application/octet-stream"
	}
	return mediaType
}

func validateArtifactBody(body []byte) error {
	if len(body) == 0 || int64(len(body)) > maxWorkspaceArtifactBytes {
		return errors.New("workspace artifact size is invalid")
	}
	return nil
}
