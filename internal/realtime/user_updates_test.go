// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package realtime

import (
	"context"
	"errors"
	"net"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
	"github.com/redis/go-redis/v9"
)

type userUpdateReaderFake struct {
	workspaceIDs []string
	grants       []workspaces.DocGrantRef
	page         workspaces.DocGrantPage
}

func (f userUpdateReaderFake) UserWorkspaceIDs(context.Context, string) ([]string, error) {
	return f.workspaceIDs, nil
}

func (f userUpdateReaderFake) UserWorkspacePage(_ context.Context, _ string, after string, limit int) ([]string, error) {
	page := make([]string, 0, limit)
	for _, id := range f.workspaceIDs {
		if id > after && len(page) < limit {
			page = append(page, id)
		}
	}
	return page, nil
}

func (f userUpdateReaderFake) DirectGrantDocIDsByUser(context.Context, string) ([]workspaces.DocGrantRef, error) {
	return f.grants, nil
}

func (f userUpdateReaderFake) DirectGrantDocPage(_ context.Context, _ string, after *workspaces.DocGrantRef, limit int) ([]workspaces.DocGrantRef, error) {
	page := make([]workspaces.DocGrantRef, 0, limit)
	for _, grant := range f.grants {
		if after != nil && (grant.WorkspaceID < after.WorkspaceID || grant.WorkspaceID == after.WorkspaceID && grant.DocID <= after.DocID) {
			continue
		}
		if len(page) == limit {
			break
		}
		page = append(page, grant)
	}
	return page, nil
}

func (f userUpdateReaderFake) ListDirectDocGrantUsers(context.Context, string, string, int, int, *time.Time) (workspaces.DocGrantPage, error) {
	return f.page, nil
}

type docGrantAuthorizerFake struct {
	usersRead bool
}

func (f docGrantAuthorizerFake) AuthorizeDocument(context.Context, string, string, string) (workspaces.DocumentAuthorization, error) {
	return workspaces.DocumentAuthorization{Read: true, UsersRead: f.usersRead}, nil
}

func TestRealtimeDocGrantsSnapshotAndUserUpdatedPropagation(t *testing.T) {
	avatarURL := "https://example.test/avatar.png"
	reader := userUpdateReaderFake{
		workspaceIDs: []string{"workspace-1", "workspace-invited"},
		grants:       []workspaces.DocGrantRef{{WorkspaceID: "workspace-1", DocID: "doc-1"}},
		page: workspaces.DocGrantPage{
			TotalCount: 1, HasNext: true,
			EndCursor: ptr("MjAyNi0wMS0wMVQwMDowMDowMC4wMDBa"),
			Users: []workspaces.DocGrantUser{{
				UserID: "granted-user", Name: "Granted User", Email: "granted@example.test",
				AvatarURL: &avatarURL, Role: "editor",
				CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			}},
		},
	}
	backend, err := NewWithDependencies(config.Config{NodeEnv: "test", Host: "localhost"}, testAuthenticator{}, testDocumentSync{}, WorkspaceDependencies{
		Members: workspaceMembersReaderFake{count: 1}, UserUpdates: reader, GrantsAuth: docGrantAuthorizerFake{usersRead: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	server := httptest.NewServer(backend.Handler())
	defer server.Close()

	docClient := newPollingClient(t, server.URL, "valid-session")
	if !strings.Contains(docClient.connect(), "40{") {
		t.Fatal("expected authenticated Socket.IO connection")
	}
	request := docClient.emitAck(t, 1, "realtime:request", map[string]any{
		"op": "doc.grants.get", "input": map[string]any{
			"workspaceId": "workspace-1", "docId": "doc-1",
			"pagination": map[string]any{"first": 8},
		}, "clientVersion": "0.26.0",
	})
	for _, field := range []string{`"totalCount":1`, `"hasNextPage":true`, `"role":"Editor"`, `"id":"granted-user"`, `"email":"granted@example.test"`, `"avatarUrl":"https://example.test/avatar.png"`} {
		if !strings.Contains(request, field) {
			t.Fatalf("doc grants response missing %s: %q", field, request)
		}
	}

	profileClient := newPollingClient(t, server.URL, "valid-session")
	workspaceClient := newPollingClient(t, server.URL, "valid-session")
	if !strings.Contains(profileClient.connect(), "40{") || !strings.Contains(workspaceClient.connect(), "40{") {
		t.Fatal("expected authenticated observer connections")
	}
	subscribeRealtimeTopic(t, profileClient, 1, "user.profile.changed", map[string]any{})
	subscribeRealtimeTopic(t, workspaceClient, 1, "workspace.members.changed", map[string]any{"workspaceId": "workspace-1"})
	subscribeRealtimeTopic(t, docClient, 2, "doc.grants.changed", map[string]any{"workspaceId": "workspace-1", "docId": "doc-1"})

	backend.PublishUserUpdated(context.Background(), "user-1")
	assertRealtimeEvent(t, profileClient.poll(), `"topic":"user.profile.changed"`, `"reason":"user-updated"`)
	assertRealtimeEvent(t, workspaceClient.poll(), `"topic":"workspace.members.changed"`, `"reason":"user-updated"`)
	assertRealtimeEvent(t, docClient.poll(), `"topic":"doc.grants.changed"`, `"reason":"user-updated"`)
}

func TestRealtimeDocGrantsRequiresUsersRead(t *testing.T) {
	backend, err := NewWithDependencies(config.Config{NodeEnv: "test", Host: "localhost"}, testAuthenticator{}, testDocumentSync{}, WorkspaceDependencies{
		UserUpdates: userUpdateReaderFake{}, GrantsAuth: docGrantAuthorizerFake{usersRead: false},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	server := httptest.NewServer(backend.Handler())
	defer server.Close()

	client := newPollingClient(t, server.URL, "valid-session")
	if !strings.Contains(client.connect(), "40{") {
		t.Fatal("expected authenticated Socket.IO connection")
	}
	request := client.emitAck(t, 1, "realtime:request", map[string]any{
		"op": "doc.grants.get", "input": map[string]any{
			"workspaceId": "workspace-1", "docId": "doc-1", "pagination": map[string]any{"first": 8},
		}, "clientVersion": "0.26.0",
	})
	if !strings.Contains(request, `"name":"DOC_ACTION_DENIED"`) {
		t.Fatalf("unauthorized doc grants request = %q", request)
	}
	subscription := client.emitAck(t, 2, "realtime:subscribe", map[string]any{
		"topic": "doc.grants.changed", "input": map[string]any{"workspaceId": "workspace-1", "docId": "doc-1"},
		"clientVersion": "0.26.0",
	})
	if !strings.Contains(subscription, `"name":"DOC_ACTION_DENIED"`) {
		t.Fatalf("unauthorized doc grants subscription = %q", subscription)
	}
}

type blockingUserUpdateReader struct {
	mu      sync.Mutex
	calls   int
	entered chan struct{}
	release chan struct{}
}

func (r *blockingUserUpdateReader) UserWorkspacePage(context.Context, string, string, int) ([]string, error) {
	r.mu.Lock()
	r.calls++
	calls := r.calls
	r.mu.Unlock()
	if calls == 1 {
		close(r.entered)
		<-r.release
	}
	return nil, nil
}

func (*blockingUserUpdateReader) DirectGrantDocPage(context.Context, string, *workspaces.DocGrantRef, int) ([]workspaces.DocGrantRef, error) {
	return nil, nil
}

func (*blockingUserUpdateReader) ListDirectDocGrantUsers(context.Context, string, string, int, int, *time.Time) (workspaces.DocGrantPage, error) {
	return workspaces.DocGrantPage{}, nil
}

func TestUserUpdateQueueCoalescesUpdatesArrivingDuringRefresh(t *testing.T) {
	reader := &blockingUserUpdateReader{entered: make(chan struct{}), release: make(chan struct{})}
	queue := newUserUpdateQueue(&Server{}, reader)
	defer queue.Close()

	if err := queue.Enqueue(context.Background(), "user-1"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-reader.entered:
	case <-time.After(time.Second):
		t.Fatal("worker did not start user update refresh")
	}
	if err := queue.Enqueue(context.Background(), "user-1"); err != nil {
		t.Fatal(err)
	}
	if err := queue.Enqueue(context.Background(), "user-1"); err != nil {
		t.Fatal(err)
	}
	close(reader.release)

	deadline := time.After(time.Second)
	for {
		queue.mu.Lock()
		pending := queue.pending["user-1"]
		queue.mu.Unlock()
		reader.mu.Lock()
		calls := reader.calls
		reader.mu.Unlock()
		if pending == nil {
			if calls != 2 {
				t.Fatalf("workspace refresh count = %d, want one initial and one coalesced refresh", calls)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("coalesced user update did not finish")
		case <-time.After(time.Millisecond):
		}
	}
}

type flakyUserUpdateReader struct {
	mu                sync.Mutex
	workspaceFailures int
	grantFailures     int
	workspaceCalls    int
	grantCalls        int
}

func (r *flakyUserUpdateReader) UserWorkspacePage(context.Context, string, string, int) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.workspaceCalls++
	if r.workspaceFailures > 0 {
		r.workspaceFailures--
		return nil, errors.New("temporary workspace lookup failure")
	}
	return []string{"workspace-1"}, nil
}

func (r *flakyUserUpdateReader) DirectGrantDocPage(context.Context, string, *workspaces.DocGrantRef, int) ([]workspaces.DocGrantRef, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.grantCalls++
	if r.grantFailures > 0 {
		r.grantFailures--
		return nil, errors.New("temporary grant lookup failure")
	}
	return nil, nil
}

func (*flakyUserUpdateReader) ListDirectDocGrantUsers(context.Context, string, string, int, int, *time.Time) (workspaces.DocGrantPage, error) {
	return workspaces.DocGrantPage{}, nil
}

func TestUserUpdateQueueRetriesFailedWorkspaceAndGrantLookups(t *testing.T) {
	for _, test := range []struct {
		name              string
		workspaceFailures int
		grantFailures     int
	}{
		{name: "workspace lookup", workspaceFailures: 1},
		{name: "direct grant lookup", grantFailures: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := &flakyUserUpdateReader{workspaceFailures: test.workspaceFailures, grantFailures: test.grantFailures}
			queue := newUserUpdateQueue(&Server{}, reader)
			defer queue.Close()
			if err := queue.Enqueue(context.Background(), "user-1"); err != nil {
				t.Fatal(err)
			}

			deadline := time.After(3 * time.Second)
			for {
				queue.mu.Lock()
				pending := queue.pending["user-1"] != nil
				queue.mu.Unlock()
				reader.mu.Lock()
				workspaceCalls, grantCalls := reader.workspaceCalls, reader.grantCalls
				reader.mu.Unlock()
				if !pending {
					if test.workspaceFailures > 0 && workspaceCalls < 2 {
						t.Fatalf("workspace lookup calls = %d, want retry", workspaceCalls)
					}
					if test.grantFailures > 0 && grantCalls < 2 {
						t.Fatalf("direct grant lookup calls = %d, want retry", grantCalls)
					}
					return
				}
				select {
				case <-deadline:
					t.Fatal("user update refresh did not finish after transient lookup failure")
				case <-time.After(5 * time.Millisecond):
				}
			}
		})
	}
}

func TestUserUpdateQueueKeepsPendingRefreshWhenRedisFanoutFails(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	client := redis.NewClient(&redis.Options{Addr: address, MaxRetries: -1, DialTimeout: 50 * time.Millisecond})
	defer client.Close()
	server := &Server{fanout: &realtimeFanout{client: client, source: "user-update-test"}}
	reader := &flakyUserUpdateReader{}
	queue := newUserUpdateQueue(server, reader)
	defer queue.Close()
	if err := queue.Enqueue(context.Background(), "user-1"); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(3 * time.Second)
	for {
		queue.mu.Lock()
		pending := queue.pending["user-1"]
		retried := pending != nil && pending.retryAttempt > 0
		queue.mu.Unlock()
		if retried {
			return
		}
		select {
		case <-deadline:
			t.Fatal("Redis fanout failure did not retain and retry the refresh")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func fullUserUpdateQueue() *userUpdateQueue {
	ctx, cancel := context.WithCancel(context.Background())
	queue := &userUpdateQueue{
		ctx: ctx, cancel: cancel,
		jobs:    make(chan string, userUpdateQueueCapacity),
		pending: make(map[string]*pendingUserUpdate, userUpdateQueueCapacity),
	}
	queue.space = sync.NewCond(&queue.mu)
	for i := range userUpdateQueueCapacity {
		userID := strconv.Itoa(i)
		queue.pending[userID] = &pendingUserUpdate{}
		queue.jobs <- userID
	}
	return queue
}

func TestUserUpdateQueueBackpressuresInsteadOfDropping(t *testing.T) {
	queue := fullUserUpdateQueue()
	defer queue.Close()

	enqueued := make(chan error, 1)
	go func() {
		enqueued <- queue.Enqueue(context.Background(), "overflow-user")
	}()
	select {
	case <-enqueued:
		t.Fatal("enqueue should wait while the bounded queue is full")
	case <-time.After(20 * time.Millisecond):
	}

	completedUserID := <-queue.jobs
	queue.mu.Lock()
	delete(queue.pending, completedUserID)
	queue.space.Broadcast()
	queue.mu.Unlock()

	select {
	case err := <-enqueued:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("enqueue did not resume after capacity became available")
	}
	var overflowJobs int
	for range userUpdateQueueCapacity {
		if userID := <-queue.jobs; userID == "overflow-user" {
			overflowJobs++
		}
	}
	if overflowJobs != 1 {
		t.Fatalf("overflow job count = %d, want 1", overflowJobs)
	}
}

func TestUserUpdateQueueAdmissionRespondsToRequestCancellation(t *testing.T) {
	queue := fullUserUpdateQueue()
	defer queue.Close()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- queue.Enqueue(ctx, "canceled-user") }()
	cancel()
	select {
	case err := <-result:
		if err != context.Canceled {
			t.Fatalf("admission error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled request remained blocked on queue admission")
	}
	queue.mu.Lock()
	_, admitted := queue.pending["canceled-user"]
	queue.mu.Unlock()
	if admitted {
		t.Fatal("canceled request unexpectedly entered a full queue")
	}
}

func TestUserUpdateQueueRejectsAlreadyCanceledRequestBeforeAdmission(t *testing.T) {
	queueCtx, queueCancel := context.WithCancel(context.Background())
	queue := &userUpdateQueue{
		ctx: queueCtx, cancel: queueCancel,
		jobs: make(chan string, userUpdateQueueCapacity), pending: make(map[string]*pendingUserUpdate),
	}
	queue.space = sync.NewCond(&queue.mu)
	defer queue.Close()

	requestCtx, requestCancel := context.WithCancel(context.Background())
	requestCancel()
	if err := queue.Enqueue(requestCtx, "canceled-user"); err != context.Canceled {
		t.Fatalf("already canceled admission error = %v, want context canceled", err)
	}
	if len(queue.pending) != 0 || len(queue.jobs) != 0 {
		t.Fatalf("already canceled request entered queue: pending=%d jobs=%d", len(queue.pending), len(queue.jobs))
	}
}

func subscribeRealtimeTopic(t *testing.T, client pollingTestClient, id int, topic string, input map[string]any) {
	t.Helper()
	ack := client.emitAck(t, id, "realtime:subscribe", map[string]any{
		"topic": topic, "input": input, "clientVersion": "0.26.0",
	})
	if !strings.Contains(ack, `"data":{"subscriptionId":`) {
		t.Fatalf("%s subscription ack = %q", topic, ack)
	}
}

func assertRealtimeEvent(t *testing.T, packet string, fields ...string) {
	t.Helper()
	for _, field := range fields {
		if !strings.Contains(packet, field) {
			t.Fatalf("realtime event missing %s: %q", field, packet)
		}
	}
}

func ptr[T any](value T) *T {
	return &value
}
