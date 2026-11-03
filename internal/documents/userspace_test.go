// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package documents

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Deln0r/ygo"
	"github.com/fydeos/fydeos-notes-server/internal/crdt/yjs"
	"github.com/jackc/pgx/v5"
)

func TestUserspacePushAndDiffRoundTrip(t *testing.T) {
	store := &memoryUserspaceStore{}
	repository := &UserspaceRepository{store: store}
	first := fixtureUpdate(t, 5001, "first")
	second := fixtureUpdate(t, 5002, "second")
	firstTimestamp, err := repository.Push(context.Background(), "user-1", "doc-1", first)
	if err != nil {
		t.Fatal(err)
	}
	secondTimestamp, err := repository.Push(context.Background(), "user-1", "doc-1", second)
	if err != nil {
		t.Fatal(err)
	}
	if secondTimestamp < firstTimestamp {
		t.Fatalf("timestamp moved backwards: first=%d second=%d", firstTimestamp, secondTimestamp)
	}

	missing, stateVector, timestamp, err := repository.Diff(context.Background(), "user-1", "doc-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if timestamp != secondTimestamp {
		t.Fatalf("diff timestamp = %d, want %d", timestamp, secondTimestamp)
	}
	doc, err := yjs.Restore(missing, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := doc.StateVector(), stateVector; string(got) != string(want) {
		t.Fatalf("state vector = %x, want %x", got, want)
	}

	peer := ygo.NewDocWithOptions(ygo.Options{ClientID: 7001})
	if err := ygo.ApplyUpdate(peer, missing); err != nil {
		t.Fatal(err)
	}
	peerVector := ygo.EncodeStateVector(peer)
	delta, _, _, err := repository.Diff(context.Background(), "user-1", "doc-1", peerVector)
	if err != nil {
		t.Fatal(err)
	}
	if len(delta) != 0 && string(delta) != string([]byte{0, 0}) {
		t.Fatalf("diff for synchronized peer = %x, want empty update", delta)
	}
}

func TestUserspacePushRejectsInvalidAndOversizedUpdates(t *testing.T) {
	repository := &UserspaceRepository{store: &memoryUserspaceStore{}}
	if timestamp, err := repository.Push(context.Background(), "user-1", "doc-1", nil); err != nil || timestamp != 0 {
		t.Fatalf("empty Push() = %d, %v; want no-op", timestamp, err)
	}
	for _, update := range [][]byte{{1}, make([]byte, MaxUpdateBytes+1)} {
		if _, err := repository.Push(context.Background(), "user-1", "doc-1", update); !errors.Is(err, ErrInvalidDoc) {
			t.Fatalf("Push(%d bytes) error = %v, want ErrInvalidDoc", len(update), err)
		}
	}
}

func TestUserspaceTimestampsFilterByCursor(t *testing.T) {
	store := &memoryUserspaceStore{docs: map[string]Doc{}}
	base := time.UnixMilli(1_800_000_000_000)
	store.docs["user-1/doc-1"] = Doc{UserID: "user-1", ID: "doc-1", Timestamp: base}
	store.docs["user-1/doc-2"] = Doc{UserID: "user-1", ID: "doc-2", Timestamp: base.Add(time.Second)}
	repository := &UserspaceRepository{store: store}
	cursor := base.UnixMilli()
	got, err := repository.Timestamps(context.Background(), "user-1", &cursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["doc-2"] != base.Add(time.Second).UnixMilli() {
		t.Fatalf("Timestamps() = %#v", got)
	}
}

func TestUserspaceDeleteRemovesOnlyTheSelectedDocument(t *testing.T) {
	store := &memoryUserspaceStore{docs: map[string]Doc{
		"user-1/doc-1": {UserID: "user-1", ID: "doc-1"},
		"user-1/doc-2": {UserID: "user-1", ID: "doc-2"},
		"user-2/doc-1": {UserID: "user-2", ID: "doc-1"},
	}}
	repository := &UserspaceRepository{store: store}
	if err := repository.Delete(context.Background(), "user-1", "doc-1"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := repository.Diff(context.Background(), "user-1", "doc-1", nil); !errors.Is(err, ErrDocNotFound) {
		t.Fatalf("deleted document Diff() error = %v", err)
	}
	if _, _, _, err := repository.Diff(context.Background(), "user-1", "doc-2", nil); err != nil {
		t.Fatalf("neighboring document Diff() error = %v", err)
	}
	if _, _, _, err := repository.Diff(context.Background(), "user-2", "doc-1", nil); err != nil {
		t.Fatalf("other user's document Diff() error = %v", err)
	}
}

func fixtureUpdate(t *testing.T, clientID uint64, value string) []byte {
	t.Helper()
	doc := ygo.NewDocWithOptions(ygo.Options{ClientID: clientID})
	text := ygo.NewText(doc, "body")
	txn := doc.WriteTxn()
	if err := text.Insert(txn, 0, value); err != nil {
		t.Fatal(err)
	}
	txn.Commit()
	return ygo.EncodeStateAsUpdate(doc)
}

type memoryUserspaceStore struct {
	mu   sync.Mutex
	docs map[string]Doc
}

func (s *memoryUserspaceStore) Get(_ context.Context, userID, docID string) (Doc, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, ok := s.docs[userID+"/"+docID]
	if !ok {
		return Doc{}, pgx.ErrNoRows
	}
	return doc, nil
}

func (s *memoryUserspaceStore) Append(_ context.Context, userID, docID string, merge func([]byte, time.Time) ([]byte, time.Time, error)) (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.docs == nil {
		s.docs = make(map[string]Doc)
	}
	key := userID + "/" + docID
	existing := s.docs[key]
	now := time.Now().UTC().Truncate(time.Millisecond)
	if !existing.Timestamp.IsZero() && !existing.Timestamp.Add(time.Millisecond).Before(now) {
		now = existing.Timestamp.Add(time.Millisecond)
	}
	blob, timestamp, err := merge(existing.Blob, now)
	if err != nil {
		return time.Time{}, err
	}
	s.docs[key] = Doc{UserID: userID, ID: docID, Blob: blob, Timestamp: timestamp}
	return timestamp, nil
}

func (s *memoryUserspaceStore) Timestamps(_ context.Context, userID string, after *time.Time) (map[string]int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make(map[string]int64)
	for _, doc := range s.docs {
		if doc.UserID == userID && (after == nil || doc.Timestamp.After(*after)) {
			result[doc.ID] = doc.Timestamp.UnixMilli()
		}
	}
	return result, nil
}

func (s *memoryUserspaceStore) Delete(_ context.Context, userID, docID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.docs, userID+"/"+docID)
	return nil
}
