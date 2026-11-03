// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package realtime

import (
	"context"
	"errors"
	"time"
)

type NotificationCountRepository interface {
	CountUnread(context.Context, string) (int, error)
}

var errNotificationCountUnavailable = errors.New("notification count repository is unavailable")

func (s *Server) SetNotificationCountRepository(repository NotificationCountRepository) {
	s.notificationCountRepository = repository
}

func (s *Server) notificationCount(ctx context.Context, userID string) (int, error) {
	if s == nil || s.notificationCountRepository == nil {
		return 0, errNotificationCountUnavailable
	}
	return s.notificationCountRepository.CountUnread(ctx, userID)
}

func (s *Server) PublishNotificationCountChanged(userID string, count int, reason string) {
	if s == nil || userID == "" {
		return
	}
	s.broadcastRoom("user:"+userID+":notification", "realtime:event", map[string]any{
		"topic":    "notification.count.changed",
		"inputKey": "{}",
		"sentAt":   time.Now().UnixMilli(),
		"event":    map[string]any{"count": count, "reason": reason},
	}, "")
}
