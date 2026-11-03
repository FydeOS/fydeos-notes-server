// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"log/slog"
	"strings"
	"unicode/utf16"

	"github.com/fydeos/fydeos-notes-server/internal/graphql/model"
)

func normalizeMentionInput(input model.MentionInput) (string, string, map[string]any, error) {
	if input.Doc == nil {
		return "", "", nil, ErrInvalidMentionInput
	}
	userID, err := normalizeMentionID(input.UserID)
	if err != nil {
		return "", "", nil, err
	}
	workspaceID, err := normalizeMentionID(input.WorkspaceID)
	if err != nil {
		return "", "", nil, err
	}
	docID, err := normalizeMentionID(input.Doc.ID)
	if err != nil {
		return "", "", nil, err
	}
	title := strings.TrimSpace(input.Doc.Title)
	if utf16Length(title) > 255 || !input.Doc.Mode.IsValid() {
		return "", "", nil, ErrInvalidMentionInput
	}
	doc := map[string]any{"id": docID, "title": title, "mode": string(input.Doc.Mode)}
	if input.Doc.BlockID != nil {
		blockID, err := normalizeMentionID(*input.Doc.BlockID)
		if err != nil {
			return "", "", nil, err
		}
		doc["blockId"] = blockID
	}
	if input.Doc.ElementID != nil {
		elementID, err := normalizeMentionID(*input.Doc.ElementID)
		if err != nil {
			return "", "", nil, err
		}
		doc["elementId"] = elementID
	}
	return userID, workspaceID, doc, nil
}

func normalizeMentionID(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || utf16Length(value) > 100 {
		return "", ErrInvalidMentionInput
	}
	return value, nil
}

func utf16Length(value string) int {
	return len(utf16.Encode([]rune(value)))
}

func (r *mutationResolver) publishCommentChanged(workspaceID, docID string) {
	if r.CommentPublisher != nil {
		r.CommentPublisher.PublishCommentChanged(workspaceID, docID)
	}
}

func (r *mutationResolver) publishNotificationCountChanged(ctx context.Context, userID, reason string) {
	if r.Notifications == nil || r.NotificationPublisher == nil {
		return
	}
	count, err := r.Notifications.CountUnread(ctx, userID)
	if err != nil {
		slog.WarnContext(ctx, "could not count unread notifications after mutation", "user_id", userID)
		return
	}
	r.NotificationPublisher.PublishNotificationCountChanged(userID, count, reason)
}
