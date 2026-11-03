// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package realtime

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

const (
	userUpdateQueueCapacity = 512
	userUpdatePageSize      = 32
	userUpdateWorkerCount   = 2
	userUpdateRetryMaxDelay = 5 * time.Second
	userUpdateLeaseDuration = 30 * time.Second
	userUpdateLeaseRenewal  = 10 * time.Second
)

type pendingUserUpdate struct {
	dirty            bool
	grants           bool
	afterWorkspaceID string
	afterGrant       *workspaces.DocGrantRef
	retryAttempt     uint
}

type userUpdateQueue struct {
	server  *Server
	reader  UserUpdateReader
	store   UserUpdateRefreshJobStore
	ctx     context.Context
	cancel  context.CancelFunc
	jobs    chan string
	wake    chan struct{}
	mu      sync.Mutex
	space   *sync.Cond
	pending map[string]*pendingUserUpdate
	closed  bool
	workers sync.WaitGroup
	retries sync.WaitGroup
}

func newUserUpdateQueue(server *Server, reader UserUpdateReader) *userUpdateQueue {
	ctx, cancel := context.WithCancel(context.Background())
	queue := &userUpdateQueue{
		server: server, reader: reader, ctx: ctx, cancel: cancel,
		jobs:    make(chan string, userUpdateQueueCapacity),
		pending: make(map[string]*pendingUserUpdate, userUpdateQueueCapacity),
	}
	queue.space = sync.NewCond(&queue.mu)
	queue.workers.Add(userUpdateWorkerCount)
	for range userUpdateWorkerCount {
		go queue.run()
	}
	return queue
}

func newDurableUserUpdateQueue(server *Server, reader UserUpdateReader, store UserUpdateRefreshJobStore) *userUpdateQueue {
	ctx, cancel := context.WithCancel(context.Background())
	queue := &userUpdateQueue{
		server: server, reader: reader, store: store, ctx: ctx, cancel: cancel,
		wake: make(chan struct{}, userUpdateWorkerCount), pending: make(map[string]*pendingUserUpdate),
	}
	queue.space = sync.NewCond(&queue.mu)
	queue.workers.Add(userUpdateWorkerCount)
	for range userUpdateWorkerCount {
		go queue.run()
	}
	return queue
}

func (q *userUpdateQueue) Enqueue(ctx context.Context, userID string) error {
	if q.store != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
		q.mu.Lock()
		closed := q.closed
		q.mu.Unlock()
		if closed {
			return context.Canceled
		}
		if err := q.store.MarkDirty(ctx, userID); err != nil {
			return err
		}
		select {
		case q.wake <- struct{}{}:
		default:
		}
		return nil
	}
	stop := context.AfterFunc(ctx, func() {
		q.mu.Lock()
		q.space.Broadcast()
		q.mu.Unlock()
	})
	defer stop()
	q.mu.Lock()
	defer q.mu.Unlock()
	for {
		if q.closed {
			return context.Canceled
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if pending, ok := q.pending[userID]; ok {
			pending.dirty = true
			return nil
		}
		if len(q.pending) < userUpdateQueueCapacity {
			q.pending[userID] = &pendingUserUpdate{}
			q.jobs <- userID
			return nil
		}
		q.space.Wait()
	}
}

func (q *userUpdateQueue) Close() {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		q.workers.Wait()
		q.retries.Wait()
		return
	}
	q.closed = true
	q.cancel()
	q.space.Broadcast()
	q.mu.Unlock()
	q.workers.Wait()
	q.retries.Wait()
}

func (q *userUpdateQueue) run() {
	defer q.workers.Done()
	if q.store != nil {
		q.runDurable()
		return
	}
	for {
		select {
		case <-q.ctx.Done():
			return
		case userID := <-q.jobs:
			q.mu.Lock()
			q.space.Broadcast()
			q.mu.Unlock()
			q.processOnePage(userID)
		}
	}
}

func (q *userUpdateQueue) runDurable() {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-q.ctx.Done():
			return
		case <-q.wake:
		case <-ticker.C:
		}
		for q.ctx.Err() == nil {
			processed, err := q.processDurableOne()
			if err != nil {
				slog.Warn("user update durable refresh poll failed", "error", err)
				break
			}
			if !processed {
				break
			}
		}
	}
}

func (q *userUpdateQueue) processDurableOne() (bool, error) {
	job, ok, err := q.store.Claim(q.ctx, userUpdateLeaseDuration)
	if err != nil || !ok {
		return ok, err
	}
	state := pendingUserUpdate{
		grants: job.State.Grants, afterWorkspaceID: job.State.AfterWorkspaceID,
		afterGrant: job.State.AfterGrant,
	}
	jobCtx, cancelJob := context.WithCancel(q.ctx)
	leaseLost := make(chan struct{})
	renewDone := make(chan struct{})
	go func() {
		defer close(renewDone)
		q.renewDurableLease(jobCtx, cancelJob, job, leaseLost)
	}()
	next, more, err := q.publishUserScopePageContext(jobCtx, job.UserID, state)
	cancelJob()
	<-renewDone
	if q.ctx.Err() != nil {
		return true, nil
	}
	select {
	case <-leaseLost:
		return true, nil
	default:
	}
	if err != nil {
		delay := userUpdateRetryDelay(job.Attempt + 1)
		if retryErr := q.store.Retry(q.ctx, job, delay); retryErr != nil {
			return true, retryErr
		}
		slog.Warn("user update durable refresh will retry", "user_id", job.UserID, "generation", job.Generation, "attempt", job.Attempt+1, "retry_in", delay, "error", err)
		return true, nil
	}
	if more {
		nextState := UserUpdateJobState{Grants: next.grants, AfterWorkspaceID: next.afterWorkspaceID, AfterGrant: next.afterGrant}
		return true, q.store.Advance(q.ctx, job, nextState)
	}
	return true, q.store.Complete(q.ctx, job)
}

func (q *userUpdateQueue) renewDurableLease(ctx context.Context, cancel context.CancelFunc, job UserUpdateJob, leaseLost chan<- struct{}) {
	ticker := time.NewTicker(userUpdateLeaseRenewal)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		renewCtx, renewCancel := context.WithTimeout(ctx, 5*time.Second)
		owned, err := q.store.Renew(renewCtx, job, userUpdateLeaseDuration)
		renewCancel()
		if err == nil && owned {
			continue
		}
		if err != nil {
			slog.Warn("user update durable refresh lease renewal failed", "user_id", job.UserID, "generation", job.Generation, "error", err)
		}
		close(leaseLost)
		cancel()
		return
	}
}

func (q *userUpdateQueue) processOnePage(userID string) {
	if q.ctx.Err() != nil {
		return
	}
	q.mu.Lock()
	pending := q.pending[userID]
	if pending == nil {
		q.mu.Unlock()
		return
	}
	state := *pending
	if pending.afterGrant != nil {
		afterGrant := *pending.afterGrant
		state.afterGrant = &afterGrant
	}
	q.mu.Unlock()

	nextState, more, err := q.publishUserScopePage(userID, state)

	q.mu.Lock()
	defer q.mu.Unlock()
	pending = q.pending[userID]
	if pending == nil || q.ctx.Err() != nil {
		return
	}
	if pending.dirty {
		pending.dirty = false
		pending.grants = false
		pending.afterWorkspaceID = ""
		pending.afterGrant = nil
		more = true
	} else {
		pending.grants = nextState.grants
		pending.afterWorkspaceID = nextState.afterWorkspaceID
		pending.afterGrant = nextState.afterGrant
	}
	if err != nil {
		pending.retryAttempt++
		attempt := pending.retryAttempt
		delay := userUpdateRetryDelay(attempt)
		slog.Warn("user update realtime refresh will retry", "user_id", userID, "attempt", attempt, "retry_in", delay, "error", err)
		q.scheduleRetry(userID, delay)
		return
	}
	pending.retryAttempt = 0
	if more {
		q.jobs <- userID
		return
	}
	delete(q.pending, userID)
	q.space.Broadcast()
}

func (q *userUpdateQueue) publishUserScopePage(userID string, state pendingUserUpdate) (pendingUserUpdate, bool, error) {
	return q.publishUserScopePageContext(q.ctx, userID, state)
}

func (q *userUpdateQueue) publishUserScopePageContext(ctx context.Context, userID string, state pendingUserUpdate) (pendingUserUpdate, bool, error) {
	if !state.grants {
		queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		workspaceIDs, err := q.reader.UserWorkspacePage(queryCtx, userID, state.afterWorkspaceID, userUpdatePageSize)
		cancel()
		if err != nil {
			return state, true, err
		}
		for _, workspaceID := range workspaceIDs {
			if ctx.Err() != nil {
				return state, true, ctx.Err()
			}
			if err := q.server.publishWorkspaceMembersChangedContext(ctx, workspaceID, "user-updated"); err != nil {
				return state, true, err
			}
			state.afterWorkspaceID = workspaceID
		}
		if len(workspaceIDs) < userUpdatePageSize {
			state.grants = true
			state.afterWorkspaceID = ""
		}
		return state, true, nil
	}

	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	grants, err := q.reader.DirectGrantDocPage(queryCtx, userID, state.afterGrant, userUpdatePageSize)
	cancel()
	if err != nil {
		return state, true, err
	}
	for _, grant := range grants {
		if ctx.Err() != nil {
			return state, true, ctx.Err()
		}
		if err := q.server.publishDocGrantsChangedContext(ctx, grant.WorkspaceID, grant.DocID, "user-updated"); err != nil {
			return state, true, err
		}
		after := grant
		state.afterGrant = &after
	}
	if len(grants) < userUpdatePageSize {
		return state, false, nil
	}
	return state, true, nil
}

func userUpdateRetryDelay(attempt uint) time.Duration {
	delay := 100 * time.Millisecond
	for i := uint(1); i < attempt && delay < userUpdateRetryMaxDelay; i++ {
		delay *= 2
	}
	if delay > userUpdateRetryMaxDelay {
		return userUpdateRetryMaxDelay
	}
	return delay
}

func (q *userUpdateQueue) scheduleRetry(userID string, delay time.Duration) {
	q.retries.Add(1)
	go func() {
		defer q.retries.Done()
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-q.ctx.Done():
			return
		case <-timer.C:
		}
		q.mu.Lock()
		if q.closed || q.pending[userID] == nil {
			q.mu.Unlock()
			return
		}
		q.mu.Unlock()
		select {
		case q.jobs <- userID:
		case <-q.ctx.Done():
		}
	}()
}
