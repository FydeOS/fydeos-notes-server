// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package invitelinks

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

type fakeRedis struct {
	redis.UniversalClient
	value  string
	ttl    time.Duration
	getErr error
	ttlErr error
	setErr error
	keys   []string
	delErr error
	del    int64
	dels   [][]string
	sets   []struct {
		key   string
		value string
		ttl   time.Duration
	}
}

func (r *fakeRedis) Get(ctx context.Context, key string) *redis.StringCmd {
	r.keys = append(r.keys, key)
	command := redis.NewStringCmd(ctx, "GET", key)
	command.SetVal(r.value)
	command.SetErr(r.getErr)
	return command
}

func (r *fakeRedis) TTL(ctx context.Context, key string) *redis.DurationCmd {
	r.keys = append(r.keys, key)
	command := redis.NewDurationCmd(ctx, time.Second, "TTL", key)
	command.SetVal(r.ttl)
	command.SetErr(r.ttlErr)
	return command
}

func (r *fakeRedis) Del(ctx context.Context, keys ...string) *redis.IntCmd {
	r.dels = append(r.dels, append([]string(nil), keys...))
	command := redis.NewIntCmd(ctx, "DEL")
	command.SetVal(r.del)
	command.SetErr(r.delErr)
	return command
}

func (r *fakeRedis) Set(ctx context.Context, key string, value any, ttl time.Duration) *redis.StatusCmd {
	var raw string
	switch value := value.(type) {
	case string:
		raw = value
	case []byte:
		raw = string(value)
	}
	r.sets = append(r.sets, struct {
		key   string
		value string
		ttl   time.Duration
	}{key: key, value: raw, ttl: ttl})
	command := redis.NewStatusCmd(ctx, "SET", key, raw, ttl)
	command.SetVal("OK")
	command.SetErr(r.setErr)
	return command
}

func TestReaderUsesLegacyRedisKeyAndLinkFormat(t *testing.T) {
	client := &fakeRedis{value: `{"inviteId":"Abc_123-xyz"}`, ttl: 90 * time.Minute}
	reader := NewRedisStore(client, "https://notes.example.test/tenant/path")

	link, err := reader.Get(context.Background(), "workspace-7")
	if err != nil {
		t.Fatal(err)
	}
	if link == nil || link.URL != "https://notes.example.test/invite/Abc_123-xyz" {
		t.Fatalf("link = %#v", link)
	}
	if delta := time.Until(link.ExpiresAt); delta < 89*time.Minute || delta > 90*time.Minute {
		t.Fatalf("expiry delta = %s, want around 90m", delta)
	}
	if len(client.keys) != 2 || client.keys[0] != "workspace:inviteLink:workspace-7" || client.keys[1] != client.keys[0] {
		t.Fatalf("Redis keys = %#v", client.keys)
	}
}

func TestReaderTreatsInvalidOrExpiredCacheAsMissing(t *testing.T) {
	for _, test := range []struct {
		name   string
		value  string
		ttl    time.Duration
		getErr error
		ttlErr error
	}{
		{name: "missing value", value: ""},
		{name: "malformed json", value: "{"},
		{name: "invalid id", value: `{"inviteId":"../../admin"}`, ttl: time.Hour},
		{name: "no expiry", value: `{"inviteId":"valid-id"}`, ttl: -1},
		{name: "subsecond expiry", value: `{"inviteId":"valid-id"}`, ttl: 500 * time.Millisecond},
		{name: "redis read failure", value: `{"inviteId":"valid-id"}`, getErr: errors.New("redis down")},
		{name: "redis ttl failure", value: `{"inviteId":"valid-id"}`, ttl: time.Hour, ttlErr: errors.New("redis down")},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := NewRedisStore(&fakeRedis{value: test.value, ttl: test.ttl, getErr: test.getErr, ttlErr: test.ttlErr}, "https://notes.example.test")
			link, err := reader.Get(context.Background(), "workspace-1")
			if err != nil || link != nil {
				t.Fatalf("Get() = (%#v, %v), want (nil, nil)", link, err)
			}
		})
	}
}

func TestLookupByIDReadsLegacyInvitationIndex(t *testing.T) {
	client := &fakeRedis{value: `{"workspaceId":"workspace-7","inviterUserId":"user-9"}`}
	store := NewRedisStore(client, "https://notes.example.test")
	target, err := store.LookupByID(context.Background(), "Abc_123-xyz")
	if err != nil || target == nil || target.WorkspaceID != "workspace-7" || target.InviterID != "user-9" {
		t.Fatalf("LookupByID() = %#v, %v", target, err)
	}
	if len(client.keys) != 1 || client.keys[0] != "workspace:inviteLinkId:Abc_123-xyz" {
		t.Fatalf("Redis keys = %#v", client.keys)
	}
}

func TestLookupByIDTreatsInvalidIndexAsMissing(t *testing.T) {
	for _, test := range []struct {
		name   string
		id     string
		value  string
		getErr error
	}{
		{name: "bad id", id: "../workspace", value: `{"workspaceId":"workspace-7"}`},
		{name: "bad json", id: "valid-id", value: "{"},
		{name: "missing workspace", id: "valid-id", value: `{"inviterUserId":"user-9"}`},
		{name: "redis unavailable", id: "valid-id", value: `{"workspaceId":"workspace-7"}`, getErr: errors.New("redis down")},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := NewRedisStore(&fakeRedis{value: test.value, getErr: test.getErr}, "https://notes.example.test")
			target, err := store.LookupByID(context.Background(), test.id)
			if err != nil || target != nil {
				t.Fatalf("LookupByID() = %#v, %v, want missing", target, err)
			}
		})
	}
}

func TestRedisStoreRevokeDeletesLegacyKeys(t *testing.T) {
	client := &fakeRedis{value: `{"inviteId":"Abc_123-xyz"}`, del: 1}
	store := NewRedisStore(client, "https://notes.example.test")

	if !store.Revoke(context.Background(), "workspace-7") {
		t.Fatal("Revoke() = false, want true for an existing workspace key")
	}
	want := [][]string{{"workspace:inviteLink:workspace-7"}, {"workspace:inviteLinkId:Abc_123-xyz"}}
	if len(client.dels) != len(want) {
		t.Fatalf("DEL calls = %#v", client.dels)
	}
	for i := range want {
		if len(client.dels[i]) != 1 || client.dels[i][0] != want[i][0] {
			t.Fatalf("DEL call %d = %#v, want %#v", i, client.dels[i], want[i])
		}
	}
}

func TestRedisStoreRevokeReturnsFalseWhenPrimaryKeyIsMissing(t *testing.T) {
	client := &fakeRedis{del: 0}
	store := NewRedisStore(client, "https://notes.example.test")
	if store.Revoke(context.Background(), "workspace-7") {
		t.Fatal("Revoke() = true, want false for a missing key")
	}
	if len(client.dels) != 1 || client.dels[0][0] != "workspace:inviteLink:workspace-7" {
		t.Fatalf("missing-key DEL calls = %#v", client.dels)
	}
}

func TestRedisStoreCreateWritesLegacyKeysAndPayloads(t *testing.T) {
	client := &fakeRedis{}
	store := NewRedisStore(client, "https://notes.example.test")
	link, err := store.create(context.Background(), "user-9", "workspace-7", 3*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	const invitePrefix = "https://notes.example.test/invite/"
	if link == nil || !link.Created || !strings.HasPrefix(link.URL, invitePrefix) {
		t.Fatalf("created link = %#v", link)
	}
	id := link.URL[len(invitePrefix):]
	if !inviteIDPattern.MatchString(id) || len(id) != 21 {
		t.Fatalf("invite ID = %q", id)
	}
	if len(client.sets) != 2 || client.sets[0].key != "workspace:inviteLink:workspace-7" || client.sets[1].key != "workspace:inviteLinkId:"+id {
		t.Fatalf("SET calls = %#v", client.sets)
	}
	for _, set := range client.sets {
		if set.ttl != 3*24*time.Hour {
			t.Fatalf("TTL for %s = %s", set.key, set.ttl)
		}
	}
	var workspacePayload map[string]string
	if err := json.Unmarshal([]byte(client.sets[0].value), &workspacePayload); err != nil || workspacePayload["inviteId"] != id {
		t.Fatalf("workspace payload = %#v, error = %v", workspacePayload, err)
	}
	var indexPayload map[string]string
	if err := json.Unmarshal([]byte(client.sets[1].value), &indexPayload); err != nil || indexPayload["workspaceId"] != "workspace-7" || indexPayload["inviterUserId"] != "user-9" {
		t.Fatalf("index payload = %#v, error = %v", indexPayload, err)
	}
	if delta := time.Until(link.ExpiresAt); delta < 71*time.Hour || delta > 72*time.Hour {
		t.Fatalf("expiry delta = %s", delta)
	}
}

func TestRedisStoreCreateReusesUnexpiredLegacyLink(t *testing.T) {
	client := &fakeRedis{value: `{"inviteId":"existing_123"}`, ttl: 45 * time.Minute}
	store := NewRedisStore(client, "https://notes.example.test")
	link, err := store.create(context.Background(), "user-9", "workspace-7", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if link == nil || link.Created || link.URL != "https://notes.example.test/invite/existing_123" {
		t.Fatalf("reused link = %#v", link)
	}
	if len(client.sets) != 0 {
		t.Fatalf("reused link unexpectedly wrote Redis keys: %#v", client.sets)
	}
}
