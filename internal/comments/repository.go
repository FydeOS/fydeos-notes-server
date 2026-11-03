// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package comments

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

var ErrNotFound = errors.New("comment not found")
var ErrReplyNotFound = errors.New("reply not found")
var ErrWriteUnavailable = errors.New("comment write service is unavailable")
var ErrCommentsCreateForbidden = errors.New("document_comments_create_forbidden")
var ErrCommentsModerateForbidden = errors.New("document_comments_moderate_forbidden")

type PermissionDeniedError struct {
	Action      string
	WorkspaceID string
	DocID       string
	Cause       error
}

func (e *PermissionDeniedError) Error() string { return e.Cause.Error() }
func (e *PermissionDeniedError) Unwrap() error { return e.Cause }

type Querier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

type Repository struct {
	db         Querier
	beginner   transactionBeginner
	authorizer WriteAuthorizer
}

type transactionBeginner interface {
	Begin(context.Context) (pgx.Tx, error)
}

type WriteAuthorizer interface {
	AuthorizeDocumentInTx(context.Context, pgx.Tx, string, string, string) (workspaces.DocumentAuthorization, error)
}

type User struct {
	ID        string
	Name      string
	AvatarURL *string
}

type Reply struct {
	ID                    string
	CommentID             string
	WorkspaceID           string
	DocID                 string
	Content               map[string]any
	CreatedAt             time.Time
	UpdatedAt             time.Time
	User                  User
	NotificationReceivers []CreatedNotification
}

type Comment struct {
	SID                   int64
	ID                    string
	WorkspaceID           string
	DocID                 string
	Content               map[string]any
	Resolved              bool
	CreatedAt             time.Time
	UpdatedAt             time.Time
	User                  User
	Replies               []Reply
	NotificationReceivers []CreatedNotification
}

type NotificationInput struct {
	DocTitle string
	DocMode  string
	Mentions []string
}

type CreatedNotification struct {
	ID     string
	UserID string
}

type Change struct {
	Action    string
	ID        string
	CommentID *string
	UpdatedAt time.Time
	Item      map[string]any
}

func NewRepository(db Querier) *Repository {
	beginner, _ := db.(transactionBeginner)
	return &Repository{db: db, beginner: beginner}
}

func NewRepositoryWithAuthorizer(db Querier, authorizer WriteAuthorizer) *Repository {
	repository := NewRepository(db)
	repository.authorizer = authorizer
	return repository
}

func (r *Repository) Count(ctx context.Context, workspaceID, docID string) (int, error) {
	var count int
	err := r.db.QueryRow(ctx, `SELECT count(*) FROM comments WHERE workspace_id=$1 AND doc_id=$2 AND deleted_at IS NULL`, workspaceID, docID).Scan(&count)
	return count, err
}

func (r *Repository) List(ctx context.Context, workspaceID, docID string, afterSID *int64, limit int) ([]Comment, error) {
	rows, err := r.db.Query(ctx, `
		SELECT c.sid,c.id,c.workspace_id,c.doc_id,c.content,
		       c.resolved,c.created_at,c.updated_at,
		       c.user_id,u.name,u.avatar_url
		FROM comments c
		JOIN users u ON u.id=c.user_id
		WHERE c.workspace_id=$1 AND c.doc_id=$2 AND c.deleted_at IS NULL
		  AND ($3::bigint IS NULL OR c.sid<$3)
		ORDER BY c.sid DESC
		LIMIT $4`, workspaceID, docID, afterSID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	comments := make([]Comment, 0)
	ids := make([]string, 0)
	for rows.Next() {
		var item Comment
		var content []byte
		var avatar pgtype.Text
		if err := rows.Scan(
			&item.SID, &item.ID, &item.WorkspaceID, &item.DocID, &content,
			&item.Resolved, &item.CreatedAt, &item.UpdatedAt,
			&item.User.ID, &item.User.Name, &avatar,
		); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(content, &item.Content); err != nil {
			return nil, err
		}
		item.User.AvatarURL = nullableText(avatar)
		item.Replies = []Reply{}
		comments = append(comments, item)
		ids = append(ids, item.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return comments, nil
	}

	replyRows, err := r.db.Query(ctx, `
		SELECT r.id,r.comment_id,r.workspace_id,r.doc_id,r.content,r.created_at,r.updated_at,
		       r.user_id,u.name,u.avatar_url
		FROM replies r
		JOIN users u ON u.id=r.user_id
		WHERE r.comment_id=ANY($1::text[]) AND r.deleted_at IS NULL
		ORDER BY r.sid ASC`, ids)
	if err != nil {
		return nil, err
	}
	defer replyRows.Close()
	index := make(map[string]int, len(comments))
	for i := range comments {
		index[comments[i].ID] = i
	}
	for replyRows.Next() {
		var item Reply
		var content []byte
		var avatar pgtype.Text
		if err := replyRows.Scan(
			&item.ID, &item.CommentID, &item.WorkspaceID, &item.DocID, &content, &item.CreatedAt, &item.UpdatedAt,
			&item.User.ID, &item.User.Name, &avatar,
		); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(content, &item.Content); err != nil {
			return nil, err
		}
		item.User.AvatarURL = nullableText(avatar)
		if commentIndex, ok := index[item.CommentID]; ok {
			comments[commentIndex].Replies = append(comments[commentIndex].Replies, item)
		}
	}
	if err := replyRows.Err(); err != nil {
		return nil, err
	}
	return comments, nil
}

func (r *Repository) ListChanges(ctx context.Context, workspaceID, docID string, commentAfter, replyAfter *time.Time, limit int) ([]Change, error) {
	comments, err := r.listCommentChanges(ctx, workspaceID, docID, commentAfter, limit)
	if err != nil {
		return nil, err
	}
	replies, err := r.listReplyChanges(ctx, workspaceID, docID, replyAfter, limit)
	if err != nil {
		return nil, err
	}
	return append(comments, replies...), nil
}

func (r *Repository) listCommentChanges(ctx context.Context, workspaceID, docID string, after *time.Time, limit int) ([]Change, error) {
	rows, err := r.db.Query(ctx, `
		SELECT c.sid,c.id,c.workspace_id,c.doc_id,c.user_id,c.content,
		       c.resolved,c.created_at,c.updated_at,c.deleted_at,
		       u.name,u.avatar_url
		FROM comments c
		LEFT JOIN users u ON u.id=c.user_id
		WHERE c.workspace_id=$1 AND c.doc_id=$2
		  AND ($3::timestamptz IS NULL OR c.updated_at>$3)
		ORDER BY c.updated_at ASC
		LIMIT $4`, workspaceID, docID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	changes := make([]Change, 0)
	for rows.Next() {
		var sid int64
		var id, workspaceID, docID, authorID string
		var content []byte
		var resolved bool
		var createdAt, updatedAt time.Time
		var deletedAt pgtype.Timestamptz
		var name, avatar pgtype.Text
		if err := rows.Scan(&sid, &id, &workspaceID, &docID, &authorID, &content, &resolved, &createdAt, &updatedAt, &deletedAt, &name, &avatar); err != nil {
			return nil, err
		}
		change := Change{ID: id, UpdatedAt: updatedAt}
		if deletedAt.Valid {
			change.Action = "delete"
			change.Item = map[string]any{
				"deletedAt": deletedAt.Time.UTC().Format(time.RFC3339Nano),
				"updatedAt": updatedAt.UTC().Format(time.RFC3339Nano),
			}
		} else {
			var decoded map[string]any
			if err := json.Unmarshal(content, &decoded); err != nil {
				return nil, err
			}
			user := map[string]any{"id": authorID, "name": name.String}
			if avatar.Valid {
				user["avatarUrl"] = avatar.String
			}
			change.Action = "update"
			change.Item = map[string]any{
				"sid": sid, "id": id, "workspaceId": workspaceID, "docId": docID,
				"content": decoded, "resolved": resolved,
				"createdAt": createdAt.UTC().Format(time.RFC3339Nano),
				"updatedAt": updatedAt.UTC().Format(time.RFC3339Nano),
				"deletedAt": nil, "userId": authorID, "user": user,
			}
		}
		changes = append(changes, change)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return changes, nil
}

func (r *Repository) listReplyChanges(ctx context.Context, workspaceID, docID string, after *time.Time, limit int) ([]Change, error) {
	rows, err := r.db.Query(ctx, `
		SELECT r.sid,r.id,r.comment_id,r.workspace_id,r.doc_id,r.user_id,r.content,r.created_at,r.updated_at,r.deleted_at,
		       u.name,u.avatar_url
		FROM replies r
		LEFT JOIN users u ON u.id=r.user_id
		WHERE r.workspace_id=$1 AND r.doc_id=$2
		  AND ($3::timestamptz IS NULL OR r.updated_at>$3)
		ORDER BY r.updated_at ASC
		LIMIT $4`, workspaceID, docID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	changes := make([]Change, 0)
	for rows.Next() {
		var sid int64
		var id, commentID, workspaceID, docID, authorID string
		var content []byte
		var createdAt, updatedAt time.Time
		var deletedAt pgtype.Timestamptz
		var name, avatar pgtype.Text
		if err := rows.Scan(&sid, &id, &commentID, &workspaceID, &docID, &authorID, &content, &createdAt, &updatedAt, &deletedAt, &name, &avatar); err != nil {
			return nil, err
		}
		commentIDValue := commentID
		change := Change{ID: id, CommentID: &commentIDValue, UpdatedAt: updatedAt}
		if deletedAt.Valid {
			change.Action = "delete"
			change.Item = map[string]any{
				"deletedAt": deletedAt.Time.UTC().Format(time.RFC3339Nano),
				"updatedAt": updatedAt.UTC().Format(time.RFC3339Nano),
			}
		} else {
			var decoded map[string]any
			if err := json.Unmarshal(content, &decoded); err != nil {
				return nil, err
			}
			user := map[string]any{"id": authorID, "name": name.String}
			if avatar.Valid {
				user["avatarUrl"] = avatar.String
			}
			change.Action = "update"
			change.Item = map[string]any{
				"sid": sid, "id": id, "commentId": commentID,
				"workspaceId": workspaceID, "docId": docID, "content": decoded,
				"createdAt": createdAt.UTC().Format(time.RFC3339Nano),
				"updatedAt": updatedAt.UTC().Format(time.RFC3339Nano),
				"deletedAt": nil, "userId": authorID, "user": user,
			}
		}
		changes = append(changes, change)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return changes, nil
}

func nullableText(value pgtype.Text) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}
