// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package workspaces

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// DocumentAnalyticsService records and reports document views.
type DocumentAnalyticsService struct {
	repository *Repository
	redis      redis.Cmdable
}

func NewDocumentAnalyticsService(repository *Repository, redisClient redis.Cmdable) *DocumentAnalyticsService {
	return &DocumentAnalyticsService{repository: repository, redis: redisClient}
}

func (s *DocumentAnalyticsService) ready() error {
	if s == nil || s.repository == nil {
		return ErrAuthorizationUnavailable
	}
	return nil
}

// DocVisitorID derives a stable anonymous visitor id.
func DocVisitorID(workspace, doc, tracker string) string {
	sum := sha256.Sum256([]byte(workspace + ":" + doc + ":" + tracker))
	return hex.EncodeToString(sum[:])
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func utcDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func timezoneOf(tz *string) string {
	if tz == nil || strings.TrimSpace(*tz) == "" {
		return "UTC"
	}
	return strings.TrimSpace(*tz)
}

// dayWindow builds a window of n days ending today (UTC).
func dayWindow(now time.Time, requested, effective int, tz string) DocAnalyticsTimeWindow {
	to := utcDay(now)
	return DocAnalyticsTimeWindow{From: to.AddDate(0, 0, -(effective - 1)), To: to, Timezone: tz, Bucket: "Day", RequestedSize: requested, EffectiveSize: effective}
}

func (s *DocumentAnalyticsService) team(ctx context.Context, workspace string) (bool, error) {
	team, known, err := teamFlag(ctx, s.repository.db, workspace)
	if err != nil {
		return false, err
	}
	if !known {
		return false, ErrQuotaStateUnknown
	}
	return team, nil
}

// GetDocPageAnalytics returns daily views of a document.
func (s *DocumentAnalyticsService) GetDocPageAnalytics(ctx context.Context, actor, workspace, doc string, input DocPageAnalyticsInput) (DocPageAnalytics, error) {
	var out DocPageAnalytics
	if err := s.ready(); err != nil {
		return out, err
	}
	team, err := s.team(ctx, workspace)
	if err != nil {
		return out, err
	}
	def, max := 7, 7
	if team {
		def, max = 28, 90
	}
	requested := def
	if input.WindowDays != nil {
		requested = *input.WindowDays
	}
	now := time.Now().UTC()
	out.Window = dayWindow(now, requested, clamp(requested, 1, max), timezoneOf(input.Timezone))
	out.GeneratedAt = now
	rows, err := s.repository.db.Query(ctx, `SELECT date, total_views, unique_views, guest_views, last_accessed_at FROM workspace_doc_view_daily
		WHERE workspace_id=$1 AND doc_id=$2 AND date BETWEEN $3::date AND $4::date`, workspace, doc, out.Window.From, out.Window.To)
	if err != nil {
		return out, fmt.Errorf("read doc analytics: %w", err)
	}
	byDay := map[string]DocPageAnalyticsPoint{}
	for rows.Next() {
		var p DocPageAnalyticsPoint
		var last *time.Time
		if err := rows.Scan(&p.Date, &p.TotalViews, &p.UniqueViews, &p.GuestViews, &last); err != nil {
			rows.Close()
			return out, err
		}
		byDay[p.Date.Format("2006-01-02")] = p
		if last != nil && (out.Summary.LastAccessedAt == nil || last.After(*out.Summary.LastAccessedAt)) {
			out.Summary.LastAccessedAt = last
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	for d := out.Window.From; !d.After(out.Window.To); d = d.AddDate(0, 0, 1) {
		p := byDay[d.Format("2006-01-02")]
		p.Date = d
		out.Series = append(out.Series, p)
		out.Summary.TotalViews += p.TotalViews
		out.Summary.UniqueViews += p.UniqueViews
		out.Summary.GuestViews += p.GuestViews
	}
	return out, nil
}

// PaginateDocLastAccessedMembers lists members whose latest view was doc.
func (s *DocumentAnalyticsService) PaginateDocLastAccessedMembers(ctx context.Context, actor, workspace, doc string, pagination DocMemberLastAccessPagination, query *string, includeTotal bool) (DocMemberLastAccessPage, error) {
	page := DocMemberLastAccessPage{Members: []DocMemberLastAccess{}}
	if err := s.ready(); err != nil {
		return page, err
	}
	first := pagination.First
	if first == 0 {
		first = 10
	}
	first = clamp(first, 1, 50)
	offset := max(pagination.Offset, 0)
	keyword := ""
	if query != nil {
		keyword = strings.TrimSpace(*query)
	}
	if utf16Len(keyword) > MemberSearchMaxLength {
		return page, ErrDocAnalyticsQueryTooLong
	}
	cursor, err := decodeMemberCursor(pagination.After)
	if err != nil {
		return page, err
	}
	team, err := s.team(ctx, workspace)
	if err != nil {
		return page, err
	}
	var since *time.Time
	if !team {
		since = ptr(utcDay(time.Now()).AddDate(0, 0, -6))
	}
	const base = `
		FROM workspace_member_last_access la
		JOIN workspace_members m ON m.workspace_id=la.workspace_id AND m.user_id=la.user_id AND m.state='active'
		JOIN users u ON u.id=la.user_id
		WHERE la.workspace_id=$1 AND la.last_doc_id=$2
		  AND ($3::timestamptz IS NULL OR la.last_accessed_at >= $3)
		  AND ($4 = '' OR strpos(lower(u.name), lower($4)) > 0 OR strpos(lower(u.email), lower($4)) > 0)`
	var cAt *time.Time
	var cID *string
	if cursor != nil {
		cAt, cID = &cursor.at, &cursor.userID
	}
	rows, err := s.repository.db.Query(ctx, `SELECT u.id, u.name, u.avatar_url, la.last_accessed_at, la.last_doc_id `+base+`
		AND ($5::timestamptz IS NULL OR la.last_accessed_at < $5 OR (la.last_accessed_at = $5 AND la.user_id > $6))
		ORDER BY la.last_accessed_at DESC, la.user_id OFFSET $7 LIMIT $8`, workspace, doc, since, keyword, cAt, cID, offset, first+1)
	if err != nil {
		return page, fmt.Errorf("list doc members: %w", err)
	}
	for rows.Next() {
		var m DocMemberLastAccess
		if err := rows.Scan(&m.UserID, &m.Name, &m.AvatarURL, &m.LastAccessedAt, &m.LastDocID); err != nil {
			rows.Close()
			return page, err
		}
		page.Members = append(page.Members, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return page, err
	}
	if len(page.Members) > first {
		page.HasNextPage = true
		page.Members = page.Members[:first]
	}
	page.HasPreviousPage = cursor != nil || offset > 0
	if n := len(page.Members); n > 0 {
		start, err := EncodeDocMemberLastAccessCursor(page.Members[0])
		if err != nil {
			return page, err
		}
		end, err := EncodeDocMemberLastAccessCursor(page.Members[n-1])
		if err != nil {
			return page, err
		}
		page.StartCursor, page.EndCursor = &start, &end
	}
	if includeTotal {
		var total int
		if err := s.repository.db.QueryRow(ctx, `SELECT count(*) `+base, workspace, doc, since, keyword).Scan(&total); err != nil {
			return page, err
		}
		page.TotalCount = &total
	}
	return page, nil
}

// RecordDocView counts one view of a document.
func (s *DocumentAnalyticsService) RecordDocView(ctx context.Context, workspace, doc, visitorID string, isGuest bool, userID string, viewedAt time.Time) error {
	if err := s.ready(); err != nil {
		return err
	}
	if viewedAt.IsZero() {
		viewedAt = time.Now()
	}
	viewedAt = viewedAt.UTC()
	day := utcDay(viewedAt)
	unique := true
	if s.redis != nil {
		key := "analytics:doc_uv:" + workspace + ":" + doc + ":" + day.Format("2006-01-02")
		if added, err := s.redis.SAdd(ctx, key, visitorID).Result(); err == nil {
			unique = added > 0
			if unique {
				_ = s.redis.Expire(ctx, key, 90*24*time.Hour).Err()
			}
		}
	}
	u, g := int64(0), int64(0)
	if unique {
		u = 1
	}
	if isGuest {
		g = 1
	}
	db := s.repository.db
	if _, err := db.Exec(ctx, `
		INSERT INTO workspace_doc_view_daily(workspace_id, doc_id, date, total_views, unique_views, guest_views, last_accessed_at, updated_at)
		VALUES ($1,$2,$3::date,1,$4,$5,$6,now())
		ON CONFLICT (workspace_id, doc_id, date) DO UPDATE SET
		  total_views=workspace_doc_view_daily.total_views+1,
		  unique_views=workspace_doc_view_daily.unique_views+EXCLUDED.unique_views,
		  guest_views=workspace_doc_view_daily.guest_views+EXCLUDED.guest_views,
		  last_accessed_at=GREATEST(workspace_doc_view_daily.last_accessed_at, EXCLUDED.last_accessed_at),
		  updated_at=now()`, workspace, doc, day, u, g, viewedAt); err != nil {
		return fmt.Errorf("record doc view: %w", err)
	}
	if userID == "" {
		return nil
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO workspace_member_last_access(workspace_id, user_id, last_accessed_at, last_doc_id, updated_at)
		SELECT $1::text, $2::text, $3::timestamptz, $4::text, now() WHERE EXISTS (SELECT 1 FROM users WHERE id=$2::text)
		ON CONFLICT (workspace_id, user_id) DO UPDATE SET
		  last_doc_id=CASE WHEN EXCLUDED.last_accessed_at >= workspace_member_last_access.last_accessed_at THEN EXCLUDED.last_doc_id ELSE workspace_member_last_access.last_doc_id END,
		  last_accessed_at=GREATEST(workspace_member_last_access.last_accessed_at, EXCLUDED.last_accessed_at),
		  updated_at=now()`, workspace, userID, viewedAt, doc); err != nil {
		return fmt.Errorf("record member access: %w", err)
	}
	return nil
}

const sharedLinkBaseSQL = `
	WITH links AS (
		SELECT p.workspace_id, p.doc_id, wp.title, COALESCE(wp.published_at, (to_jsonb(p)->>'published_at')::timestamptz) AS published_at, s.updated_at AS doc_updated_at, s.updated_by,
		       (SELECT o.user_id FROM workspace_members o WHERE o.workspace_id=p.workspace_id AND o.role='owner' AND o.state='active' ORDER BY o.created_at, o.id LIMIT 1) AS owner,
		       COALESCE(v.views,0) AS views, COALESCE(v.uniq,0) AS uniq, COALESCE(v.guests,0) AS guests, v.last_at
		FROM doc_access_policies p
		LEFT JOIN workspace_pages wp ON wp.workspace_id=p.workspace_id AND wp.page_id=p.doc_id
		LEFT JOIN snapshots s ON s.workspace_id=p.workspace_id AND s.guid=p.doc_id
		LEFT JOIN LATERAL (
			SELECT sum(total_views)::bigint AS views, sum(unique_views)::bigint AS uniq, sum(guest_views)::bigint AS guests, max(last_accessed_at) AS last_at
			FROM workspace_doc_view_daily d WHERE d.workspace_id=p.workspace_id AND d.doc_id=p.doc_id AND d.date BETWEEN $1::date AND $2::date
		) v ON true
		WHERE p.visibility='public' AND p.public_role='external'
	)`

// PaginateAdminSharedLinks lists published links for the admin console.
func (s *DocumentAnalyticsService) PaginateAdminSharedLinks(ctx context.Context, input AdminSharedLinksInput) (AdminSharedLinksPage, error) {
	page := AdminSharedLinksPage{Edges: []AdminSharedLinkEdge{}}
	if err := s.ready(); err != nil {
		return page, err
	}
	first := 10
	if input.First != nil {
		first = clamp(*input.First, 1, MaxAdminSharedLinksPageSize)
	}
	offset := 0
	if input.Offset != nil && *input.Offset > 0 {
		offset = *input.Offset
	}
	keyword := ""
	if input.Keyword != nil {
		keyword = strings.TrimSpace(*input.Keyword)
	}
	if utf16Len(keyword) > MemberSearchMaxLength {
		return page, ErrAdminSharedLinksQueryTooLong
	}
	order := input.OrderBy
	if order == "" {
		order = AdminSharedLinksUpdatedAtDesc
	}
	var sortExpr string
	switch order {
	case AdminSharedLinksUpdatedAtDesc:
		sortExpr = `COALESCE(doc_updated_at, 'epoch'::timestamptz)`
	case AdminSharedLinksPublishedDesc:
		sortExpr = `COALESCE(published_at, 'epoch'::timestamptz)`
	case AdminSharedLinksViewsDesc:
		sortExpr = `views`
	default:
		return page, ErrInvalidAdminSharedLinksCursor
	}
	cursor, err := decodeSharedLinkCursor(input.After, order)
	if err != nil {
		return page, err
	}
	requested := DefaultAdminSharedLinksWindowDays
	if input.AnalyticsWindowDays != nil {
		requested = *input.AnalyticsWindowDays
	}
	page.AnalyticsWindow = dayWindow(time.Now(), requested, clamp(requested, 1, MaxAdminSharedLinksWindowDays), "UTC")
	var wsFilter *string
	if input.WorkspaceID != nil && *input.WorkspaceID != "" {
		wsFilter = input.WorkspaceID
	}
	filter := `
		WHERE ($3 = '' OR strpos(lower(COALESCE(title,'')), lower($3)) > 0 OR strpos(lower(doc_id), lower($3)) > 0 OR strpos(lower(workspace_id), lower($3)) > 0)
		  AND ($4::text IS NULL OR workspace_id=$4)
		  AND ($5::timestamptz IS NULL OR doc_updated_at >= $5)`
	args := []any{page.AnalyticsWindow.From, page.AnalyticsWindow.To, keyword, wsFilter, input.UpdatedAfter}
	if input.IncludeTotal {
		var total int
		if err := s.repository.db.QueryRow(ctx, sharedLinkBaseSQL+` SELECT count(*) FROM links`+filter, args...).Scan(&total); err != nil {
			return page, fmt.Errorf("count shared links: %w", err)
		}
		page.TotalCount = &total
	}
	cursorSQL := ""
	if cursor != nil {
		var v any = cursor.date
		cast := "::timestamptz"
		if order == AdminSharedLinksViewsDesc {
			v, cast = cursor.number, "::bigint"
		}
		args = append(args, v, cursor.workspaceID, cursor.docID)
		cursorSQL = fmt.Sprintf(` AND (%[1]s < $6%[2]s OR (%[1]s = $6%[2]s AND (workspace_id, doc_id) > ($7::text, $8::text)))`, sortExpr, cast)
	}
	n := len(args)
	args = append(args, offset, first+1)
	rows, err := s.repository.db.Query(ctx, sharedLinkBaseSQL+`
		SELECT workspace_id, doc_id, title, published_at, doc_updated_at, owner, updated_by, views, uniq, guests, last_at,
		       COALESCE(doc_updated_at, 'epoch'::timestamptz), COALESCE(published_at, 'epoch'::timestamptz)
		FROM links`+filter+cursorSQL+fmt.Sprintf(`
		ORDER BY %s DESC, workspace_id, doc_id OFFSET $%d LIMIT $%d`, sortExpr, n+1, n+2), args...)
	if err != nil {
		return page, fmt.Errorf("list shared links: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var l AdminSharedLink
		var updatedSort, publishedSort time.Time
		if err := rows.Scan(&l.WorkspaceID, &l.DocID, &l.Title, &l.PublishedAt, &l.DocUpdatedAt, &l.WorkspaceOwner, &l.LastUpdaterID,
			&l.Views, &l.UniqueViews, &l.GuestViews, &l.LastAccessedAt, &updatedSort, &publishedSort); err != nil {
			return page, err
		}
		switch order {
		case AdminSharedLinksPublishedDesc:
			l.SortValueDate = publishedSort.UTC()
		case AdminSharedLinksViewsDesc:
			l.SortValueNumber = l.Views
		default:
			l.SortValueDate = updatedSort.UTC()
		}
		page.Edges = append(page.Edges, AdminSharedLinkEdge{Cursor: encodeSharedLinkCursor(order, l), Node: l})
	}
	if err := rows.Err(); err != nil {
		return page, err
	}
	if len(page.Edges) > first {
		page.HasNextPage = true
		page.Edges = page.Edges[:first]
	}
	page.HasPreviousPage = cursor != nil || offset > 0
	if n := len(page.Edges); n > 0 {
		page.StartCursor, page.EndCursor = ptr(page.Edges[0].Cursor), ptr(page.Edges[n-1].Cursor)
	}
	return page, nil
}

func optionalInt(v *int, def int) int {
	if v == nil {
		return def
	}
	return *v
}

// GetAdminDashboard summarizes instance activity.
func (s *DocumentAnalyticsService) GetAdminDashboard(ctx context.Context, input AdminDashboardInput) (AdminDashboard, error) {
	var out AdminDashboard
	if err := s.ready(); err != nil {
		return out, err
	}
	db := s.repository.db
	tz := timezoneOf(input.Timezone)
	now, err := dbNow(ctx, db)
	if err != nil {
		return out, err
	}
	now = now.UTC()
	out.GeneratedAt = now

	reqHours := optionalInt(input.SyncHistoryHours, 48)
	hours := clamp(reqHours, 1, 72)
	end := now.Truncate(time.Minute)
	start := end.Add(-time.Duration(hours*60-1) * time.Minute)
	out.SyncWindow = AdminDashboardTimeWindow{From: start, To: end, Timezone: tz, Bucket: "Minute", RequestedSize: reqHours, EffectiveSize: hours}
	samples := map[int64]int{}
	rows, err := db.Query(ctx, `SELECT minute_ts, active_users FROM sync_active_users_minutely WHERE minute_ts BETWEEN $1 AND $2`, start, end)
	if err != nil {
		return out, fmt.Errorf("read sync activity: %w", err)
	}
	for rows.Next() {
		var t time.Time
		var n int
		if err := rows.Scan(&t, &n); err != nil {
			rows.Close()
			return out, err
		}
		samples[t.UTC().Truncate(time.Minute).Unix()] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	last := 0
	out.SyncActiveUsersTimeline = make([]AdminDashboardMinutePoint, 0, hours*60)
	for t := start; !t.After(end); t = t.Add(time.Minute) {
		if v, ok := samples[t.Unix()]; ok {
			last = v
		}
		out.SyncActiveUsersTimeline = append(out.SyncActiveUsersTimeline, AdminDashboardMinutePoint{Minute: t, ActiveUsers: last})
	}
	out.SyncActiveUsers = last

	reqCopilot := optionalInt(input.CopilotWindowDays, 7)
	copilotDays := clamp(reqCopilot, 1, 90)
	copilotFrom := now.AddDate(0, 0, -copilotDays)
	out.CopilotWindow = AdminDashboardTimeWindow{From: copilotFrom, To: now, Timezone: tz, Bucket: "Day", RequestedSize: reqCopilot, EffectiveSize: copilotDays}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM ai_sessions_messages WHERE role='user' AND created_at >= $1 AND created_at <= $2`, copilotFrom, now).Scan(&out.CopilotConversations); err != nil {
		return out, fmt.Errorf("count copilot conversations: %w", err)
	}

	reqStorage := optionalInt(input.StorageHistoryDays, 30)
	storageDays := clamp(reqStorage, 1, 90)
	today := utcDay(now)
	storageFrom := today.AddDate(0, 0, -(storageDays - 1))
	out.StorageWindow = AdminDashboardTimeWindow{From: storageFrom, To: today, Timezone: tz, Bucket: "Day", RequestedSize: reqStorage, EffectiveSize: storageDays}
	if err := db.QueryRow(ctx, `SELECT COALESCE(sum(snapshot_size),0)::bigint, COALESCE(sum(blob_size),0)::bigint FROM workspace_admin_stats`).
		Scan(&out.WorkspaceStorageBytes, &out.BlobStorageBytes); err != nil {
		return out, fmt.Errorf("read storage totals: %w", err)
	}
	var seedWS, seedBlob int64
	if err := db.QueryRow(ctx, `SELECT COALESCE(sum(snapshot_size),0)::bigint, COALESCE(sum(blob_size),0)::bigint FROM workspace_admin_stats_daily
		WHERE date = (SELECT max(date) FROM workspace_admin_stats_daily WHERE date < $1::date)`, storageFrom).Scan(&seedWS, &seedBlob); err != nil {
		return out, fmt.Errorf("read storage history seed: %w", err)
	}
	type pair struct{ ws, blob int64 }
	daily := map[string]pair{}
	rows, err = db.Query(ctx, `SELECT date, sum(snapshot_size)::bigint, sum(blob_size)::bigint FROM workspace_admin_stats_daily
		WHERE date BETWEEN $1::date AND $2::date GROUP BY date`, storageFrom, today)
	if err != nil {
		return out, fmt.Errorf("read storage history: %w", err)
	}
	for rows.Next() {
		var d time.Time
		var p pair
		if err := rows.Scan(&d, &p.ws, &p.blob); err != nil {
			rows.Close()
			return out, err
		}
		daily[d.Format("2006-01-02")] = p
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	cur := pair{seedWS, seedBlob}
	for d := storageFrom; !d.After(today); d = d.AddDate(0, 0, 1) {
		if p, ok := daily[d.Format("2006-01-02")]; ok {
			cur = p
		}
		if d.Equal(today) {
			cur = pair{out.WorkspaceStorageBytes, out.BlobStorageBytes}
		}
		out.WorkspaceStorageHistory = append(out.WorkspaceStorageHistory, AdminDashboardValueDayPoint{Date: d, Value: cur.ws})
		out.BlobStorageHistory = append(out.BlobStorageHistory, AdminDashboardValueDayPoint{Date: d, Value: cur.blob})
	}

	reqLinks := optionalInt(input.SharedLinkWindowDays, DefaultAdminSharedLinksWindowDays)
	linkDays := clamp(reqLinks, 1, MaxAdminSharedLinksWindowDays)
	w := dayWindow(now, reqLinks, linkDays, tz)
	out.TopSharedLinksWindow = AdminDashboardTimeWindow(w)
	out.TopSharedLinks = []AdminDashboardTopSharedLink{}
	if input.IncludeTopSharedLinks {
		rows, err := db.Query(ctx, sharedLinkBaseSQL+`
			SELECT workspace_id, doc_id, title, published_at, views, uniq, guests, last_at FROM links
			ORDER BY views DESC, uniq DESC, workspace_id, doc_id LIMIT 10`, w.From, w.To)
		if err != nil {
			return out, fmt.Errorf("read top shared links: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var l AdminDashboardTopSharedLink
			if err := rows.Scan(&l.WorkspaceID, &l.DocID, &l.Title, &l.PublishedAt, &l.Views, &l.UniqueViews, &l.GuestViews, &l.LastAccessedAt); err != nil {
				return out, err
			}
			out.TopSharedLinks = append(out.TopSharedLinks, l)
		}
		if err := rows.Err(); err != nil {
			return out, err
		}
	}
	return out, nil
}
