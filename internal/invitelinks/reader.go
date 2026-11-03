// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package invitelinks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const workspaceInviteLinkKeyPrefix = "workspace:inviteLink:"
const workspaceInviteLinkIDKeyPrefix = "workspace:inviteLinkId:"

var inviteIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

type redisReader interface {
	Get(context.Context, string) *redis.StringCmd
	TTL(context.Context, string) *redis.DurationCmd
	Del(context.Context, ...string) *redis.IntCmd
}

type redisWriter interface {
	Set(context.Context, string, any, time.Duration) *redis.StatusCmd
}

type Link struct {
	URL       string
	ExpiresAt time.Time
	Created   bool
}

type InvitationTarget struct {
	WorkspaceID string `json:"workspaceId"`
	InviterID   string `json:"inviterUserId"`
}

type RedisStore struct {
	redis  redisReader
	origin *url.URL
}

func NewRedisStore(redis redisReader, publicOrigin string) *RedisStore {
	origin, _ := url.Parse(strings.TrimRight(publicOrigin, "/"))
	if origin != nil && origin.Scheme != "" && origin.Host != "" {
		origin.Path = ""
		origin.RawPath = ""
		origin.RawQuery = ""
		origin.Fragment = ""
	} else {
		origin = nil
	}
	return &RedisStore{redis: redis, origin: origin}
}

func (r *RedisStore) Get(ctx context.Context, workspaceID string) (*Link, error) {
	if r == nil || r.redis == nil || r.origin == nil || workspaceID == "" {
		return nil, nil
	}
	key := workspaceInviteLinkKeyPrefix + workspaceID
	value, err := r.redis.Get(ctx, key).Result()
	if err != nil {
		return nil, nil
	}
	var cached struct {
		InviteID string `json:"inviteId"`
	}
	if json.Unmarshal([]byte(value), &cached) != nil || !inviteIDPattern.MatchString(cached.InviteID) {
		return nil, nil
	}
	ttl, err := r.redis.TTL(ctx, key).Result()
	if err != nil || ttl < time.Second {
		return nil, nil
	}

	return &Link{URL: r.inviteURL(cached.InviteID), ExpiresAt: time.Now().UTC().Add(ttl)}, nil
}

func (r *RedisStore) LookupByID(ctx context.Context, inviteID string) (*InvitationTarget, error) {
	if r == nil || r.redis == nil || !inviteIDPattern.MatchString(inviteID) {
		return nil, nil
	}
	value, err := r.redis.Get(ctx, workspaceInviteLinkIDKeyPrefix+inviteID).Result()
	if err != nil {
		return nil, nil
	}
	var target InvitationTarget
	if err := json.Unmarshal([]byte(value), &target); err != nil || target.WorkspaceID == "" {
		return nil, nil
	}
	return &target, nil
}

func (r *RedisStore) create(ctx context.Context, actorID, workspaceID string, ttl time.Duration) (*Link, error) {
	if r == nil || r.origin == nil || workspaceID == "" || ttl <= 0 {
		return nil, errors.New("invite-link store is unavailable")
	}
	key := workspaceInviteLinkKeyPrefix + workspaceID
	if value, err := r.redis.Get(ctx, key).Result(); err == nil {
		var cached struct {
			InviteID string `json:"inviteId"`
		}
		if json.Unmarshal([]byte(value), &cached) == nil && inviteIDPattern.MatchString(cached.InviteID) {
			if currentTTL, err := r.redis.TTL(ctx, key).Result(); err == nil && currentTTL > 0 {
				return &Link{URL: r.inviteURL(cached.InviteID), ExpiresAt: time.Now().UTC().Add(currentTTL)}, nil
			}
		}
	}
	writer, ok := r.redis.(redisWriter)
	if !ok {
		return nil, errors.New("invite-link store does not support writes")
	}
	inviteID, err := newInviteID()
	if err != nil {
		return nil, fmt.Errorf("generate invite-link id: %w", err)
	}
	workspaceValue, _ := json.Marshal(struct {
		InviteID string `json:"inviteId"`
	}{InviteID: inviteID})
	indexValue, _ := json.Marshal(struct {
		WorkspaceID string `json:"workspaceId"`
		InviterID   string `json:"inviterUserId"`
	}{WorkspaceID: workspaceID, InviterID: actorID})
	// The legacy cache API reports Redis write failures as false and the resolver
	// still returns the generated URL, so preserve that behavior here.
	_ = writer.Set(ctx, key, workspaceValue, ttl).Err()
	_ = writer.Set(ctx, workspaceInviteLinkIDKeyPrefix+inviteID, indexValue, ttl).Err()
	return &Link{URL: r.inviteURL(inviteID), ExpiresAt: time.Now().UTC().Add(ttl), Created: true}, nil
}

func (r *RedisStore) inviteURL(inviteID string) string {
	linkURL := *r.origin
	linkURL.Path = "/invite/" + inviteID
	linkURL.RawPath = ""
	return linkURL.String()
}

func (r *RedisStore) Revoke(ctx context.Context, workspaceID string) bool {
	if r == nil || r.redis == nil || workspaceID == "" {
		return false
	}
	key := workspaceInviteLinkKeyPrefix + workspaceID
	value, err := r.redis.Get(ctx, key).Result()
	var cached struct {
		InviteID string `json:"inviteId"`
	}
	if err == nil {
		_ = json.Unmarshal([]byte(value), &cached)
	}
	deleted, _ := r.redis.Del(ctx, key).Result()
	if cached.InviteID != "" {
		_, _ = r.redis.Del(ctx, workspaceInviteLinkIDKeyPrefix+cached.InviteID).Result()
	}
	return deleted > 0
}
