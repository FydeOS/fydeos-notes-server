// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

var ErrNotFound = errors.New("notification not found")

type Querier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

type Repository struct {
	db         Querier
	publicBase string
}

type Notification struct {
	ID        string
	Level     string
	Type      string
	Read      bool
	CreatedAt time.Time
	UpdatedAt time.Time
	Body      map[string]any
}

type CleanupResult struct {
	Count   int
	UserIDs []string
}

type InvitationNotification struct {
	UserID          string
	WorkspaceID     string
	CreatedByUserID string
	InviteID        string
	Type            string
}

type notificationDocKey struct {
	workspaceID string
	docID       string
}

func NewRepository(db Querier, publicBaseURL string) *Repository {
	return &Repository{db: db, publicBase: strings.TrimRight(publicBaseURL, "/")}
}

func (r *Repository) CountUnread(ctx context.Context, userID string) (int, error) {
	var count int
	err := r.db.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE user_id=$1 AND read=false`, userID).Scan(&count)
	return count, err
}

func (r *Repository) CleanupExpiredNotifications(ctx context.Context, cutoff time.Time, limit int) (CleanupResult, error) {
	if limit <= 0 {
		return CleanupResult{}, errors.New("notification cleanup limit must be positive")
	}
	rows, err := r.db.Query(ctx, `
		WITH expired AS (
			SELECT id
			FROM notifications
			WHERE created_at <= $1
			ORDER BY created_at,id
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		DELETE FROM notifications notification
		USING expired
		WHERE notification.id=expired.id
		RETURNING notification.user_id`, cutoff, limit)
	if err != nil {
		return CleanupResult{}, err
	}
	defer rows.Close()

	users := make(map[string]struct{})
	result := CleanupResult{}
	for rows.Next() {
		var userID string
		if err := rows.Scan(&userID); err != nil {
			return CleanupResult{}, err
		}
		result.Count++
		users[userID] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return CleanupResult{}, err
	}
	result.UserIDs = make([]string, 0, len(users))
	for userID := range users {
		result.UserIDs = append(result.UserIDs, userID)
	}
	sort.Strings(result.UserIDs)
	return result, nil
}

func (r *Repository) CreateMention(ctx context.Context, userID, workspaceID, createdByUserID string, doc map[string]any) (string, error) {
	id := uuid.NewString()
	body, err := json.Marshal(map[string]any{
		"workspaceId":     workspaceID,
		"createdByUserId": createdByUserID,
		"doc":             doc,
	})
	if err != nil {
		return "", err
	}
	_, err = r.db.Exec(ctx, `
		INSERT INTO notifications(id,user_id,level,read,type,body)
		VALUES ($1,$2,'Default',false,'Mention',$3::jsonb)`, id, userID, body)
	if err != nil {
		return "", err
	}
	return id, nil
}

func (r *Repository) CreateInvitation(ctx context.Context, input InvitationNotification) (string, bool, error) {
	if input.Type != "Invitation" && input.Type != "InvitationAccepted" && input.Type != "InvitationReviewRequest" && input.Type != "InvitationReviewApproved" && input.Type != "InvitationReviewDeclined" {
		return "", false, errors.New("unsupported invitation notification type")
	}
	if input.Type == "InvitationReviewApproved" {
		var recipientActive bool
		err := r.db.QueryRow(ctx, `SELECT EXISTS(
			SELECT 1 FROM workspace_members
			WHERE workspace_id=$1 AND user_id=$2 AND state='active'
		)`, input.WorkspaceID, input.UserID).Scan(&recipientActive)
		if err != nil {
			return "", false, err
		}
		if !recipientActive {
			return "", false, nil
		}
	} else if input.Type == "Invitation" {
		var inviteeActive bool
		err := r.db.QueryRow(ctx, `SELECT EXISTS(
			SELECT 1 FROM workspace_members
			WHERE workspace_id=$1 AND user_id=$2 AND state='active'
		)`, input.WorkspaceID, input.UserID).Scan(&inviteeActive)
		if err != nil {
			return "", false, err
		}
		if inviteeActive {
			return "", false, nil
		}
	} else if input.Type != "InvitationReviewDeclined" {
		var inviteeActive bool
		err := r.db.QueryRow(ctx, `SELECT EXISTS(
			SELECT 1 FROM workspace_members
			WHERE workspace_id=$1 AND user_id=$2 AND state='active'
		)`, input.WorkspaceID, input.CreatedByUserID).Scan(&inviteeActive)
		if err != nil {
			return "", false, err
		}
		if (input.Type == "Invitation" || input.Type == "InvitationAccepted") && !inviteeActive || input.Type == "InvitationReviewRequest" && inviteeActive {
			return "", false, nil
		}
	}
	id := uuid.NewString()
	bodyFields := map[string]string{"workspaceId": input.WorkspaceID, "createdByUserId": input.CreatedByUserID}
	if input.InviteID != "" {
		bodyFields["inviteId"] = input.InviteID
	}
	body, err := json.Marshal(bodyFields)
	if err != nil {
		return "", false, err
	}
	_, err = r.db.Exec(ctx, `
		INSERT INTO notifications(id,user_id,level,read,type,body)
		VALUES ($1,$2,'Default',false,$3::"NotificationType",$4::jsonb)`,
		id, input.UserID, input.Type, body)
	if err != nil {
		return "", false, err
	}
	return id, true, nil
}

func (r *Repository) ListUnread(ctx context.Context, userID string, after *time.Time, first, offset int) ([]Notification, error) {
	rows, err := r.db.Query(ctx, `
		SELECT id,level::text,type::text,read,created_at,updated_at,body
		FROM notifications
		WHERE user_id=$1 AND read=false AND ($2::timestamptz IS NULL OR created_at<$2)
		ORDER BY created_at DESC
		OFFSET $3 LIMIT $4`, userID, after, offset, first)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]Notification, 0)
	for rows.Next() {
		var item Notification
		var body []byte
		if err := rows.Scan(&item.ID, &item.Level, &item.Type, &item.Read, &item.CreatedAt, &item.UpdatedAt, &body); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(body, &item.Body); err != nil {
			return nil, err
		}
		if item.Body == nil {
			item.Body = map[string]any{}
		}
		item.Body["type"] = item.Type
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := r.enrichBodies(ctx, items); err != nil {
		return nil, err
	}
	return items, nil
}

func (r *Repository) MarkAsRead(ctx context.Context, userID, notificationID string) error {
	result, err := r.db.Exec(ctx, `
		UPDATE notifications SET read=true,updated_at=statement_timestamp()
		WHERE id=$1 AND user_id=$2`, notificationID, userID)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *Repository) MarkAllAsRead(ctx context.Context, userID string) error {
	_, err := r.db.Exec(ctx, `
		UPDATE notifications SET read=true,updated_at=statement_timestamp()
		WHERE user_id=$1`, userID)
	return err
}

func (r *Repository) enrichBodies(ctx context.Context, items []Notification) error {
	userIDs := make(map[string]struct{})
	workspaceIDs := make(map[string]struct{})
	docKeys := make(map[notificationDocKey]struct{})
	for _, item := range items {
		if id, ok := item.Body["createdByUserId"].(string); ok && id != "" {
			userIDs[id] = struct{}{}
		}
		if id, ok := item.Body["workspaceId"].(string); ok && id != "" {
			workspaceIDs[id] = struct{}{}
		}
		if notificationHasDocumentTitle(item.Type) {
			if workspaceID, ok := item.Body["workspaceId"].(string); ok && workspaceID != "" {
				if doc, ok := item.Body["doc"].(map[string]any); ok {
					if docID, ok := doc["id"].(string); ok && docID != "" {
						docKeys[notificationDocKey{workspaceID: workspaceID, docID: docID}] = struct{}{}
					}
				}
			}
		}
	}
	users, err := r.publicUsers(ctx, userIDs)
	if err != nil {
		return err
	}
	workspaces, err := r.workspaceInfo(ctx, workspaceIDs)
	if err != nil {
		return err
	}
	docTitles, err := r.documentTitles(ctx, docKeys)
	if err != nil {
		return err
	}
	for i := range items {
		body := items[i].Body
		if id, ok := body["createdByUserId"].(string); ok {
			if user, exists := users[id]; exists {
				body["createdByUser"] = user
			}
		}
		if id, ok := body["workspaceId"].(string); ok {
			if workspace, exists := workspaces[id]; exists {
				body["workspace"] = workspace
			}
		}
		if notificationHasDocumentTitle(items[i].Type) {
			if workspaceID, ok := body["workspaceId"].(string); ok {
				if doc, ok := body["doc"].(map[string]any); ok {
					if docID, ok := doc["id"].(string); ok {
						if title := docTitles[notificationDocKey{workspaceID: workspaceID, docID: docID}]; title != "" {
							doc["title"] = title
						}
					}
				}
			}
		}
	}
	return nil
}

func notificationHasDocumentTitle(notificationType string) bool {
	switch notificationType {
	case "Mention", "Comment", "CommentMention":
		return true
	default:
		return false
	}
}

func (r *Repository) documentTitles(ctx context.Context, keys map[notificationDocKey]struct{}) (map[notificationDocKey]string, error) {
	result := make(map[notificationDocKey]string, len(keys))
	if len(keys) == 0 {
		return result, nil
	}
	workspaceIDs := make([]string, 0, len(keys))
	docIDs := make([]string, 0, len(keys))
	for key := range keys {
		workspaceIDs = append(workspaceIDs, key.workspaceID)
		docIDs = append(docIDs, key.docID)
	}
	rows, err := r.db.Query(ctx, `
		SELECT page.workspace_id,page.page_id,page.title
		FROM unnest($1::text[],$2::text[]) AS requested(workspace_id,doc_id)
		JOIN workspace_pages page
		  ON page.workspace_id=requested.workspace_id AND page.page_id=requested.doc_id
		WHERE page.title IS NOT NULL AND page.title<>''`, workspaceIDs, docIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key notificationDocKey
		var title string
		if err := rows.Scan(&key.workspaceID, &key.docID, &title); err != nil {
			return nil, err
		}
		result[key] = title
	}
	return result, rows.Err()
}

func (r *Repository) publicUsers(ctx context.Context, ids map[string]struct{}) (map[string]map[string]any, error) {
	result := make(map[string]map[string]any, len(ids))
	if len(ids) == 0 {
		return result, nil
	}
	values := stringSet(ids)
	rows, err := r.db.Query(ctx, `SELECT id,name,avatar_url FROM users WHERE id=ANY($1::text[])`, values)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		var avatar pgtype.Text
		if err := rows.Scan(&id, &name, &avatar); err != nil {
			return nil, err
		}
		user := map[string]any{"id": id, "name": name}
		if avatar.Valid {
			user["avatarUrl"] = avatar.String
		}
		result[id] = user
	}
	return result, rows.Err()
}

func (r *Repository) workspaceInfo(ctx context.Context, ids map[string]struct{}) (map[string]map[string]any, error) {
	result := make(map[string]map[string]any, len(ids))
	if len(ids) == 0 {
		return result, nil
	}
	rows, err := r.db.Query(ctx, `SELECT id,name FROM workspaces WHERE id=ANY($1::text[])`, stringSet(ids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var name pgtype.Text
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		workspaceName := "Untitled Workspace"
		if name.Valid && name.String != "" {
			workspaceName = name.String
		}
		workspace := map[string]any{"id": id, "name": workspaceName}
		if r.publicBase != "" {
			workspace["url"] = r.publicBase + "/workspace/" + url.PathEscape(id)
		}
		result[id] = workspace
	}
	return result, rows.Err()
}

func stringSet(ids map[string]struct{}) []string {
	values := make([]string, 0, len(ids))
	for id := range ids {
		values = append(values, id)
	}
	return values
}
