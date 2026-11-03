// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package invitelinks

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/entitlements"
)

var (
	ErrPolicyDenied        = errors.New("invite-link policy denied")
	ErrWorkspaceNameDenied = errors.New("workspace name contains a URL or domain")
)

type Creator struct {
	store *RedisStore
	gate  *invitePolicyGate
}

func NewCreator(db Database, store *RedisStore, cfg config.Config, bootstrapKey string) *Creator {
	return &Creator{
		store: store,
		gate: &invitePolicyGate{
			db: db, config: cfg, bootstrapKey: bootstrapKey,
			deployment: entitlements.ParseDeployment(cfg.Deployment),
		},
	}
}

func (c *Creator) Create(ctx context.Context, actorID, workspaceID, expireTime string) (*Link, error) {
	if c == nil || c.gate == nil || c.store == nil {
		return nil, errors.New("invite-link creator is unavailable")
	}
	ttl, ok := inviteLinkTTL(expireTime)
	if !ok {
		return nil, errors.New("invalid invite-link expiration")
	}
	allowed, workspaceName, err := c.gate.Evaluate(ctx, actorID, workspaceID)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, ErrPolicyDenied
	}
	if containsURLOrDomain(workspaceName) {
		return nil, ErrWorkspaceNameDenied
	}
	return c.store.create(ctx, actorID, workspaceID, ttl)
}

func inviteLinkTTL(value string) (time.Duration, bool) {
	switch value {
	case "OneDay":
		return 24 * time.Hour, true
	case "ThreeDays":
		return 3 * 24 * time.Hour, true
	case "OneWeek":
		return 7 * 24 * time.Hour, true
	case "OneMonth":
		return 30 * 24 * time.Hour, true
	default:
		return 0, false
	}
}

const inviteAlphabet = "_-0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

func newInviteID() (string, error) {
	var raw [21]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("read random source: %w", err)
	}
	for index, value := range raw {
		raw[index] = inviteAlphabet[value&63]
	}
	return string(raw[:]), nil
}
