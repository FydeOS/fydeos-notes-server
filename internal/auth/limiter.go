// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/redis/go-redis/v9"
)

type SignInRateLimitPolicy struct {
	TTL        time.Duration
	IPLimit    int
	EmailLimit int
}

type PasswordLoginLimiter interface {
	AllowStrict(ctx context.Context, ip string, now time.Time) bool
	Allow(ctx context.Context, email, ip string, policy SignInRateLimitPolicy, now time.Time) bool
}

var defaultSignInPolicy = SignInRateLimitPolicy{TTL: time.Minute, IPLimit: 20, EmailLimit: 5}

const (
	strictIPLimit   = 20
	strictIPWindow  = time.Minute
	memoryMaxKeys   = 10000
	memorySweepTime = time.Minute
)

func loadSignInPolicy(ctx context.Context, db RowQuerier, cfg config.Config) (SignInRateLimitPolicy, error) {
	raw, found, err := config.ReadRuntimeValue(ctx, db, cfg.RuntimeConfigPath, "auth.signInRateLimit")
	if err != nil {
		return SignInRateLimitPolicy{}, err
	}
	if !found {
		return defaultSignInPolicy, nil
	}
	var stored struct {
		TTL        *float64 `json:"ttl"`
		IPLimit    *float64 `json:"ipLimit"`
		EmailLimit *float64 `json:"emailLimit"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&stored); err != nil {
		return SignInRateLimitPolicy{}, errors.New("invalid auth.signInRateLimit")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return SignInRateLimitPolicy{}, errors.New("invalid auth.signInRateLimit")
	}
	policy := defaultSignInPolicy
	check := func(v *float64, set func(int64)) error {
		if v == nil {
			return nil
		}
		if *v < 1 || *v > math.MaxInt32 || *v != math.Trunc(*v) {
			return errors.New("invalid auth.signInRateLimit")
		}
		set(int64(*v))
		return nil
	}
	if err := errors.Join(
		check(stored.TTL, func(v int64) { policy.TTL = time.Duration(v) * time.Millisecond }),
		check(stored.IPLimit, func(v int64) { policy.IPLimit = int(v) }),
		check(stored.EmailLimit, func(v int64) { policy.EmailLimit = int(v) }),
	); err != nil {
		return SignInRateLimitPolicy{}, err
	}
	return policy, nil
}

type signInLimiter struct {
	client redis.UniversalClient
	memory *memoryCounter
}

// NewRedisPasswordLoginLimiter counts in Redis, falling back to memory.
func NewRedisPasswordLoginLimiter(client redis.UniversalClient) PasswordLoginLimiter {
	return &signInLimiter{client: client, memory: newMemoryCounter()}
}

func newMemoryLimiter() PasswordLoginLimiter {
	return &signInLimiter{memory: newMemoryCounter()}
}

func limiterKey(scope, value string) string {
	sum := sha256.Sum256([]byte(value))
	return "auth:sign-in-rate:" + scope + ":" + hex.EncodeToString(sum[:])
}

var incrScript = redis.NewScript(`
local n = redis.call('INCR', KEYS[1])
if n == 1 or redis.call('PTTL', KEYS[1]) < 0 then
  redis.call('PEXPIRE', KEYS[1], ARGV[1])
end
return n`)

func (l *signInLimiter) hit(ctx context.Context, scope, value string, ttl time.Duration, now time.Time) int64 {
	key := limiterKey(scope, value)
	if l.client != nil {
		n, err := incrScript.Run(ctx, l.client, []string{key}, ttl.Milliseconds()).Int64()
		if err == nil {
			return n
		}
	}
	return l.memory.hit(scope, key, ttl, now)
}

func (l *signInLimiter) AllowStrict(ctx context.Context, ip string, now time.Time) bool {
	if ip == "" {
		return true
	}
	return l.hit(ctx, "strict-ip", ip, strictIPWindow, now) <= strictIPLimit
}

func (l *signInLimiter) Allow(ctx context.Context, email, ip string, policy SignInRateLimitPolicy, now time.Time) bool {
	allowed := l.hit(ctx, "email", strings.ToLower(email), policy.TTL, now) <= int64(policy.EmailLimit)
	if ip != "" {
		if l.hit(ctx, "ip", ip, policy.TTL, now) > int64(policy.IPLimit) {
			allowed = false
		}
	}
	return allowed
}

type memoryEntry struct {
	count   int64
	expires time.Time
}

type memoryCounter struct {
	mu        sync.Mutex
	entries   map[string]*memoryEntry
	lastSweep time.Time
}

func newMemoryCounter() *memoryCounter {
	return &memoryCounter{entries: map[string]*memoryEntry{}}
}

func (m *memoryCounter) hit(scope, key string, ttl time.Duration, now time.Time) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if now.Sub(m.lastSweep) >= memorySweepTime {
		for k, e := range m.entries {
			if !now.Before(e.expires) {
				delete(m.entries, k)
			}
		}
		m.lastSweep = now
	}
	e, ok := m.entries[key]
	if ok && !now.Before(e.expires) {
		delete(m.entries, key)
		ok = false
	}
	if !ok {
		if len(m.entries) >= memoryMaxKeys {
			key = "overflow:" + scope
			if e, ok = m.entries[key]; ok && !now.Before(e.expires) {
				ok = false
			}
		}
		if !ok {
			e = &memoryEntry{expires: now.Add(ttl)}
			m.entries[key] = e
		}
	}
	e.count++
	return e.count
}
