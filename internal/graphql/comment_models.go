// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/comments"
	"github.com/fydeos/fydeos-notes-server/internal/graphql/model"
)

type commentChangeCursor struct {
	CommentUpdatedAt *time.Time `json:"commentUpdatedAt,omitempty"`
	ReplyUpdatedAt   *time.Time `json:"replyUpdatedAt,omitempty"`
}

func decodeCommentCursor(value string, target any) error {
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(decoded, target)
}

func encodeCommentCursor(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(encoded), nil
}

func commentModel(item comments.Comment) *model.CommentObjectType {
	replies := make([]*model.ReplyObjectType, 0, len(item.Replies))
	for _, reply := range item.Replies {
		replies = append(replies, replyModel(reply))
	}
	return &model.CommentObjectType{
		ID: item.ID, Content: model.JSONObject(item.Content), Resolved: item.Resolved,
		CreatedAt: item.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt: item.UpdatedAt.UTC().Format(time.RFC3339Nano),
		User:      publicUserModel(item.User), Replies: replies,
	}
}

func replyModel(item comments.Reply) *model.ReplyObjectType {
	return &model.ReplyObjectType{
		CommentID: item.CommentID, ID: item.ID, Content: model.JSONObject(item.Content),
		CreatedAt: item.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt: item.UpdatedAt.UTC().Format(time.RFC3339Nano),
		User:      publicUserModel(item.User),
	}
}

func publicUserModel(user comments.User) *model.PublicUserType {
	return &model.PublicUserType{ID: user.ID, Name: user.Name, AvatarURL: user.AvatarURL}
}
