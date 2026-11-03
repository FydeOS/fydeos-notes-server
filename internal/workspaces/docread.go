// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package workspaces

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/documents"
	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
)

type DocumentOwnerRepairGate interface {
	ShouldRepairDocOwner(context.Context, string, string) bool
}

type WorkspaceMetadataNotifier interface {
	WorkspaceMetadataBackfilled(string)
}

type DocumentOwnerRepairNotifier interface {
	PublishDocumentRoleChanged(workspaceID, docID, reason string)
}

// RedisDocumentOwnerRepairGate lets one caller per day repair a doc owner.
type RedisDocumentOwnerRepairGate struct {
	client redis.Cmdable
}

func NewRedisDocumentOwnerRepairGate(client redis.Cmdable) *RedisDocumentOwnerRepairGate {
	return &RedisDocumentOwnerRepairGate{client: client}
}

func (g *RedisDocumentOwnerRepairGate) ShouldRepairDocOwner(ctx context.Context, workspace, doc string) bool {
	if g == nil || g.client == nil {
		return false
	}
	ok, err := g.client.SetNX(ctx, "fixingOwner:"+workspace+":"+doc, 1, 24*time.Hour).Result()
	return err == nil && ok
}

const (
	maxConcurrentProjections = 2
	previewCacheTTL          = 2 * time.Minute
	previewCacheCapacity     = 512
	previewSummaryLength     = 150
)

type previewEntry struct {
	revision string
	at       time.Time
	title    string
	summary  string
	avatar   string
	found    bool
	err      error
}

// DocumentReadService serves document reads and previews.
type DocumentReadService struct {
	repository    *Repository
	gate          DocumentOwnerRepairGate
	mu            sync.Mutex
	wsNotifier    WorkspaceMetadataNotifier
	ownerNotifier DocumentOwnerRepairNotifier
	slots         chan struct{}
	cache         map[string]previewEntry
}

func NewDocumentReadService(repository *Repository, gate DocumentOwnerRepairGate) *DocumentReadService {
	return &DocumentReadService{
		repository: repository,
		gate:       gate,
		slots:      make(chan struct{}, maxConcurrentProjections),
		cache:      map[string]previewEntry{},
	}
}

func (s *DocumentReadService) SetWorkspaceMetadataNotifier(notifier WorkspaceMetadataNotifier) {
	s.mu.Lock()
	s.wsNotifier = notifier
	s.mu.Unlock()
}

func (s *DocumentReadService) SetDocumentOwnerRepairNotifier(notifier DocumentOwnerRepairNotifier) {
	s.mu.Lock()
	s.ownerNotifier = notifier
	s.mu.Unlock()
}

func (s *DocumentReadService) ready() error {
	if s == nil || s.repository == nil {
		return ErrAuthorizationUnavailable
	}
	return nil
}

// ReadWorkspace returns a workspace for a member, or anyone when it is
// public with sharing on.
func (s *DocumentReadService) ReadWorkspace(ctx context.Context, actor, workspace string) (Workspace, error) {
	if err := s.ready(); err != nil {
		return Workspace{}, err
	}
	r := s.repository
	if actor != "" {
		ws, err := r.GetForUser(ctx, workspace, actor)
		if err == nil || !errors.Is(err, ErrAccessDenied) {
			return ws, err
		}
	}
	exists, err := workspaceExists(ctx, r.db, workspace)
	if err != nil {
		return Workspace{}, err
	}
	if !exists {
		return Workspace{}, ErrNotFound
	}
	public, sharing, _, err := workspaceExposure(ctx, r.db, workspace)
	if err != nil {
		return Workspace{}, err
	}
	if !public || !sharing {
		return Workspace{}, ErrAccessDenied
	}
	return r.readWorkspaceAny(ctx, workspace)
}

// readPermissionFacts reads one consistent permission view.
func (r *Repository) readPermissionFacts(ctx context.Context, actor, workspace string, docIDs []string, withPreview bool) (factsBundle, bool, error) {
	var b factsBundle
	var commercial bool
	err := r.inTx(ctx, readOnlySnapshot, func(tx pgx.Tx) error {
		var err error
		if b, err = r.loadBundle(ctx, tx, actor, workspace, docIDs); err != nil {
			return err
		}
		if withPreview {
			if err = loadPreviewExtras(ctx, tx, &b, workspace, docIDs); err != nil {
				return err
			}
		}
		if !b.snap.WorkspaceExists {
			return nil
		}
		if b.snap.Generation, err = readGeneration(ctx, tx, workspace); err != nil {
			return err
		}
		commercial, err = r.commercial(ctx, tx, workspace)
		return err
	})
	return b, commercial, err
}

func (s *DocumentReadService) ReadDocumentPermissions(ctx context.Context, actor, workspace, doc string) (map[string]bool, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	b, commercial, err := s.repository.readPermissionFacts(ctx, actor, workspace, []string{doc}, true)
	if err != nil {
		return nil, err
	}
	return b.doc(doc, commercial).permissions(true), nil
}

func (s *DocumentReadService) ReadDocumentPermissionsBatch(ctx context.Context, actor, workspace string, docIDs []string) (map[string]map[string]bool, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	ids := sortedUnique(docIDs)
	out := make(map[string]map[string]bool, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	b, commercial, err := s.repository.readPermissionFacts(ctx, actor, workspace, ids, true)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		f := b.doc(id, commercial)
		out[id] = f.permissions(f.pageExists)
	}
	return out, nil
}

func (s *DocumentReadService) ReadDocumentHistoryAccess(ctx context.Context, actor, workspace, doc string) (DocumentHistoryAccess, error) {
	if err := s.ready(); err != nil {
		return DocumentHistoryAccess{}, err
	}
	b, commercial, err := s.repository.readPermissionFacts(ctx, actor, workspace, []string{doc}, false)
	if err != nil {
		return DocumentHistoryAccess{}, err
	}
	p := b.doc(doc, commercial).permissions(false)
	return DocumentHistoryAccess{Read: p["Doc.History.Read"], Update: p["Doc.Update"], Generation: b.snap.Generation}, nil
}

const docInfoSQL = `
	SELECT wp.page_id, wp.mode, wp.title, wp.summary,
	       COALESCE(p.visibility='public' AND p.public_role='external', false), COALESCE(p.member_default_role, 'manager'),
	       LEAST(s.created_at, u.first_at), GREATEST(s.updated_at, u.last_at),
	       COALESCE(s.created_by, u.first_by),
	       CASE WHEN u.last_at IS NOT NULL AND (s.updated_at IS NULL OR u.last_at > s.updated_at) THEN u.last_by ELSE s.updated_by END,
	       p.published_at
	FROM workspace_pages wp
	LEFT JOIN doc_access_policies p ON p.workspace_id=wp.workspace_id AND p.doc_id=wp.page_id
	LEFT JOIN snapshots s ON s.workspace_id=wp.workspace_id AND s.guid=wp.page_id
	LEFT JOIN LATERAL (
		SELECT min(created_at) AS first_at, max(created_at) AS last_at,
		       (array_agg(created_by ORDER BY created_at))[1] AS first_by,
		       (array_agg(created_by ORDER BY created_at DESC))[1] AS last_by
		FROM updates WHERE workspace_id=wp.workspace_id AND guid=wp.page_id
	) u ON true
	WHERE wp.workspace_id=$1`

type docInfoRow struct {
	info        DocumentInfo
	publishedAt *time.Time
}

func scanDocInfos(ctx context.Context, q queryer, workspace, tail string, args ...any) ([]docInfoRow, error) {
	rows, err := q.Query(ctx, docInfoSQL+tail, append([]any{workspace}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("read documents: %w", err)
	}
	defer rows.Close()
	var out []docInfoRow
	for rows.Next() {
		var row docInfoRow
		d := &row.info
		d.WorkspaceID = workspace
		if err := rows.Scan(&d.DocID, &d.Mode, &d.Title, &d.Summary, &d.Public, &d.DefaultRole, &d.CreatedAt, &d.UpdatedAt, &d.CreatorID, &d.LastUpdaterID, &row.publishedAt); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// ReadDocument returns document metadata after an optional owner repair.
func (s *DocumentReadService) ReadDocument(ctx context.Context, actor, workspace, doc string) (DocumentInfo, error) {
	if err := s.ready(); err != nil {
		return DocumentInfo{}, err
	}
	r := s.repository
	if s.gate != nil && s.gate.ShouldRepairDocOwner(ctx, workspace, doc) {
		changed, err := s.repairOwner(ctx, workspace, doc)
		if err != nil {
			return DocumentInfo{}, err
		}
		if changed {
			s.mu.Lock()
			n := s.ownerNotifier
			s.mu.Unlock()
			if n != nil {
				n.PublishDocumentRoleChanged(workspace, doc, "owner-repaired")
			}
		}
	}
	rows, err := scanDocInfos(ctx, r.db, workspace, ` AND wp.page_id=$2`, doc)
	if err != nil {
		return DocumentInfo{}, err
	}
	if len(rows) == 0 {
		var public bool
		if err := r.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM doc_access_policies WHERE workspace_id=$1 AND doc_id=$2 AND visibility='public' AND public_role='external')`,
			workspace, doc).Scan(&public); err != nil {
			return DocumentInfo{}, err
		}
		return DocumentInfo{PublishedDocumentMetadata: PublishedDocumentMetadata{WorkspaceID: workspace, DocID: doc, DefaultRole: "manager", Public: public}}, nil
	}
	perms, err := s.ReadDocumentPermissions(ctx, actor, workspace, doc)
	if err != nil {
		return DocumentInfo{}, err
	}
	if !perms["Doc.Read"] {
		return DocumentInfo{}, ErrDocRoleDenied
	}
	return rows[0].info, nil
}

// repairOwner grants an owner to a doc that has content but no ACL.
func (s *DocumentReadService) repairOwner(ctx context.Context, workspace, doc string) (bool, error) {
	r := s.repository
	changed := false
	err := r.runDocMutation(ctx, docMutation{
		workspace: workspace, doc: doc,
		extraLocks: []string{"permission:doc-owner:" + workspace + ":" + doc},
		apply: func(tx pgx.Tx) error {
			ok, err := docHasContent(ctx, tx, workspace, doc)
			if err != nil || !ok {
				return err
			}
			var hasOwner, hasPage, hasGrant, hasPolicy bool
			if err := tx.QueryRow(ctx, `SELECT
				EXISTS(SELECT 1 FROM doc_grants WHERE workspace_id=$1 AND doc_id=$2 AND principal_type='user' AND role='owner'),
				EXISTS(SELECT 1 FROM workspace_pages WHERE workspace_id=$1 AND page_id=$2),
				EXISTS(SELECT 1 FROM doc_grants WHERE workspace_id=$1 AND doc_id=$2),
				EXISTS(SELECT 1 FROM doc_access_policies WHERE workspace_id=$1 AND doc_id=$2)`, workspace, doc).
				Scan(&hasOwner, &hasPage, &hasGrant, &hasPolicy); err != nil {
				return err
			}
			if hasOwner || (hasPage && (hasGrant || hasPolicy)) {
				return nil
			}
			var creator *string
			err = tx.QueryRow(ctx, `SELECT s.created_by FROM snapshots s JOIN users u ON u.id=s.created_by WHERE s.workspace_id=$1 AND s.guid=$2`, workspace, doc).Scan(&creator)
			if err != nil && !noRows(err) {
				return err
			}
			target := ""
			if creator != nil {
				target = *creator
			}
			if target == "" {
				if target, err = earliestOwner(ctx, tx, workspace); err != nil {
					return err
				}
				if target == "" {
					return ErrOwnerMissing
				}
			}
			if _, err := tx.Exec(ctx, `INSERT INTO doc_grants(workspace_id, doc_id, principal_type, principal_id, role) VALUES ($1,$2,'user',$3,'owner')
				ON CONFLICT (workspace_id, doc_id, principal_type, principal_id) DO UPDATE SET role='owner', updated_at=CURRENT_TIMESTAMP`, workspace, doc, target); err != nil {
				return err
			}
			changed = true
			return nil
		},
	})
	return changed, err
}

// ReadPublicDocuments lists published documents, oldest publication first.
func (s *DocumentReadService) ReadPublicDocuments(ctx context.Context, workspace string) ([]DocumentInfo, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	rows, err := scanDocInfos(ctx, s.repository.db, workspace, ` AND p.visibility='public' AND p.public_role='external' ORDER BY p.published_at, wp.page_id`)
	if err != nil {
		return nil, err
	}
	out := make([]DocumentInfo, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.info)
	}
	return out, nil
}

// PaginateWorkspaceDocuments lists documents by creation, or readable
// documents by recent update.
func (s *DocumentReadService) PaginateWorkspaceDocuments(ctx context.Context, actor, workspace string, input DocumentListInput) (DocumentListPage, error) {
	page := DocumentListPage{Documents: []DocumentInfo{}}
	if err := s.ready(); err != nil {
		return page, err
	}
	if err := validatePage(input.First, input.Offset, input.After); err != nil {
		return page, err
	}
	r := s.repository
	if !input.RecentlyUpdated {
		if err := r.db.QueryRow(ctx, `SELECT count(*) FROM workspace_pages WHERE workspace_id=$1`, workspace).Scan(&page.TotalCount); err != nil {
			return page, err
		}
		rows, err := scanDocInfos(ctx, r.db, workspace, `
			AND ($2::timestamptz IS NULL OR LEAST(s.created_at, u.first_at) > $2)
			ORDER BY LEAST(s.created_at, u.first_at) NULLS LAST, wp.page_id OFFSET $3 LIMIT $4`, input.After, input.Offset, input.First)
		if err != nil {
			return page, err
		}
		for _, row := range rows {
			page.Documents = append(page.Documents, row.info)
		}
		return page, nil
	}
	if _, err := s.ReadWorkspace(ctx, actor, workspace); err != nil {
		return page, err
	}
	rows, err := scanDocInfos(ctx, r.db, workspace, `
		AND ($2::timestamptz IS NULL OR GREATEST(s.updated_at, u.last_at) < $2)
		ORDER BY GREATEST(s.updated_at, u.last_at) DESC NULLS LAST, wp.page_id`, input.After)
	if err != nil {
		return page, err
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.info.DocID)
	}
	perms, err := s.ReadDocumentPermissionsBatch(ctx, actor, workspace, ids)
	if err != nil {
		return page, err
	}
	var readable []DocumentInfo
	for _, row := range rows {
		if perms[row.info.DocID]["Doc.Read"] {
			row.info.Summary = nil
			readable = append(readable, row.info)
		}
	}
	page.TotalCount = len(readable)
	start := min(input.Offset, len(readable))
	end := min(start+input.First, len(readable))
	page.Documents = append(page.Documents, readable[start:end]...)
	return page, nil
}

type authorRow struct {
	id, guid             string
	createdAt, updatedAt time.Time
	createdBy, updatedBy *string
}

func (s *DocumentReadService) readAuthorRows(ctx context.Context, workspace string, docIDs []string) (map[string]DocumentAuthors, error) {
	r := s.repository
	rows, err := r.db.Query(ctx, `
		SELECT d.id, LEAST(s.created_at, u.first_at), GREATEST(s.updated_at, u.last_at),
		       COALESCE(s.created_by, u.first_by),
		       CASE WHEN u.last_at IS NOT NULL AND (s.updated_at IS NULL OR u.last_at > s.updated_at) THEN u.last_by ELSE s.updated_by END
		FROM unnest($2::text[]) AS d(id)
		LEFT JOIN snapshots s ON s.workspace_id=$1 AND s.guid=d.id
		LEFT JOIN LATERAL (
			SELECT min(created_at) AS first_at, max(created_at) AS last_at,
			       (array_agg(created_by ORDER BY created_at))[1] AS first_by,
			       (array_agg(created_by ORDER BY created_at DESC))[1] AS last_by
			FROM updates WHERE workspace_id=$1 AND guid=d.id
		) u ON true
		WHERE s.guid IS NOT NULL OR u.first_at IS NOT NULL`, workspace, docIDs)
	if err != nil {
		return nil, fmt.Errorf("read document authors: %w", err)
	}
	var items []authorRow
	for rows.Next() {
		var a authorRow
		if err := rows.Scan(&a.id, &a.createdAt, &a.updatedAt, &a.createdBy, &a.updatedBy); err != nil {
			rows.Close()
			return nil, err
		}
		items = append(items, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make(map[string]DocumentAuthors, len(items))
	users := map[string]*DocumentAuthor{}
	lookup := func(id *string) (*DocumentAuthor, error) {
		if id == nil {
			return nil, nil
		}
		if a, ok := users[*id]; ok {
			return a, nil
		}
		a, err := readAuthor(ctx, r.db, *id, true)
		if err != nil {
			return nil, err
		}
		users[*id] = a
		return a, nil
	}
	for _, a := range items {
		cb, err := lookup(a.createdBy)
		if err != nil {
			return nil, err
		}
		ub, err := lookup(a.updatedBy)
		if err != nil {
			return nil, err
		}
		out[a.id] = DocumentAuthors{CreatedAt: a.createdAt, UpdatedAt: a.updatedAt, CreatedBy: cb, UpdatedBy: ub}
	}
	return out, nil
}

func readAuthor(ctx context.Context, q queryer, id string, includeDisabled bool) (*DocumentAuthor, error) {
	var a DocumentAuthor
	var disabled bool
	err := q.QueryRow(ctx, `SELECT id, name, avatar_url, disabled FROM users WHERE id=$1`, id).Scan(&a.ID, &a.Name, &a.AvatarURL, &disabled)
	if noRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if disabled && !includeDisabled {
		return nil, nil
	}
	return &a, nil
}

func (s *DocumentReadService) ReadDocumentAuthors(ctx context.Context, workspace, doc string) (DocumentAuthors, error) {
	if err := s.ready(); err != nil {
		return DocumentAuthors{}, err
	}
	m, err := s.readAuthorRows(ctx, workspace, []string{doc})
	if err != nil {
		return DocumentAuthors{}, err
	}
	a, ok := m[doc]
	if !ok {
		return DocumentAuthors{}, ErrDocumentNotFound
	}
	return a, nil
}

func (s *DocumentReadService) ReadDocumentAuthorsBatch(ctx context.Context, workspace string, docIDs []string) (map[string]DocumentAuthors, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	ids := sortedUnique(docIDs)
	if len(ids) == 0 {
		return map[string]DocumentAuthors{}, nil
	}
	return s.readAuthorRows(ctx, workspace, ids)
}

func (s *DocumentReadService) ReadDocumentAuthor(ctx context.Context, id string) (*DocumentAuthor, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	return readAuthor(ctx, s.repository.db, id, false)
}

func (s *DocumentReadService) ReadDocumentGrants(ctx context.Context, actor, workspace, doc string, first, offset int, after *time.Time) (DocGrantPage, error) {
	if err := s.ready(); err != nil {
		return DocGrantPage{}, err
	}
	perms, err := s.ReadDocumentPermissions(ctx, actor, workspace, doc)
	if err != nil {
		return DocGrantPage{}, err
	}
	if !perms["Doc.Users.Read"] {
		return DocGrantPage{}, ErrDocRoleDenied
	}
	return s.repository.listGrants(ctx, workspace, doc, first, offset, after, true)
}

func (s *DocumentReadService) ReadDocumentMarkdown(ctx context.Context, workspace, doc string) (string, bool, error) {
	if err := s.ready(); err != nil {
		return "", false, err
	}
	perms, err := s.ReadDocumentPermissions(ctx, "", workspace, doc)
	if err != nil {
		return "", false, err
	}
	if !perms["Doc.Read"] {
		return "", false, nil
	}
	updates, err := loadHistory(ctx, s.repository.db, workspace, doc)
	if err != nil {
		return "", false, err
	}
	md, found, err := documents.ProjectMarkdown(updates)
	if err != nil || !found {
		return "", false, err
	}
	return md.Markdown, true, nil
}

func (s *DocumentReadService) ReadDocumentCanvas(ctx context.Context, actor, workspace, doc string) (documents.CanvasProjection, bool, error) {
	var zero documents.CanvasProjection
	if err := s.ready(); err != nil {
		return zero, false, err
	}
	perms, err := s.ReadDocumentPermissions(ctx, actor, workspace, doc)
	if err != nil {
		return zero, false, err
	}
	if !perms["Doc.Read"] {
		return zero, false, nil
	}
	rev, err := readRevision(ctx, s.repository.db, workspace, doc)
	if err != nil {
		return zero, false, err
	}
	updates, err := loadHistory(ctx, s.repository.db, workspace, doc)
	if err != nil {
		return zero, false, err
	}
	if len(updates) == 0 || rev.latest == nil {
		return zero, false, nil
	}
	return documents.ProjectCanvas(updates, doc, rev.latest.UTC().Format(time.RFC3339Nano))
}

// project runs fn under the projection limiter, failing fast when busy.
func (s *DocumentReadService) project(fn func() error) error {
	select {
	case s.slots <- struct{}{}:
	default:
		return ErrPreviewProjectionBusy
	}
	defer func() { <-s.slots }()
	return fn()
}

// cachedMetadata returns projected metadata for a doc or the root.
func (s *DocumentReadService) cachedMetadata(ctx context.Context, workspace, guid string, root bool) (previewEntry, error) {
	rev, err := readRevision(ctx, s.repository.db, workspace, guid)
	if err != nil {
		return previewEntry{}, err
	}
	key := workspace + "\x00" + guid
	if root {
		key = workspace + "\x00root"
	}
	now := time.Now()
	s.mu.Lock()
	if e, ok := s.cache[key]; ok && e.revision == rev.key && now.Sub(e.at) < previewCacheTTL {
		s.mu.Unlock()
		return e, e.err
	}
	s.mu.Unlock()
	entry := previewEntry{revision: rev.key, at: now}
	err = s.project(func() error {
		updates, err := loadHistory(ctx, s.repository.db, workspace, guid)
		if err != nil {
			return err
		}
		if root {
			m, found, err := documents.ProjectWorkspaceMetadata(updates)
			if err != nil {
				return err
			}
			entry.found, entry.title, entry.avatar = found, m.Name, m.AvatarKey
			return nil
		}
		m, found, err := documents.ProjectPageMetadata(updates, previewSummaryLength)
		if err != nil {
			return err
		}
		entry.found, entry.title, entry.summary = found, m.Title, m.Summary
		return nil
	})
	if err != nil && !errors.Is(err, ErrPreviewYjsHistoryTooLarge) && !errors.Is(err, documents.ErrMetadataProjection) {
		return previewEntry{}, err
	}
	entry.err = err
	s.mu.Lock()
	if len(s.cache) >= previewCacheCapacity {
		s.cache = map[string]previewEntry{}
	}
	s.cache[key] = entry
	s.mu.Unlock()
	return entry, err
}

// ReadDocumentPreview returns link-preview metadata for a doc.
func (s *DocumentReadService) ReadDocumentPreview(ctx context.Context, workspace, doc string) (PreviewResult, error) {
	result := PreviewResult{Exposure: PreviewDenied}
	if err := s.ready(); err != nil {
		return result, err
	}
	b, err := s.repository.loadBundle(ctx, s.repository.db, "", workspace, []string{doc})
	if err != nil {
		return result, err
	}
	if err := loadPreviewExtras(ctx, s.repository.db, &b, workspace, []string{doc}); err != nil {
		return result, err
	}
	if !b.snap.WorkspaceExists || !b.snap.SharingEnabled {
		return result, nil
	}
	acl := b.snap.Documents[doc]
	switch {
	case acl.VisibilityPublic && acl.PublicRole == "external" && acl.Published:
		result.Exposure = PreviewPublicIndexable
	case b.docURLPreview[doc] || b.wsURLPreview:
		result.Exposure = PreviewMetadataNoIndex
	default:
		return result, nil
	}
	ok, err := docHasContent(ctx, s.repository.db, workspace, doc)
	if err != nil {
		return result, err
	}
	if !ok {
		return result, nil
	}
	entry, err := s.cachedMetadata(ctx, workspace, doc, false)
	if err != nil {
		return result, err
	}
	result.Present = true
	result.Title, result.Summary = entry.title, entry.summary
	return result, nil
}

// ReadWorkspacePreview returns link-preview metadata for a workspace and
// backfills its stored name from the root document when missing.
func (s *DocumentReadService) ReadWorkspacePreview(ctx context.Context, workspace string) (PreviewResult, error) {
	result := PreviewResult{Exposure: PreviewDenied}
	if err := s.ready(); err != nil {
		return result, err
	}
	r := s.repository
	exists, err := workspaceExists(ctx, r.db, workspace)
	if err != nil || !exists {
		return result, err
	}
	public, sharing, preview, err := workspaceExposure(ctx, r.db, workspace)
	if err != nil || !sharing {
		return result, err
	}
	switch {
	case public:
		result.Exposure = PreviewPublicIndexable
	case preview:
		result.Exposure = PreviewMetadataNoIndex
	default:
		return result, nil
	}
	result.Present = true
	name, avatar, err := storedWorkspaceName(ctx, r.db, workspace)
	if err != nil {
		return result, err
	}
	if name != "" {
		result.Title, result.AvatarKey = name, avatar
		return result, nil
	}
	ok, err := docHasContent(ctx, r.db, workspace, workspace)
	if err != nil || !ok {
		return result, err
	}
	before, err := readRevision(ctx, r.db, workspace, workspace)
	if err != nil {
		return result, err
	}
	entry, err := s.cachedMetadata(ctx, workspace, workspace, true)
	if err != nil {
		if errors.Is(err, ErrPreviewYjsHistoryTooLarge) || errors.Is(err, documents.ErrMetadataProjection) {
			return result, nil
		}
		return result, err
	}
	if !entry.found || entry.title == "" {
		return result, nil
	}
	result.Title, result.AvatarKey = entry.title, entry.avatar
	if entry.revision != before.key {
		return result, nil
	}
	backfilled, err := s.backfillName(ctx, workspace, before.key, entry.title, entry.avatar)
	if err != nil {
		return result, nil
	}
	if backfilled {
		s.mu.Lock()
		n := s.wsNotifier
		s.mu.Unlock()
		if n != nil {
			n.WorkspaceMetadataBackfilled(workspace)
		}
		return result, nil
	}
	if name, avatar, err := storedWorkspaceName(ctx, r.db, workspace); err == nil && name != "" {
		result.Title, result.AvatarKey = name, avatar
	}
	return result, nil
}

func storedWorkspaceName(ctx context.Context, q queryer, workspace string) (string, string, error) {
	var name, avatar *string
	err := q.QueryRow(ctx, `SELECT name, avatar_key FROM workspaces WHERE id=$1`, workspace).Scan(&name, &avatar)
	if noRows(err) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	n, a := "", ""
	if name != nil {
		n = *name
	}
	if avatar != nil {
		a = *avatar
	}
	return n, a, nil
}

func (s *DocumentReadService) backfillName(ctx context.Context, workspace, revision, name, avatar string) (bool, error) {
	r := s.repository
	done := false
	err := r.inTx(ctx, nil, func(tx pgx.Tx) error {
		if err := setLockTimeout(ctx, tx, time.Second); err != nil {
			return err
		}
		ok, err := tryLockExclusive(ctx, tx, domainLockKey(workspace))
		if err != nil || !ok {
			return err
		}
		rev, err := readRevision(ctx, tx, workspace, workspace)
		if err != nil {
			return err
		}
		if rev.key != revision {
			return nil
		}
		tag, err := tx.Exec(ctx, `UPDATE workspaces SET name=$2, avatar_key=COALESCE(NULLIF($3,''), avatar_key) WHERE id=$1 AND COALESCE(name,'')=''`,
			workspace, name, avatar)
		if err != nil {
			return err
		}
		done = tag.RowsAffected() > 0
		return nil
	})
	return done, err
}
