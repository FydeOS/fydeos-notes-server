// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const realtimeFanoutChannel = "notes:backend-go:realtime:v1"
const realtimeFanoutReceiveTimeout = time.Minute

type realtimeFanoutMessage struct {
	Source                string `json:"source"`
	Kind                  string `json:"kind"`
	Room                  string `json:"room,omitempty"`
	Event                 string `json:"event"`
	Payload               any    `json:"payload,omitempty"`
	ExcludeSocketID       string `json:"excludeSocketId,omitempty"`
	WorkspaceID           string `json:"workspaceId,omitempty"`
	DocID                 string `json:"docId,omitempty"`
	AfterPermissionChange bool   `json:"afterPermissionChange,omitempty"`
}

type realtimeFanout struct {
	client  redis.UniversalClient
	pubsub  *redis.PubSub
	source  string
	handler func(realtimeFanoutMessage)
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
}

func newRealtimeFanout(client redis.UniversalClient, source string, handler func(realtimeFanoutMessage)) *realtimeFanout {
	ctx, cancel := context.WithCancel(context.Background())
	fanout := &realtimeFanout{
		client:  client,
		pubsub:  client.Subscribe(ctx, realtimeFanoutChannel),
		source:  source,
		handler: handler,
		ctx:     ctx,
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	go fanout.run()
	return fanout
}

func (f *realtimeFanout) Publish(message realtimeFanoutMessage) error {
	return f.PublishContext(context.Background(), message)
}

func (f *realtimeFanout) PublishContext(parent context.Context, message realtimeFanoutMessage) error {
	if f == nil || f.client == nil {
		return nil
	}
	message.Source = f.source
	if err := validateFanoutMessage(message); err != nil {
		return err
	}
	encoded, err := json.Marshal(message)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, time.Second)
	defer cancel()
	return f.client.Publish(ctx, realtimeFanoutChannel, encoded).Err()
}

func (f *realtimeFanout) Close() {
	if f == nil {
		return
	}
	f.cancel()
	_ = f.pubsub.Close()
	<-f.done
}

func (f *realtimeFanout) run() {
	defer close(f.done)
	delay := 100 * time.Millisecond
	for {
		received, err := f.pubsub.ReceiveTimeout(f.ctx, realtimeFanoutReceiveTimeout)
		if err != nil {
			if errors.Is(err, context.Canceled) || f.ctx.Err() != nil {
				return
			}
			if timeout, ok := err.(interface{ Timeout() bool }); ok && timeout.Timeout() {
				continue
			}
			if !errors.Is(err, redis.ErrClosed) {
				slog.Warn("realtime cluster subscription failed", "error", err)
			}
			select {
			case <-f.ctx.Done():
				return
			case <-time.After(delay):
			}
			if delay < 2*time.Second {
				delay *= 2
			}
			continue
		}
		delay = 100 * time.Millisecond
		message, ok := received.(*redis.Message)
		if !ok {
			continue
		}
		var payload realtimeFanoutMessage
		if err := json.Unmarshal([]byte(message.Payload), &payload); err != nil {
			slog.Warn("invalid realtime cluster message", "error", err)
			continue
		}
		if payload.Source == f.source || validateFanoutMessage(payload) != nil {
			continue
		}
		f.handler(payload)
	}
}

func validateFanoutMessage(message realtimeFanoutMessage) error {
	if message.Source == "" || message.Event == "" {
		return errors.New("realtime fanout message is missing a source or event")
	}
	switch message.Kind {
	case "room":
		if strings.TrimSpace(message.Room) == "" {
			return errors.New("realtime room fanout message is missing a room")
		}
	case "workspace-document":
		if message.WorkspaceID == "" || message.DocID == "" {
			return errors.New("realtime workspace fanout message is missing a document")
		}
	case "workspace-permissions":
		if message.WorkspaceID == "" {
			return errors.New("realtime workspace permission fanout message is missing a workspace")
		}
	default:
		return errors.New("realtime fanout message has an unknown kind")
	}
	return nil
}
