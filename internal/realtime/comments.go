// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package realtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/comments"
)

type CommentChangeRepository interface {
	ListChanges(context.Context, string, string, *time.Time, *time.Time, int) ([]comments.Change, error)
}

var errCommentReadDenied = errors.New("comment document read access is denied")
var errInvalidCommentChangesRequest = errors.New("invalid comment changes request")

type commentChangeCursor struct {
	CommentUpdatedAt *time.Time `json:"commentUpdatedAt,omitempty"`
	ReplyUpdatedAt   *time.Time `json:"replyUpdatedAt,omitempty"`
}

func (s *Server) SetCommentDependencies(repository CommentChangeRepository, authorizer WorkspaceAuthorizer) {
	s.commentReader = repository
	s.commentAuthorizer = authorizer
}

func (s *Server) PublishCommentChanged(workspaceID, docID string) {
	if s == nil || workspaceID == "" || docID == "" {
		return
	}
	input := map[string]string{"workspaceId": workspaceID, "docId": docID}
	inputKey, err := json.Marshal(input)
	if err != nil {
		return
	}
	s.broadcastRoom(commentRealtimeRoom(workspaceID, docID), "realtime:event", map[string]any{
		"topic":    "comment.changed",
		"inputKey": string(inputKey),
		"sentAt":   time.Now().UnixMilli(),
		"event":    map[string]any{"changed": true},
	}, "")
}

func commentRealtimeRoom(workspaceID, docID string) string {
	return "workspace:" + workspaceID + ":doc:" + docID + ":comment"
}

func (s *Server) authorizeCommentRead(ctx context.Context, userID, workspaceID, docID string) error {
	if s == nil || s.commentAuthorizer == nil {
		return errors.New("comment realtime authorization is unavailable")
	}
	access, err := s.commentAuthorizer.AuthorizeWorkspaceSync(ctx, userID, workspaceID, []string{docID})
	if err != nil {
		return err
	}
	if !access.Documents[docID].Read {
		return errCommentReadDenied
	}
	return nil
}

func (s *Server) commentChanges(ctx context.Context, userID string, input map[string]any) (map[string]any, error) {
	workspaceID := stringField(input, "workspaceId")
	docID := stringField(input, "docId")
	if workspaceID == "" || docID == "" {
		return nil, errInvalidCommentChangesRequest
	}
	if err := s.authorizeCommentRead(ctx, userID, workspaceID, docID); err != nil {
		return nil, err
	}
	if s.commentReader == nil {
		return nil, errors.New("comment change repository is unavailable")
	}
	var cursor commentChangeCursor
	afterValue, hasCursor := input["after"]
	encoded := ""
	if hasCursor {
		var valid bool
		encoded, valid = afterValue.(string)
		if !valid {
			return nil, errInvalidCommentChangesRequest
		}
	}
	if encoded != "" {
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || json.Unmarshal(decoded, &cursor) != nil {
			return nil, errInvalidCommentChangesRequest
		}
	}
	first, hasLimit := integerField(input, "first")
	if _, exists := input["first"]; exists && !hasLimit {
		return nil, errInvalidCommentChangesRequest
	}
	paginate := hasLimit && first != 0
	if !paginate {
		first = 10000
	}
	if first < 0 {
		return nil, errInvalidCommentChangesRequest
	}
	changes, err := s.commentReader.ListChanges(ctx, workspaceID, docID, cursor.CommentUpdatedAt, cursor.ReplyUpdatedAt, first+boolInt(paginate))
	if err != nil {
		return nil, err
	}
	hasNextPage := paginate && len(changes) > first
	if hasNextPage {
		changes = changes[:first]
	}
	result := make([]map[string]any, 0, len(changes))
	for _, change := range changes {
		result = append(result, map[string]any{
			"id": change.ID, "action": change.Action, "item": change.Item, "commentId": change.CommentID,
		})
		updatedAt := change.UpdatedAt
		if change.CommentID != nil {
			cursor.ReplyUpdatedAt = &updatedAt
		} else {
			cursor.CommentUpdatedAt = &updatedAt
		}
	}
	cursorJSON, err := json.Marshal(cursor)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"changes": result, "startCursor": "", "endCursor": base64.StdEncoding.EncodeToString(cursorJSON), "hasNextPage": hasNextPage,
	}, nil
}

func integerField(input map[string]any, name string) (int, bool) {
	value, ok := input[name]
	if !ok || value == nil {
		return 0, false
	}
	switch number := value.(type) {
	case float64:
		return int(number), number == float64(int(number))
	case int:
		return number, true
	case int64:
		return int(number), int64(int(number)) == number
	default:
		return 0, false
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func commentSubscriptionID(socketID string, input map[string]any) (string, error) {
	canonical := make(map[string]any, len(input))
	for key, value := range input {
		if strings.TrimSpace(key) != "" {
			canonical[key] = value
		}
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("encode comment subscription input: %w", err)
	}
	return socketID + ":comment.changed:" + string(encoded), nil
}
