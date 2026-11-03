// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package workspaces

import (
	"context"
	"fmt"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/documents"
)

const (
	maxHistoryRows  = 4096
	maxHistoryBytes = 64 << 20
)

// docRevision identifies the stored state of a document's history.
type docRevision struct {
	key    string
	latest *time.Time
}

func readRevision(ctx context.Context, q queryer, workspace, guid string) (docRevision, error) {
	var snapAt, updAt *time.Time
	var version *string
	err := q.QueryRow(ctx, `
		SELECT s.updated_at, s.xmin::text, (SELECT max(created_at) FROM updates WHERE workspace_id=$1 AND guid=$2)
		FROM (SELECT 1) one LEFT JOIN snapshots s ON s.workspace_id=$1 AND s.guid=$2`, workspace, guid).Scan(&snapAt, &version, &updAt)
	if err != nil {
		return docRevision{}, fmt.Errorf("read document revision: %w", err)
	}
	rev := docRevision{}
	f := func(t *time.Time) string {
		if t == nil {
			return "-"
		}
		return t.UTC().Format(time.RFC3339Nano)
	}
	v := "-"
	if version != nil {
		v = *version
	}
	rev.key = f(snapAt) + "|" + v + "|" + f(updAt)
	rev.latest = snapAt
	if updAt != nil && (rev.latest == nil || updAt.After(*rev.latest)) {
		rev.latest = updAt
	}
	return rev, nil
}

// loadHistory returns the snapshot followed by updates, enforcing limits.
func loadHistory(ctx context.Context, q queryer, workspace, guid string) ([][]byte, error) {
	var count, total, largest int64
	err := q.QueryRow(ctx, `
		SELECT count(*), COALESCE(sum(len),0), COALESCE(max(len),0) FROM (
			SELECT octet_length(blob) AS len FROM snapshots WHERE workspace_id=$1 AND guid=$2
			UNION ALL SELECT octet_length(blob) FROM updates WHERE workspace_id=$1 AND guid=$2
		) h`, workspace, guid).Scan(&count, &total, &largest)
	if err != nil {
		return nil, fmt.Errorf("measure document history: %w", err)
	}
	if count > maxHistoryRows || total > maxHistoryBytes || largest > documents.MaxUpdateBytes {
		return nil, ErrPreviewYjsHistoryTooLarge
	}
	rows, err := q.Query(ctx, `
		SELECT blob FROM (
			SELECT blob, 0 AS part, updated_at AS at FROM snapshots WHERE workspace_id=$1 AND guid=$2
			UNION ALL SELECT blob, 1, created_at FROM updates WHERE workspace_id=$1 AND guid=$2
		) h ORDER BY part, at`, workspace, guid)
	if err != nil {
		return nil, fmt.Errorf("load document history: %w", err)
	}
	defer rows.Close()
	var out [][]byte
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
