// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package realtime

import (
	"context"
	"strings"
	"sync"

	"github.com/zishang520/socket.io/v2/socket"
)

type workspaceDocSubscription struct {
	Read       bool
	Update     bool
	Generation int64
}

type workspaceSubscriber struct {
	client       *socket.Socket
	state        *syncClientState
	subscription workspaceDocSubscription
}

type workspaceSubscriptionRegistry struct {
	mu   sync.RWMutex
	docs map[string]map[string]workspaceSubscriber
}

func newWorkspaceSubscriptionRegistry() *workspaceSubscriptionRegistry {
	return &workspaceSubscriptionRegistry{docs: make(map[string]map[string]workspaceSubscriber)}
}

func (r *workspaceSubscriptionRegistry) add(client *socket.Socket, state *syncClientState, workspaceID, docID string, subscription workspaceDocSubscription) {
	key := workspaceDocKey(workspaceID, docID)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.docs[key] == nil {
		r.docs[key] = make(map[string]workspaceSubscriber)
	}
	r.docs[key][string(client.Id())] = workspaceSubscriber{client: client, state: state, subscription: subscription}
}

func (r *workspaceSubscriptionRegistry) remove(clientID, workspaceID, docID string) {
	key := workspaceDocKey(workspaceID, docID)
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.docs[key], clientID)
	if len(r.docs[key]) == 0 {
		delete(r.docs, key)
	}
}

func (r *workspaceSubscriptionRegistry) removeClient(clientID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, subscribers := range r.docs {
		delete(subscribers, clientID)
		if len(subscribers) == 0 {
			delete(r.docs, key)
		}
	}
}

func (r *workspaceSubscriptionRegistry) emit(ctx context.Context, documents WorkspaceDocumentSync, workspaceID, docID, excludeClientID, event string, payload any) {
	r.mu.RLock()
	subscribers := make([]workspaceSubscriber, 0, len(r.docs[workspaceDocKey(workspaceID, docID)]))
	for _, subscriber := range r.docs[workspaceDocKey(workspaceID, docID)] {
		subscribers = append(subscribers, subscriber)
	}
	r.mu.RUnlock()
	if len(subscribers) == 0 {
		return
	}

	generation, err := documents.CurrentPermissionGeneration(ctx, workspaceID)
	for _, subscriber := range subscribers {
		if string(subscriber.client.Id()) == excludeClientID {
			continue
		}
		if err != nil || generation != subscriber.subscription.Generation {
			r.remove(string(subscriber.client.Id()), workspaceID, docID)
			subscriber.state.mu.Lock()
			delete(subscriber.state.workspaceDocs, workspaceDocKey(workspaceID, docID))
			subscriber.state.mu.Unlock()
			continue
		}
		if !subscriber.subscription.Read {
			continue
		}
		_ = subscriber.client.Emit(event, payload)
	}
}

func (r *workspaceSubscriptionRegistry) emitAfterPermissionChange(ctx context.Context, authorizer WorkspaceAuthorizer, documents WorkspaceDocumentSync, workspaceID, docID, excludeClientID, event string, payload any) {
	r.revalidateDocument(ctx, authorizer, documents, workspaceID, docID, excludeClientID, event, payload)
}

func (r *workspaceSubscriptionRegistry) revalidateWorkspace(ctx context.Context, authorizer WorkspaceAuthorizer, documents WorkspaceDocumentSync, workspaceID string) {
	prefix := workspaceID + "\x00"
	r.mu.RLock()
	docIDs := make([]string, 0)
	for key := range r.docs {
		if strings.HasPrefix(key, prefix) {
			docIDs = append(docIDs, strings.TrimPrefix(key, prefix))
		}
	}
	r.mu.RUnlock()
	for _, docID := range docIDs {
		r.revalidateDocument(ctx, authorizer, documents, workspaceID, docID, "", "", nil)
	}
}

func (r *workspaceSubscriptionRegistry) revalidateDocument(ctx context.Context, authorizer WorkspaceAuthorizer, documents WorkspaceDocumentSync, workspaceID, docID, excludeClientID, event string, payload any) {
	r.mu.RLock()
	subscribers := make([]workspaceSubscriber, 0, len(r.docs[workspaceDocKey(workspaceID, docID)]))
	for _, subscriber := range r.docs[workspaceDocKey(workspaceID, docID)] {
		subscribers = append(subscribers, subscriber)
	}
	r.mu.RUnlock()

	for _, subscriber := range subscribers {
		clientID := string(subscriber.client.Id())
		if clientID == excludeClientID {
			continue
		}
		access, err := authorizer.AuthorizeWorkspaceSync(ctx, subscriber.state.userID, workspaceID, []string{docID})
		if err != nil || !access.CanSync || !access.Documents[docID].Read {
			removeWorkspaceSubscription(subscriber.client, subscriber.state, r, workspaceID, docID)
			continue
		}
		generation, err := documents.CurrentPermissionGeneration(ctx, workspaceID)
		if err != nil || generation != access.Generation {
			removeWorkspaceSubscription(subscriber.client, subscriber.state, r, workspaceID, docID)
			continue
		}

		state := subscriber.state
		state.mu.Lock()
		if _, stillSubscribed := state.workspaceDocs[workspaceDocKey(workspaceID, docID)]; !stillSubscribed {
			state.mu.Unlock()
			continue
		}
		refreshed := workspaceDocSubscription{Read: true, Update: access.Documents[docID].Update, Generation: generation}
		state.workspaceDocs[workspaceDocKey(workspaceID, docID)] = refreshed
		state.workspaceSpaces[workspaceID] = generation
		r.add(subscriber.client, state, workspaceID, docID, refreshed)
		state.mu.Unlock()
		if event != "" {
			_ = subscriber.client.Emit(event, payload)
		}
	}
}
