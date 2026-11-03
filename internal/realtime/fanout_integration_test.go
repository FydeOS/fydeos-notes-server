// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package realtime

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
	"github.com/redis/go-redis/v9"
)

func TestRedisRealtimeFanoutAcrossInstances(t *testing.T) {
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
		t.Fatalf("parse Redis connection URL: %v", err)
	}
	redisA := redis.NewClient(options)
	defer redisA.Close()
	optionsB := *options
	redisB := redis.NewClient(&optionsB)
	defer redisB.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := redisA.Ping(ctx).Err(); err != nil {
		t.Fatalf("connect integration Redis: %v", err)
	}

	workspaceAccess := workspaces.WorkspaceSyncAccess{
		CanSync:    true,
		Generation: 4,
		Documents: map[string]workspaces.DocumentSyncAccess{
			"doc-1": {Read: true, Update: true, Manage: true},
		},
	}
	workspaceDocsA := &testWorkspaceDocumentSync{}
	workspaceDocsA.generation.Store(workspaceAccess.Generation)
	workspaceDocsB := &testWorkspaceDocumentSync{}
	workspaceDocsB.generation.Store(workspaceAccess.Generation)
	authorization := &sharedWorkspaceAuthorization{access: workspaceAccess}
	authorizerA := &countingWorkspaceAuthorizer{shared: authorization}
	authorizerB := &countingWorkspaceAuthorizer{shared: authorization}
	workspaceA := WorkspaceDependencies{
		Authorizer: authorizerA, Documents: workspaceDocsA, UserUpdates: userUpdateReaderFake{},
		GrantsAuth: docGrantAuthorizerFake{usersRead: true}, ShareState: shareStateReaderFake{},
		QuotaAccess: workspaceAccessProviderFake{}, Quota: &quotaStateReaderFake{},
	}
	workspaceB := WorkspaceDependencies{
		Authorizer: authorizerB, Documents: workspaceDocsB, UserUpdates: userUpdateReaderFake{},
		GrantsAuth: docGrantAuthorizerFake{usersRead: true}, ShareState: shareStateReaderFake{},
		QuotaAccess: workspaceAccessProviderFake{}, Quota: &quotaStateReaderFake{},
	}

	backendA, err := NewWithRedis(config.Config{NodeEnv: "test", Host: "localhost"}, testAuthenticator{}, testDocumentSync{}, redisA, workspaceA)
	if err != nil {
		t.Fatal(err)
	}
	defer backendA.Close()
	backendB, err := NewWithRedis(config.Config{NodeEnv: "test", Host: "localhost"}, testAuthenticator{}, testDocumentSync{}, redisB, workspaceB)
	if err != nil {
		t.Fatal(err)
	}
	defer backendB.Close()
	waitForRealtimeFanoutSubscribers(t, redisA, 2)

	serverA := newRealtimeHTTPTestServer(t, backendA)
	defer serverA.Close()
	serverB := newRealtimeHTTPTestServer(t, backendB)
	defer serverB.Close()
	clientA := newPollingClient(t, serverA.URL, "valid-session")
	clientB := newPollingClient(t, serverB.URL, "valid-session")
	if !strings.Contains(clientA.connect(), "40{") || !strings.Contains(clientB.connect(), "40{") {
		t.Fatal("expected both authenticated Socket.IO namespace connections")
	}

	for i, client := range []pollingTestClient{clientA, clientB} {
		ack := client.emitAck(t, 1, "realtime:subscribe", map[string]any{
			"topic": "user.profile.changed", "input": map[string]any{}, "clientVersion": "0.26.0",
		})
		if !strings.Contains(ack, `"data":{"subscriptionId":`) {
			t.Fatalf("profile subscription %d acknowledgement = %q", i, ack)
		}
	}
	backendA.PublishUserProfileChanged("user-1")
	assertPacketContains(t, clientA.poll(), `"topic":"user.profile.changed"`)
	assertPacketContains(t, clientB.poll(), `"topic":"user.profile.changed"`)

	for i, client := range []pollingTestClient{clientA, clientB} {
		ack := client.emitAck(t, 2, "space:join-batch", map[string]any{
			"spaces": []any{map[string]any{"spaceType": "userspace", "spaceId": "user-1", "docId": "doc-1"}}, "clientVersion": "0.27.5",
		})
		if !strings.Contains(ack, `"success":true`) {
			t.Fatalf("userspace join %d acknowledgement = %q", i, ack)
		}
	}
	userPush := clientA.emitAck(t, 3, "space:push-doc-update", map[string]any{
		"spaceType": "userspace", "spaceId": "user-1", "docId": "doc-1", "update": "AQ==",
	})
	if !strings.Contains(userPush, `"accepted":true`) {
		t.Fatalf("userspace push acknowledgement = %q", userPush)
	}
	pollUntilContainsAll(t, clientB, "space:broadcast-doc-invalidation", "space:broadcast-doc-updates")

	for i, client := range []pollingTestClient{clientA, clientB} {
		ack := client.emitAck(t, 4, "space:join-batch", map[string]any{
			"spaces": []any{map[string]any{"spaceType": "workspace", "spaceId": "workspace-1", "docId": "doc-1"}}, "clientVersion": "0.27.5",
		})
		if !strings.Contains(ack, `"success":true`) {
			t.Fatalf("workspace join %d acknowledgement = %q", i, ack)
		}
	}
	collectAck := clientA.emitAck(t, 5, "space:load-awarenesses", map[string]any{
		"spaceType": "workspace", "spaceId": "workspace-1", "docId": "doc-1",
	})
	assertPacketContains(t, collectAck, `"clientId":`)
	collectPacket := clientB.poll()
	assertPacketContains(t, collectPacket, "space:collect-awareness")
	awarenessPayload, err := json.Marshal([]any{"space:update-awareness", map[string]any{
		"spaceType": "workspace", "spaceId": "workspace-1", "docId": "doc-1", "awarenessUpdate": "AQID",
	}})
	if err != nil {
		t.Fatal(err)
	}
	clientB.post("42" + string(awarenessPayload))
	awarenessPacket := clientA.poll()
	assertPacketContains(t, awarenessPacket, "space:broadcast-awareness-update")
	assertPacketContains(t, awarenessPacket, `"awarenessUpdate":"AQID"`)

	workspacePush := clientA.emitAck(t, 6, "space:push-doc-update", map[string]any{
		"spaceType": "workspace", "spaceId": "workspace-1", "docId": "doc-1", "update": "AQ==",
	})
	if !strings.Contains(workspacePush, `"accepted":true`) {
		t.Fatalf("workspace push acknowledgement = %q", workspacePush)
	}
	pollUntilContainsAll(t, clientB, "space:broadcast-doc-invalidation", "space:broadcast-doc-updates")

	for _, client := range []pollingTestClient{clientA, clientB} {
		subscribeRealtimeTopic(t, client, 10, "doc.grants.changed", map[string]any{"workspaceId": "workspace-1", "docId": "doc-1"})
		subscribeRealtimeTopic(t, client, 11, "doc.share-state.changed", map[string]any{"workspaceId": "workspace-1", "docId": "doc-1"})
		subscribeRealtimeTopic(t, client, 12, "workspace.quota-state.changed", map[string]any{"workspaceId": "workspace-1"})
	}
	for _, reason := range []string{"grants-changed", "owner-changed", "default-role-changed"} {
		backendA.PublishDocumentRoleChanged("workspace-1", "doc-1", reason)
		topic := "doc.grants.changed"
		if reason == "default-role-changed" {
			topic = "doc.share-state.changed"
		}
		for _, client := range []pollingTestClient{clientA, clientB} {
			assertRealtimeEvent(t, client.poll(), `"topic":"`+topic+`"`, `"reason":"`+reason+`"`)
		}
	}
	backendA.PublishDocumentPublicStateChanged("workspace-1", "doc-1")
	for _, client := range []pollingTestClient{clientA, clientB} {
		assertRealtimeEvent(t, client.poll(), `"topic":"doc.share-state.changed"`, `"reason":"public-state-changed"`)
	}

	configCallsA, configCallsB := authorizerA.calls.Load(), authorizerB.calls.Load()
	backendA.PublishWorkspaceConfigUpdated("workspace-1")
	waitForWorkspaceAuthorizationCall(t, authorizerA, configCallsA)
	waitForWorkspaceAuthorizationCall(t, authorizerB, configCallsB)

	authorizerCallsBeforeRevoke := authorizerB.calls.Load()
	authorization.set(workspaces.WorkspaceSyncAccess{CanSync: false, Generation: 5, Documents: map[string]workspaces.DocumentSyncAccess{}})
	workspaceDocsA.generation.Store(5)
	workspaceDocsB.generation.Store(5)
	backendA.PublishDocumentRoleChanged("workspace-1", "doc-1", "grants-changed")
	waitForWorkspaceAuthorizationCall(t, authorizerB, authorizerCallsBeforeRevoke)
	assertPacketContains(t, clientB.poll(), `"reason":"grants-changed"`)
	backendA.PublishWorkspaceMembersUpdated("workspace-1")
	for _, client := range []pollingTestClient{clientA, clientB} {
		assertRealtimeEvent(t, client.poll(), `"topic":"workspace.quota-state.changed"`, `"reason":"runtime-invalidation"`)
	}
	revokedPush := clientB.emitAck(t, 7, "space:push-doc-update", map[string]any{
		"spaceType": "workspace", "spaceId": "workspace-1", "docId": "doc-1", "update": "AQ==",
	})
	if !strings.Contains(revokedPush, `"name":"NOT_IN_SPACE"`) {
		t.Fatalf("cross-instance workspace push after permission revocation = %q", revokedPush)
	}
}

type sharedWorkspaceAuthorization struct {
	mu     sync.RWMutex
	access workspaces.WorkspaceSyncAccess
}

func (a *sharedWorkspaceAuthorization) set(access workspaces.WorkspaceSyncAccess) {
	a.mu.Lock()
	a.access = access
	a.mu.Unlock()
}

type countingWorkspaceAuthorizer struct {
	shared *sharedWorkspaceAuthorization
	calls  atomic.Int32
}

func (a *countingWorkspaceAuthorizer) AuthorizeWorkspaceSync(context.Context, string, string, []string) (workspaces.WorkspaceSyncAccess, error) {
	a.calls.Add(1)
	a.shared.mu.RLock()
	defer a.shared.mu.RUnlock()
	return a.shared.access, nil
}

func waitForWorkspaceAuthorizationCall(t *testing.T, authorizer *countingWorkspaceAuthorizer, previous int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if authorizer.calls.Load() > previous {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("workspace permission fanout did not revalidate the remote instance")
}

func TestDurableUserUpdateFanoutAcrossInstances(t *testing.T) {
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
		t.Fatalf("parse Redis connection URL: %v", err)
	}
	redisA := redis.NewClient(options)
	defer redisA.Close()
	optionsB := *options
	redisB := redis.NewClient(&optionsB)
	defer redisB.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := redisA.Ping(ctx).Err(); err != nil {
		t.Fatalf("connect integration Redis: %v", err)
	}

	pool := emptyRealtimeTestPool(t)
	if err := applyUserRefreshFixture(ctx, pool); err != nil {
		t.Fatalf("apply Go backend migrations: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,name,email) VALUES ('user-1','Initial','user-1@example.test')`); err != nil {
		t.Fatal(err)
	}

	reader := userUpdateReaderFake{
		workspaceIDs: []string{"workspace-1"},
		grants:       []workspaces.DocGrantRef{{WorkspaceID: "workspace-1", DocID: "doc-1"}},
	}
	workspace := func() WorkspaceDependencies {
		return WorkspaceDependencies{
			Members:     workspaceMembersReaderFake{members: []workspaces.Member{{ID: "user-1", InviteID: "user-1", Role: "Owner", Status: "Accepted"}}, count: 1},
			UserUpdates: reader, UserUpdateJobs: NewPostgresUserUpdateRefreshJobStore(pool),
			GrantsAuth: docGrantAuthorizerFake{usersRead: true},
		}
	}
	backendA, err := NewWithRedis(config.Config{NodeEnv: "test", Host: "localhost"}, testAuthenticator{}, testDocumentSync{}, redisA, workspace())
	if err != nil {
		t.Fatal(err)
	}
	defer backendA.Close()
	backendB, err := NewWithRedis(config.Config{NodeEnv: "test", Host: "localhost"}, testAuthenticator{}, testDocumentSync{}, redisB, workspace())
	if err != nil {
		t.Fatal(err)
	}
	defer backendB.Close()
	waitForRealtimeFanoutSubscribers(t, redisA, 2)
	serverA := newRealtimeHTTPTestServer(t, backendA)
	defer serverA.Close()
	serverB := newRealtimeHTTPTestServer(t, backendB)
	defer serverB.Close()
	clientA := newPollingClient(t, serverA.URL, "valid-session")
	clientB := newPollingClient(t, serverB.URL, "valid-session")
	if !strings.Contains(clientA.connect(), "40{") || !strings.Contains(clientB.connect(), "40{") {
		t.Fatal("expected both authenticated Socket.IO namespace connections")
	}
	for _, client := range []pollingTestClient{clientA, clientB} {
		subscribeRealtimeTopic(t, client, 1, "user.profile.changed", map[string]any{})
		subscribeRealtimeTopic(t, client, 2, "workspace.members.changed", map[string]any{"workspaceId": "workspace-1"})
		subscribeRealtimeTopic(t, client, 3, "doc.grants.changed", map[string]any{"workspaceId": "workspace-1", "docId": "doc-1"})
	}

	backendA.PublishUserUpdated(ctx, "user-1")
	for _, client := range []pollingTestClient{clientA, clientB} {
		assertUserUpdateEvents(t, client)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var remaining int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM go_user_update_refresh_jobs`).Scan(&remaining); err != nil {
			t.Fatal(err)
		}
		if remaining == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("durable user update job was not completed after both instances received the events")
}

func assertUserUpdateEvents(t *testing.T, client pollingTestClient) {
	t.Helper()
	wanted := []string{"user.profile.changed", "workspace.members.changed", "doc.grants.changed"}
	found := make(map[string]bool, len(wanted))
	for range 5 {
		packet := client.poll()
		for _, topic := range wanted {
			if strings.Contains(packet, `"topic":"`+topic+`"`) && strings.Contains(packet, `"reason":"user-updated"`) {
				found[topic] = true
			}
		}
		if len(found) == len(wanted) {
			return
		}
	}
	t.Fatalf("user update topics received = %#v, want %v", found, wanted)
}

func waitForRealtimeFanoutSubscribers(t *testing.T, client *redis.Client, count int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		subscribers, err := client.PubSubNumSub(context.Background(), realtimeFanoutChannel).Result()
		if err == nil && subscribers[realtimeFanoutChannel] >= count {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("Redis realtime channel did not reach %d subscribers", count)
}

func newRealtimeHTTPTestServer(t *testing.T, backend *Server) *httptest.Server {
	t.Helper()
	return httptest.NewServer(backend.Handler())
}

func assertPacketContains(t *testing.T, packet, expected string) {
	t.Helper()
	if !strings.Contains(packet, expected) {
		t.Fatalf("packet %q does not contain %q", packet, expected)
	}
}

func pollUntilContainsAll(t *testing.T, client pollingTestClient, expected ...string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var packets strings.Builder
	for _, value := range expected {
		for !strings.Contains(packets.String(), value) {
			if time.Now().After(deadline) {
				t.Fatalf("polled packets %q do not contain %q", packets.String(), value)
			}
			packet, err := client.pollWithError()
			if err != nil {
				t.Fatalf("poll for %q: %v", value, err)
			}
			packets.WriteString(packet)
		}
	}
	return packets.String()
}
