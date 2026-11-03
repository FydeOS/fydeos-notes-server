// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package comments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func (r *Repository) CreateComment(ctx context.Context, actorID, workspaceID, docID string, content map[string]any, notification NotificationInput) (Comment, error) {
	tx, err := r.beginWrite(ctx)
	if err != nil {
		return Comment{}, err
	}
	defer tx.Rollback(context.Background())
	if err := lockDocumentUpdate(ctx, tx, workspaceID, docID); err != nil {
		return Comment{}, err
	}
	access, err := r.authorize(ctx, tx, actorID, workspaceID, docID)
	if err != nil {
		return Comment{}, err
	}
	if !access.CommentsCreate {
		return Comment{}, &PermissionDeniedError{Action: "Doc.Comments.Create", WorkspaceID: workspaceID, DocID: docID, Cause: ErrCommentsCreateForbidden}
	}
	encoded, err := json.Marshal(content)
	if err != nil {
		return Comment{}, err
	}
	id := uuid.NewString()
	if _, err := tx.Exec(ctx, `INSERT INTO comments(id,workspace_id,doc_id,user_id,content,resolved)
		VALUES($1,$2,$3,$4,$5::jsonb,false)`, id, workspaceID, docID, actorID, encoded); err != nil {
		return Comment{}, err
	}
	item, err := r.commentByID(ctx, tx, id, false)
	if err != nil {
		return Comment{}, err
	}
	item.NotificationReceivers, err = r.createNotifications(ctx, tx, actorID, item.ID, item.WorkspaceID, item.DocID, nil, notification)
	if err != nil {
		return Comment{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Comment{}, err
	}
	item.Replies = []Reply{}
	return item, nil
}

func (r *Repository) UpdateComment(ctx context.Context, actorID, id string, content map[string]any) (Comment, error) {
	return r.updateComment(ctx, actorID, id, content, nil, false)
}

func (r *Repository) ResolveComment(ctx context.Context, actorID, id string, resolved bool) (Comment, error) {
	return r.updateComment(ctx, actorID, id, nil, &resolved, false)
}

func (r *Repository) DeleteComment(ctx context.Context, actorID, id string) (Comment, error) {
	return r.updateComment(ctx, actorID, id, nil, nil, true)
}

func (r *Repository) updateComment(ctx context.Context, actorID, id string, content map[string]any, resolved *bool, deleted bool) (Comment, error) {
	tx, err := r.beginWrite(ctx)
	if err != nil {
		return Comment{}, err
	}
	defer tx.Rollback(context.Background())
	item, err := r.commentByID(ctx, tx, id, true)
	if err != nil {
		return Comment{}, err
	}
	if err := r.authorizeModeration(ctx, tx, actorID, item.WorkspaceID, item.DocID, item.User.ID); err != nil {
		return Comment{}, err
	}
	updatedAt := time.Now().UTC().Truncate(time.Millisecond)
	switch {
	case deleted:
		_, err = tx.Exec(ctx, `UPDATE comments SET deleted_at=$2,updated_at=$2 WHERE id=$1 AND deleted_at IS NULL`, id, updatedAt)
	case resolved != nil:
		_, err = tx.Exec(ctx, `UPDATE comments SET resolved=$2,updated_at=$3 WHERE id=$1 AND deleted_at IS NULL`, id, *resolved, updatedAt)
		item.Resolved = *resolved
	default:
		var encoded []byte
		encoded, err = json.Marshal(content)
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE comments SET content=$2::jsonb,updated_at=$3 WHERE id=$1 AND deleted_at IS NULL`, id, encoded, updatedAt)
			item.Content = content
		}
	}
	if err != nil {
		return Comment{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Comment{}, err
	}
	item.UpdatedAt = updatedAt
	return item, nil
}

func (r *Repository) CreateReply(ctx context.Context, actorID, commentID string, content map[string]any, notification NotificationInput) (Reply, error) {
	tx, err := r.beginWrite(ctx)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(context.Background())
	comment, err := r.commentByID(ctx, tx, commentID, true)
	if err != nil {
		return Reply{}, err
	}
	access, err := r.authorize(ctx, tx, actorID, comment.WorkspaceID, comment.DocID)
	if err != nil {
		return Reply{}, err
	}
	if !access.CommentsCreate {
		return Reply{}, &PermissionDeniedError{Action: "Doc.Comments.Create", WorkspaceID: comment.WorkspaceID, DocID: comment.DocID, Cause: ErrCommentsCreateForbidden}
	}
	encoded, err := json.Marshal(content)
	if err != nil {
		return Reply{}, err
	}
	id := uuid.NewString()
	if _, err := tx.Exec(ctx, `INSERT INTO replies(id,comment_id,workspace_id,doc_id,user_id,content)
		VALUES($1,$2,$3,$4,$5,$6::jsonb)`, id, comment.ID, comment.WorkspaceID, comment.DocID, actorID, encoded); err != nil {
		return Reply{}, err
	}
	item, err := r.replyByID(ctx, tx, id, false)
	if err != nil {
		return Reply{}, err
	}
	item.NotificationReceivers, err = r.createNotifications(ctx, tx, actorID, comment.ID, comment.WorkspaceID, comment.DocID, &id, notification)
	if err != nil {
		return Reply{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Reply{}, err
	}
	return item.Reply, nil
}

func (r *Repository) createNotifications(ctx context.Context, tx pgx.Tx, senderID, commentID, workspaceID, docID string, replyID *string, input NotificationInput) ([]CreatedNotification, error) {
	users := make(map[string]struct{})
	rows, err := tx.Query(ctx, `SELECT principal_id FROM doc_grants
		WHERE workspace_id=$1 AND doc_id=$2 AND principal_type='user' AND role='owner'`, workspaceID, docID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var userID string
		if err := rows.Scan(&userID); err != nil {
			rows.Close()
			return nil, err
		}
		users[userID] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	if replyID != nil {
		var commentAuthor string
		if err := tx.QueryRow(ctx, `SELECT user_id FROM comments WHERE id=$1 AND deleted_at IS NULL`, commentID).Scan(&commentAuthor); err != nil {
			return nil, err
		}
		users[commentAuthor] = struct{}{}
		rows, err = tx.Query(ctx, `SELECT DISTINCT user_id FROM replies WHERE comment_id=$1 AND deleted_at IS NULL`, commentID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var userID string
			if err := rows.Scan(&userID); err != nil {
				rows.Close()
				return nil, err
			}
			users[userID] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	mentioned := make(map[string]struct{}, len(input.Mentions))
	for _, userID := range input.Mentions {
		mentioned[userID] = struct{}{}
		users[userID] = struct{}{}
	}
	delete(users, senderID)
	userIDs := make([]string, 0, len(users))
	for userID := range users {
		userIDs = append(userIDs, userID)
	}
	sort.Strings(userIDs)

	created := make([]CreatedNotification, 0, len(userIDs))
	for _, userID := range userIDs {
		access, err := r.authorize(ctx, tx, userID, workspaceID, docID)
		if err != nil {
			return nil, err
		}
		if !access.Read {
			continue
		}
		typeName := "Comment"
		if _, ok := mentioned[userID]; ok {
			typeName = "CommentMention"
		}
		body, err := json.Marshal(map[string]any{
			"workspaceId":     workspaceID,
			"createdByUserId": senderID,
			"commentId":       commentID,
			"replyId":         replyID,
			"doc": map[string]any{
				"id":    docID,
				"title": input.DocTitle,
				"mode":  input.DocMode,
			},
		})
		if err != nil {
			return nil, err
		}
		notificationID := uuid.NewString()
		if _, err := tx.Exec(ctx, `INSERT INTO notifications(id,user_id,level,read,type,body)
			VALUES($1,$2,'Default',false,$3::"NotificationType",$4::jsonb)`, notificationID, userID, typeName, body); err != nil {
			return nil, err
		}
		created = append(created, CreatedNotification{ID: notificationID, UserID: userID})
	}
	return created, nil
}

func (r *Repository) UpdateReply(ctx context.Context, actorID, id string, content map[string]any) (Reply, error) {
	return r.updateReply(ctx, actorID, id, content, false)
}

func (r *Repository) DeleteReply(ctx context.Context, actorID, id string) (Reply, error) {
	return r.updateReply(ctx, actorID, id, nil, true)
}

func (r *Repository) updateReply(ctx context.Context, actorID, id string, content map[string]any, deleted bool) (Reply, error) {
	tx, err := r.beginWrite(ctx)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(context.Background())
	item, err := r.replyByID(ctx, tx, id, true)
	if err != nil {
		return Reply{}, err
	}
	if err := r.authorizeModeration(ctx, tx, actorID, item.WorkspaceID, item.DocID, item.Reply.User.ID); err != nil {
		return Reply{}, err
	}
	updatedAt := time.Now().UTC().Truncate(time.Millisecond)
	if deleted {
		_, err = tx.Exec(ctx, `UPDATE replies SET deleted_at=$2,updated_at=$2 WHERE id=$1 AND deleted_at IS NULL`, id, updatedAt)
	} else {
		var encoded []byte
		encoded, err = json.Marshal(content)
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE replies SET content=$2::jsonb,updated_at=$3 WHERE id=$1 AND deleted_at IS NULL`, id, encoded, updatedAt)
			item.Content = content
		}
	}
	if err != nil {
		return Reply{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Reply{}, err
	}
	item.UpdatedAt = updatedAt
	return item.Reply, nil
}

func (r *Repository) beginWrite(ctx context.Context) (pgx.Tx, error) {
	if r == nil || r.beginner == nil || r.authorizer == nil {
		return nil, ErrWriteUnavailable
	}
	return r.beginner.Begin(ctx)
}

func (r *Repository) authorize(ctx context.Context, tx pgx.Tx, actorID, workspaceID, docID string) (access workspaces.DocumentAuthorization, err error) {
	return r.authorizer.AuthorizeDocumentInTx(ctx, tx, actorID, workspaceID, docID)
}

func lockDocumentUpdate(ctx context.Context, tx pgx.Tx, workspaceID, docID string) error {
	key := fmt.Sprintf("workspace-doc-update:%s/%s", workspaceID, docID)
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, key)
	return err
}

func (r *Repository) authorizeModeration(ctx context.Context, tx pgx.Tx, actorID, workspaceID, docID, authorID string) error {
	if actorID == authorID {
		return nil
	}
	access, err := r.authorize(ctx, tx, actorID, workspaceID, docID)
	if err != nil {
		return err
	}
	if !access.CommentsModerate {
		return &PermissionDeniedError{Action: "Doc.Comments.Moderate", WorkspaceID: workspaceID, DocID: docID, Cause: ErrCommentsModerateForbidden}
	}
	return nil
}

func (r *Repository) commentByID(ctx context.Context, tx pgx.Tx, id string, lock bool) (Comment, error) {
	query := `SELECT c.sid,c.id,c.workspace_id,c.doc_id,c.content,c.resolved,c.created_at,c.updated_at,c.user_id,u.name,u.avatar_url
		FROM comments c JOIN users u ON u.id=c.user_id WHERE c.id=$1 AND c.deleted_at IS NULL`
	if lock {
		query += ` FOR UPDATE OF c`
	}
	var item Comment
	var content []byte
	var avatar pgtype.Text
	err := tx.QueryRow(ctx, query, id).Scan(&item.SID, &item.ID, &item.WorkspaceID, &item.DocID, &content, &item.Resolved, &item.CreatedAt, &item.UpdatedAt, &item.User.ID, &item.User.Name, &avatar)
	if errors.Is(err, pgx.ErrNoRows) {
		return Comment{}, ErrNotFound
	}
	if err != nil {
		return Comment{}, err
	}
	if err := json.Unmarshal(content, &item.Content); err != nil {
		return Comment{}, err
	}
	item.User.AvatarURL = nullableText(avatar)
	item.Replies = []Reply{}
	return item, nil
}

type replyRecord struct {
	Reply
}

func (r *Repository) replyByID(ctx context.Context, tx pgx.Tx, id string, lock bool) (replyRecord, error) {
	query := `SELECT r.id,r.comment_id,r.workspace_id,r.doc_id,r.content,r.created_at,r.updated_at,r.user_id,u.name,u.avatar_url
		FROM replies r JOIN users u ON u.id=r.user_id WHERE r.id=$1 AND r.deleted_at IS NULL`
	if lock {
		query += ` FOR UPDATE OF r`
	}
	var item replyRecord
	var content []byte
	var avatar pgtype.Text
	err := tx.QueryRow(ctx, query, id).Scan(&item.ID, &item.CommentID, &item.WorkspaceID, &item.DocID, &content, &item.CreatedAt, &item.UpdatedAt, &item.User.ID, &item.User.Name, &avatar)
	if errors.Is(err, pgx.ErrNoRows) {
		return replyRecord{}, ErrReplyNotFound
	}
	if err != nil {
		return replyRecord{}, err
	}
	if err := json.Unmarshal(content, &item.Content); err != nil {
		return replyRecord{}, err
	}
	item.User.AvatarURL = nullableText(avatar)
	return item, nil
}
