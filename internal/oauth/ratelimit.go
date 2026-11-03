// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	rateLimit       = 20
	rateWindow      = 60 * time.Second
	maxTrackedKeys  = 10000
	overflowKey     = "\x00overflow"
	redisKeyPrefix  = "notes:oauth:preflight:strict:"
	purgeEveryCalls = 256
)

// RateLimiter decides whether a request identified by key may proceed.
type RateLimiter interface {
	Allow(context.Context, string, time.Time) (allowed bool, remaining int, reset time.Time)
}

type memoryWindow struct {
	start time.Time
	hits  int
}

// memoryLimiter is a fixed-window limiter bounded to maxTrackedKeys keys.
type memoryLimiter struct {
	mu      sync.Mutex
	windows map[string]*memoryWindow
	calls   int
}

func newMemoryLimiter() *memoryLimiter {
	return &memoryLimiter{windows: map[string]*memoryWindow{}}
}

func (l *memoryLimiter) Allow(_ context.Context, key string, now time.Time) (bool, int, time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if l.calls%purgeEveryCalls == 0 {
		for k, w := range l.windows {
			if !now.Before(w.start.Add(rateWindow)) {
				delete(l.windows, k)
			}
		}
	}
	w, ok := l.windows[key]
	if !ok {
		if len(l.windows) >= maxTrackedKeys {
			for k, old := range l.windows {
				if !now.Before(old.start.Add(rateWindow)) {
					delete(l.windows, k)
				}
			}
		}
		if len(l.windows) >= maxTrackedKeys {
			key = overflowKey
			w = l.windows[key]
		}
		if w == nil {
			w = &memoryWindow{start: now}
			l.windows[key] = w
		}
	}
	if !now.Before(w.start.Add(rateWindow)) {
		w.start, w.hits = now, 0
	}
	w.hits++
	return w.hits <= rateLimit, max(0, rateLimit-w.hits), w.start.Add(rateWindow)
}

// redisLimiter uses a Redis counter and falls back to memory on any error.
type redisLimiter struct {
	client   redis.UniversalClient
	fallback *memoryLimiter
}

func NewRedisStrictLimiter(client redis.UniversalClient) RateLimiter {
	return &redisLimiter{client: client, fallback: newMemoryLimiter()}
}

var redisIncrScript = redis.NewScript(`
local hits = redis.call('INCR', KEYS[1])
local ttl = redis.call('PTTL', KEYS[1])
if hits == 1 or ttl < 0 then
  redis.call('PEXPIRE', KEYS[1], ARGV[1])
  ttl = tonumber(ARGV[1])
end
return {hits, ttl}
`)

func (l *redisLimiter) Allow(ctx context.Context, identity string, now time.Time) (bool, int, time.Time) {
	if l.client == nil {
		return l.fallback.Allow(ctx, identity, now)
	}
	sum := sha256.Sum256([]byte(identity))
	key := redisKeyPrefix + hex.EncodeToString(sum[:])
	reply, err := redisIncrScript.Run(ctx, l.client, []string{key}, rateWindow.Milliseconds()).Int64Slice()
	if err != nil || len(reply) != 2 || reply[0] < 1 || reply[1] < 0 {
		return l.fallback.Allow(ctx, identity, now)
	}
	hits := int(reply[0])
	return hits <= rateLimit, max(0, rateLimit-hits), now.Add(time.Duration(reply[1]) * time.Millisecond)
}
