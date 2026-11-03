// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package yjs

import (
	"bytes"
	"encoding/base64"
	"testing"

	"github.com/Deln0r/ygo"
)

const (
	lockedClientUpdate = "AQOqgICAEAAoAQhzZXR0aW5ncwV0aGVtZQF3BGRhcmsoAQhzZXR0aW5ncwRtb2RlAXgEAQRib2R5CW5vdGVzMjAyNgA="
	lockedClientState  = "AaqAgIAQCw=="
)

func TestLockedYjsClientUpdateDecodesAndRestores(t *testing.T) {
	update := mustDecodeBase64(t, lockedClientUpdate)
	expectedState := mustDecodeBase64(t, lockedClientState)

	doc, err := Restore(nil, [][]byte{update})
	if err != nil {
		t.Fatalf("Restore() error = %v", err)
	}
	if got := doc.StateVector(); !bytes.Equal(got, expectedState) {
		t.Fatalf("state vector = %x, want %x", got, expectedState)
	}
	if got := doc.EncodeState(); !bytes.Equal(got, update) {
		t.Fatalf("encoded state = %x, want original Yjs bytes %x", got, update)
	}
	if doc.HasPendingUpdates() {
		t.Fatal("valid complete fixture unexpectedly left pending updates")
	}
}

func TestGoEncodingMatchesLockedYjsClientBytes(t *testing.T) {
	doc := ygo.NewDocWithOptions(ygo.Options{ClientID: 4294967338, GUID: "fixture"})
	settings := ygo.NewMap(doc, "settings")
	body := ygo.NewText(doc, "body")
	txn := doc.WriteTxn()
	settings.Set(txn, "theme", "dark")
	settings.Set(txn, "mode", true)
	if err := body.Insert(txn, 0, "notes2026"); err != nil {
		t.Fatal(err)
	}
	txn.Commit()

	got := ygo.EncodeStateAsUpdate(doc)
	want := mustDecodeBase64(t, lockedClientUpdate)
	if !bytes.Equal(got, want) {
		t.Fatalf("Go update = %x, want locked Yjs update %x", got, want)
	}
	if got := ygo.EncodeStateVector(doc); !bytes.Equal(got, mustDecodeBase64(t, lockedClientState)) {
		t.Fatalf("Go state vector = %x, want %x", got, mustDecodeBase64(t, lockedClientState))
	}
}

func TestDiffReturnsOnlyStateMissingFromPeer(t *testing.T) {
	base := ygo.NewDocWithOptions(ygo.Options{ClientID: 4001})
	baseMap := ygo.NewMap(base, "settings")
	baseTxn := base.WriteTxn()
	baseMap.Set(baseTxn, "theme", "dark")
	baseTxn.Commit()
	baseState := ygo.EncodeStateAsUpdate(base)
	baseVector := ygo.EncodeStateVector(base)

	complete := ygo.NewDocWithOptions(ygo.Options{ClientID: 4002})
	if err := ygo.ApplyUpdate(complete, baseState); err != nil {
		t.Fatal(err)
	}
	settings := ygo.NewMap(complete, "settings")
	txn := complete.WriteTxn()
	settings.Set(txn, "mode", true)
	txn.Commit()

	diff, err := (&Document{doc: complete}).Diff(baseVector)
	if err != nil {
		t.Fatalf("Diff() error = %v", err)
	}
	peer, err := Restore(baseState, [][]byte{diff})
	if err != nil {
		t.Fatalf("Restore(diff) error = %v", err)
	}
	if got, want := peer.StateVector(), completeStateVector(complete); !bytes.Equal(got, want) {
		t.Fatalf("peer state vector = %x, want %x", got, want)
	}
}

func TestRejectsEmptyMalformedAndConcatenatedUpdates(t *testing.T) {
	doc := NewDocument()
	for _, update := range [][]byte{nil, {1}, append(mustDecodeBase64(t, lockedClientUpdate), mustDecodeBase64(t, lockedClientUpdate)...)} {
		if err := doc.ApplyUpdate(update); err == nil {
			t.Fatalf("ApplyUpdate(%x) succeeded, want validation error", update)
		}
	}
}

func mustDecodeBase64(t *testing.T, value string) []byte {
	t.Helper()
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}

func completeStateVector(doc *ygo.Doc) []byte {
	return ygo.EncodeStateVector(doc)
}
