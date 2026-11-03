// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package blobs

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Deln0r/ygo"
	"github.com/fydeos/fydeos-notes-server/internal/storage"
)

func TestKeyMatchesIgnoresPadding(t *testing.T) {
	data := []byte("hello")
	key := contentKey(data)
	if strings.Contains(key, "=") || !keyMatches(key, data) || !keyMatches(key+"=", data) || keyMatches(key, []byte("other")) {
		t.Fatal("content key comparison is wrong")
	}
}

func TestValidateBlobInput(t *testing.T) {
	cases := []struct {
		key  string
		size int64
		want error
	}{
		{"k", 0, errInvalidReservation},
		{"k", 1 << 31, errSizeRange},
		{"a/b", 1, errInvalidReservation},
		{"", 1, errInvalidReservation},
		{"k", 1, nil},
	}
	for _, c := range cases {
		if err := validateBlobInput("actor", "ws", c.key, c.size); !errors.Is(err, c.want) && err != c.want {
			t.Fatalf("key=%q size=%d err=%v want %v", c.key, c.size, err, c.want)
		}
	}
}

func TestValidateParts(t *testing.T) {
	uploaded := []storage.MultipartPart{{PartNumber: 1, ETag: "a", Size: 5}, {PartNumber: 2, ETag: "b", Size: 3}}
	ok := []storage.CompletedPart{{PartNumber: 1, ETag: `"a"`}, {PartNumber: 2, ETag: "b"}}
	if err := validateParts(ok, uploaded, 8); err != nil {
		t.Fatal(err)
	}
	bad := [][]storage.CompletedPart{
		{{PartNumber: 1, ETag: "x"}, {PartNumber: 2, ETag: "b"}},
		{{PartNumber: 1, ETag: "a"}},
		{{PartNumber: 1, ETag: "a"}, {PartNumber: 1, ETag: "a"}},
		{{PartNumber: 1, ETag: "a"}, {PartNumber: 3, ETag: "b"}},
	}
	for i, parts := range bad {
		if err := validateParts(parts, uploaded, 8); !errors.Is(err, errMetadataMismatch) {
			t.Fatalf("case %d err=%v", i, err)
		}
	}
	if err := validateParts(ok, uploaded, 7); !errors.Is(err, errMetadataMismatch) {
		t.Fatal("oversized parts accepted")
	}
}

func TestSniffMime(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n0000")
	if got := sniffMime(png); got != "image/png" {
		t.Fatalf("png=%q", got)
	}
	if got := sniffMime([]byte("plain text")); got != "text/plain" {
		t.Fatalf("text=%q", got)
	}
	if got := sniffMime(nil); got != "application/octet-stream" {
		t.Fatalf("empty=%q", got)
	}
}

func TestProxyHandlerParameterErrors(t *testing.T) {
	s := NewService(nil, nil, "cloud")
	handler := s.ProxyUploadHandler()
	cases := []struct{ path, want string }{
		{"/api/storage/upload?workspaceId=w&contentType=t&contentLength=1&expiresAt=1&token=x", "Missing key."},
		{"/api/storage/upload?workspaceId=w&key=k&key=k2&contentType=t&contentLength=1&expiresAt=1&token=x", "Missing key."},
		{"/api/storage/upload?workspaceId=w&key=k&contentType=t&contentLength=1.5&expiresAt=1&token=x", "Invalid contentLength."},
		{"/api/storage/upload?workspaceId=w&key=k&contentType=t&contentLength=-1&expiresAt=1&token=x", "Invalid content length"},
		{"/api/storage/upload?workspaceId=w&key=k&contentType=t&contentLength=1e20&expiresAt=1&token=x", "Invalid contentLength."},
		{"/api/storage/multipart?workspaceId=w&key=k&uploadId=u&partNumber=0&contentLength=1&expiresAt=1&token=x", "Invalid part number"},
		{"/api/storage/multipart?workspaceId=w&key=k&uploadId=u&partNumber=1&contentLength=0&expiresAt=1&token=x", "Invalid content length"},
	}
	for _, c := range cases {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPut, c.path, strings.NewReader("x")))
		var body map[string]string
		_ = json.Unmarshal(recorder.Body.Bytes(), &body)
		if recorder.Code != http.StatusBadRequest || body["message"] != c.want || !strings.HasPrefix(recorder.Header().Get("Content-Type"), "application/json") {
			t.Fatalf("%s: status=%d body=%v", c.path, recorder.Code, body)
		}
	}
	// Multipart requires a Content-Length header.
	request := httptest.NewRequest(http.MethodPut, "/api/storage/multipart?workspaceId=w&key=k&uploadId=u&partNumber=1&contentLength=5.0&expiresAt=1e3&token=x", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if !strings.Contains(recorder.Body.String(), "Missing Content-Length header") {
		t.Fatalf("missing header body=%s", recorder.Body.String())
	}
}

func TestCursorRoundTrip(t *testing.T) {
	encoded := encodeCursor(manifestCursor{DocID: "d", Key: "k"})
	if strings.Contains(encoded, "=") {
		t.Fatal("cursor is padded")
	}
	got, err := decodeCursor(encoded)
	if err != nil || got.DocID != "d" || got.Key != "k" {
		t.Fatalf("cursor=%v err=%v", got, err)
	}
	if _, err := decodeCursor("!!garbage"); !errors.Is(err, ErrManifestCursor) {
		t.Fatal("garbage cursor accepted")
	}
}

func TestValidateSource(t *testing.T) {
	ts := int64(1700000000000)
	far := int64(9e15)
	for _, c := range []struct {
		source BlobSource
		ok     bool
	}{
		{BlobSource{Type: "currentDoc", WorkspaceID: "w", DocID: "d"}, true},
		{BlobSource{Type: "history", WorkspaceID: "w", DocID: "d"}, false},
		{BlobSource{Type: "history", WorkspaceID: "w", DocID: "d", TimestampMS: &ts}, true},
		{BlobSource{Type: "history", WorkspaceID: "w", DocID: "d", TimestampMS: &far}, false},
		{BlobSource{Type: "other", WorkspaceID: "w", DocID: "d"}, false},
		{BlobSource{Type: "currentDoc", WorkspaceID: "w"}, false},
	} {
		if err := validateSource(c.source); (err == nil) != c.ok {
			t.Fatalf("%+v err=%v", c.source, err)
		}
	}
}

// buildDoc creates a document with a page root, an image and an attachment
// reachable from it, and an unreachable image.
func buildDoc(t *testing.T, pageRoots int, avatar string) *ygo.Doc {
	t.Helper()
	doc := ygo.NewDoc()
	blocks := ygo.NewMap(doc, "blocks")
	meta := ygo.NewMap(doc, "meta")
	txn := doc.WriteTxn()
	add := func(id, flavour, source string, children ...string) {
		block := blocks.SetMap(txn, id)
		block.Set(txn, "sys:id", id)
		block.Set(txn, "sys:flavour", flavour)
		if source != "" {
			text := block.SetText(txn, "prop:sourceId")
			if err := text.Insert(txn, 0, source); err != nil {
				t.Fatal(err)
			}
		}
		arr := block.SetArray(txn, "sys:children")
		for _, child := range children {
			arr.Push(txn, child)
		}
	}
	add("page", "affine:page", "", "note")
	add("note", "affine:note", "", "img", "att")
	add("img", "affine:image", "key-b")
	add("att", "affine:attachment", "key-a")
	add("orphan", "affine:image", "key-z")
	for i := 1; i < pageRoots; i++ {
		add("page2", "affine:page", "")
	}
	if avatar != "" {
		meta.Set(txn, "avatar", avatar)
	}
	txn.Commit()
	return doc
}

func TestReferencedKeys(t *testing.T) {
	keys, err := referencedKeys(buildDoc(t, 1, "avatar-key"), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || !keys["key-a"] || !keys["key-b"] {
		t.Fatalf("keys=%v", keys)
	}
	keys, err = referencedKeys(buildDoc(t, 1, "avatar-key"), true)
	if err != nil || !keys["avatar-key"] {
		t.Fatalf("root keys=%v err=%v", keys, err)
	}
	if _, err := referencedKeys(buildDoc(t, 2, ""), false); !errors.Is(err, errManifestProjection) {
		t.Fatalf("two roots err=%v", err)
	}
	empty := ygo.NewDoc()
	meta := ygo.NewMap(empty, "meta")
	txn := empty.WriteTxn()
	meta.Set(txn, "avatar", "only")
	txn.Commit()
	keys, err = referencedKeys(empty, true)
	if err != nil || len(keys) != 1 || !keys["only"] {
		t.Fatalf("empty blocks keys=%v err=%v", keys, err)
	}
}

func TestSourceStampLengthPrefixed(t *testing.T) {
	if sourceStamp([]string{"ab", "c"}) == sourceStamp([]string{"a", "bc"}) {
		t.Fatal("stamp is ambiguous")
	}
}
