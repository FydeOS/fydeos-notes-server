// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package blobs

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"

	"github.com/Deln0r/ygo"
	"github.com/jackc/pgx/v5"
)

const (
	maxSourceBytes    = 16 << 20
	maxSourceUpdates  = 10000
	maxBlockChildren  = 10000
	maxBlocks         = 100000
	maxReferenceBytes = 1024
	maxReferences     = 10000
	maxRootPages      = 100000
	maxTimestampYear  = 262000
)

func validateSource(source BlobSource) error {
	if source.WorkspaceID == "" || source.DocID == "" {
		return ErrManifestInvalidInput
	}
	switch source.Type {
	case "currentDoc":
	case "history":
		if source.TimestampMS == nil {
			return ErrManifestInvalidInput
		}
		year := time.UnixMilli(*source.TimestampMS).UTC().Year()
		if year > maxTimestampYear || year < -maxTimestampYear {
			return ErrManifestInvalidInput
		}
	default:
		return ErrManifestInvalidInput
	}
	return nil
}

// sourceStamp hashes length-prefixed parts.
func sourceStamp(parts []string) string {
	h := sha256.New()
	var prefix [8]byte
	for _, part := range parts {
		binary.BigEndian.PutUint64(prefix[:], uint64(len(part)))
		h.Write(prefix[:])
		h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func stampTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// loadedSource is a decoded document state.
type loadedSource struct {
	doc   *ygo.Doc
	stamp string
}

func loadSource(ctx context.Context, tx pgx.Tx, source BlobSource, withDoc bool) (*loadedSource, error) {
	if source.Type == "history" {
		var blob []byte
		ts := time.UnixMilli(*source.TimestampMS).UTC()
		column := "octet_length(blob)"
		if withDoc {
			column = "blob"
		}
		var length int64
		var err error
		if withDoc {
			err = tx.QueryRow(ctx, `SELECT `+column+` FROM snapshot_histories WHERE workspace_id = $1 AND guid = $2 AND "timestamp" = $3`,
				source.WorkspaceID, source.DocID, ts).Scan(&blob)
			length = int64(len(blob))
		} else {
			err = tx.QueryRow(ctx, `SELECT `+column+` FROM snapshot_histories WHERE workspace_id = $1 AND guid = $2 AND "timestamp" = $3`,
				source.WorkspaceID, source.DocID, ts).Scan(&length)
		}
		if isNoRows(err) {
			return nil, ErrManifestNotFound
		}
		if err != nil {
			return nil, err
		}
		if length > maxSourceBytes {
			return nil, errManifestTooLarge
		}
		out := &loadedSource{stamp: sourceStamp([]string{strconv.FormatInt(*source.TimestampMS, 10)})}
		if withDoc {
			if out.doc, err = applyUpdates([][]byte{blob}); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	var snapshot []byte
	var snapshotAt *time.Time
	hasSnapshot := true
	snapshotQuery := `SELECT NULL::bytea, updated_at FROM snapshots WHERE workspace_id = $1 AND guid = $2`
	if withDoc {
		snapshotQuery = `SELECT blob, updated_at FROM snapshots WHERE workspace_id = $1 AND guid = $2`
	}
	err := tx.QueryRow(ctx, snapshotQuery, source.WorkspaceID, source.DocID).Scan(&snapshot, &snapshotAt)
	if isNoRows(err) {
		hasSnapshot = false
	} else if err != nil {
		return nil, err
	}
	updateQuery := `SELECT NULL::bytea, created_at FROM updates WHERE workspace_id = $1 AND guid = $2 ORDER BY created_at LIMIT $3`
	if withDoc {
		updateQuery = `SELECT blob, created_at FROM updates WHERE workspace_id = $1 AND guid = $2 ORDER BY created_at LIMIT $3`
	}
	rows, err := tx.Query(ctx, updateQuery, source.WorkspaceID, source.DocID, maxSourceUpdates+1)
	if err != nil {
		return nil, err
	}
	var updates [][]byte
	stamps := []string{stampTime(snapshotAt)}
	count := 0
	total := int64(len(snapshot))
	for rows.Next() {
		var blob []byte
		var at *time.Time
		if err := rows.Scan(&blob, &at); err != nil {
			rows.Close()
			return nil, err
		}
		count++
		total += int64(len(blob))
		updates = append(updates, blob)
		stamps = append(stamps, stampTime(at))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !hasSnapshot && count == 0 {
		return nil, ErrManifestNotFound
	}
	if count > maxSourceUpdates {
		return nil, fmt.Errorf("%w: too many updates", errManifestProjection)
	}
	if total > maxSourceBytes {
		return nil, errManifestTooLarge
	}
	out := &loadedSource{stamp: sourceStamp(stamps)}
	if withDoc {
		all := make([][]byte, 0, len(updates)+1)
		if len(snapshot) > 0 {
			all = append(all, snapshot)
		}
		all = append(all, updates...)
		if out.doc, err = applyUpdates(all); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func applyUpdates(updates [][]byte) (*ygo.Doc, error) {
	doc := ygo.NewDoc()
	for _, update := range updates {
		if len(update) == 0 {
			continue
		}
		if err := ygo.ValidateUpdate(update); err != nil {
			return nil, fmt.Errorf("%w: invalid update", errManifestProjection)
		}
		if err := ygo.ApplyUpdate(doc, update); err != nil {
			return nil, fmt.Errorf("%w: invalid update", errManifestProjection)
		}
	}
	if ygo.HasPending(doc) {
		return nil, fmt.Errorf("%w: pending structs", errManifestProjection)
	}
	if len(ygo.EncodeStateAsUpdate(doc)) > maxSourceBytes {
		return nil, errManifestTooLarge
	}
	return doc, nil
}

func yString(value any) (string, bool) {
	switch v := value.(type) {
	case string:
		return v, true
	case *ygo.Text:
		return v.String(), true
	}
	return "", false
}

// referencedKeys returns the blob keys a document references.
func referencedKeys(doc *ygo.Doc, rootDoc bool) (map[string]bool, error) {
	blocksMap := ygo.NewMap(doc, "blocks")
	meta := ygo.NewMap(doc, "meta")
	keys := map[string]bool{}
	add := func(key string) error {
		if key == "" {
			return nil
		}
		if len(key) > maxReferenceBytes {
			return fmt.Errorf("%w: reference too long", errManifestProjection)
		}
		keys[key] = true
		if len(keys) > maxReferences {
			return fmt.Errorf("%w: too many references", errManifestProjection)
		}
		return nil
	}
	if rootDoc {
		if avatar, ok := yString(meta.Get("avatar")); ok {
			if err := add(avatar); err != nil {
				return nil, err
			}
		}
	}
	if blocksMap.Len() == 0 {
		return keys, nil
	}
	blocks := map[string]*ygo.Map{}
	var roots []*ygo.Map
	blocksMap.Range(func(id string, value any) bool {
		block, ok := value.(*ygo.Map)
		if !ok {
			return true
		}
		blocks[id] = block
		if sysID, ok := yString(block.Get("sys:id")); ok && sysID != "" {
			if _, exists := blocks[sysID]; !exists {
				blocks[sysID] = block
			}
			if flavour, _ := yString(block.Get("sys:flavour")); flavour == "affine:page" {
				roots = append(roots, block)
			}
		}
		return true
	})
	if len(roots) != 1 {
		return nil, fmt.Errorf("%w: expected one page root, found %d", errManifestProjection, len(roots))
	}
	visited := map[*ygo.Map]bool{roots[0]: true}
	queue := []*ygo.Map{roots[0]}
	for len(queue) > 0 {
		block := queue[0]
		queue = queue[1:]
		flavour, _ := yString(block.Get("sys:flavour"))
		if flavour == "affine:attachment" || flavour == "affine:image" {
			if source, ok := yString(block.Get("prop:sourceId")); ok {
				if err := add(source); err != nil {
					return nil, err
				}
			}
		}
		children, ok := block.Get("sys:children").(*ygo.Array)
		if !ok {
			continue
		}
		if children.Len() > maxBlockChildren {
			return nil, fmt.Errorf("%w: too many children", errManifestProjection)
		}
		for _, child := range children.ToSlice() {
			id, ok := child.(string)
			if !ok {
				return nil, fmt.Errorf("%w: invalid child id", errManifestProjection)
			}
			next := blocks[id]
			if next == nil || visited[next] {
				continue
			}
			visited[next] = true
			if len(visited) > maxBlocks {
				return nil, fmt.Errorf("%w: too many blocks", errManifestProjection)
			}
			queue = append(queue, next)
		}
	}
	return keys, nil
}

// rootPageIDs lists meta.pages[*].id of a workspace root document.
func rootPageIDs(doc *ygo.Doc) ([]string, error) {
	meta := ygo.NewMap(doc, "meta")
	pages, ok := meta.Get("pages").(*ygo.Array)
	if !ok {
		return nil, fmt.Errorf("%w: missing pages", errManifestProjection)
	}
	if pages.Len() > maxRootPages {
		return nil, fmt.Errorf("%w: too many pages", errManifestProjection)
	}
	var ids []string
	for _, page := range pages.ToSlice() {
		entry, ok := page.(*ygo.Map)
		if !ok {
			continue
		}
		if id, ok := yString(entry.Get("id")); ok && id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}
