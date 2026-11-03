// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package invitelinks

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

func TestRedisStoreCreateAndReuseAgainstRedis(t *testing.T) {
	redisURL := os.Getenv("NOTES_TEST_REDIS_URL")
	if redisURL == "" {
		t.Skip("set NOTES_TEST_REDIS_URL to a disposable local Redis instance")
	}
	parsed, err := url.Parse(redisURL)
	if err != nil {
		t.Fatalf("parse NOTES_TEST_REDIS_URL: %v", err)
	}
	if parsed.Hostname() != "localhost" && parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "::1" {
		t.Fatal("integration tests require a loopback Redis host")
	}
	options, err := redis.ParseURL(redisURL)
	if err != nil {
		t.Fatalf("parse Redis URL: %v", err)
	}
	client := redis.NewClient(options)
	t.Cleanup(func() { _ = client.Close() })
	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatalf("connect integration Redis: %v", err)
	}
	workspaceID := "invite-link-test-" + uuid.NewString()
	store := NewRedisStore(client, "https://notes.example.test")
	link, err := store.create(ctx, "user-1", workspaceID, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if link == nil || !link.Created || !strings.HasPrefix(link.URL, "https://notes.example.test/invite/") {
		t.Fatalf("created link = %#v", link)
	}
	id := strings.TrimPrefix(link.URL, "https://notes.example.test/invite/")
	defer client.Del(ctx, "workspace:inviteLink:"+workspaceID, "workspace:inviteLinkId:"+id)
	value, err := client.Get(ctx, "workspace:inviteLink:"+workspaceID).Result()
	if err != nil {
		t.Fatal(err)
	}
	var cached map[string]string
	if err := json.Unmarshal([]byte(value), &cached); err != nil || cached["inviteId"] != id {
		t.Fatalf("workspace cache = %#v, error = %v", cached, err)
	}
	index, err := client.Get(ctx, "workspace:inviteLinkId:"+id).Result()
	if err != nil {
		t.Fatal(err)
	}
	var indexValue map[string]string
	if err := json.Unmarshal([]byte(index), &indexValue); err != nil || indexValue["workspaceId"] != workspaceID || indexValue["inviterUserId"] != "user-1" {
		t.Fatalf("invite index = %#v, error = %v", indexValue, err)
	}
	remaining, err := client.PTTL(ctx, "workspace:inviteLink:"+workspaceID).Result()
	if err != nil || remaining <= 0 || remaining > 24*time.Hour {
		t.Fatalf("workspace key PTTL = %s, error = %v", remaining, err)
	}

	reused, err := store.create(ctx, "another-user", workspaceID, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if reused == nil || reused.Created || reused.URL != link.URL {
		t.Fatalf("reused link = %#v", reused)
	}
	remainingAfterReuse, err := client.PTTL(ctx, "workspace:inviteLink:"+workspaceID).Result()
	if err != nil || remainingAfterReuse > remaining || remainingAfterReuse < remaining-time.Second {
		t.Fatalf("reuse changed TTL: before=%s after=%s error=%v", remaining, remainingAfterReuse, err)
	}
	if _, err := store.Get(ctx, workspaceID); err != nil {
		t.Fatal(err)
	}
	if !store.Revoke(ctx, workspaceID) {
		t.Fatal("revoke did not delete workspace invite-link key")
	}
	if _, err := client.Get(ctx, "workspace:inviteLinkId:"+id).Result(); !errors.Is(err, redis.Nil) {
		t.Fatalf("invite index remains after revoke: %v", err)
	}
}
