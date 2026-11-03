// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package avatars

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/storage"
	"github.com/fydeos/fydeos-notes-server/internal/users"
	"github.com/gen2brain/webp"
	"golang.org/x/image/bmp"
)

func TestUploadConvertsAndReplacesAvatar(t *testing.T) {
	userStore := &userStoreFake{user: &users.User{ID: "user-1", AvatarURL: ptr("https://notes.example.test/api/avatars/old-avatar.webp")}}
	objects := &objectStoreFake{provider: "fs", items: map[string]objectFake{
		"old-avatar.webp": {data: []byte("old"), metadata: storage.ObjectMetadata{ContentType: "image/webp", ContentLength: 3}},
	}}
	service := NewService(userStore, objects, "/api/avatars/", "https://notes.example.test/notes")
	service.now = func() time.Time { return time.UnixMilli(1234) }

	user, err := service.Upload(context.Background(), "user-1", "application/octet-stream", pngImage(t, 1024, 512))
	if err != nil {
		t.Fatal(err)
	}
	wantURL := "https://notes.example.test/notes/api/avatars/user-1-avatar-1234"
	if user.AvatarURL == nil || *user.AvatarURL != wantURL {
		t.Fatalf("avatar URL = %v, want %q", user.AvatarURL, wantURL)
	}
	if _, exists := objects.items["old-avatar.webp"]; exists {
		t.Fatal("old avatar object still exists")
	}
	key := "user-1-avatar-1234"
	stored, exists := objects.items[key]
	if !exists || stored.metadata.ContentType != "image/webp" || len(stored.data) < 12 || string(stored.data[:4]) != "RIFF" || string(stored.data[8:12]) != "WEBP" {
		t.Fatalf("stored avatar = %#v, want WebP object", stored)
	}
	decoded, err := webp.Decode(bytes.NewReader(stored.data))
	if err != nil {
		t.Fatalf("decode stored WebP: %v", err)
	}
	if decoded.Bounds().Dx() != MaxAvatarEdge || decoded.Bounds().Dy() != MaxAvatarEdge/2 {
		t.Fatalf("stored dimensions = %dx%d, want %dx%d", decoded.Bounds().Dx(), decoded.Bounds().Dy(), MaxAvatarEdge, MaxAvatarEdge/2)
	}
}

func TestUploadRejectsUnsupportedAndOversizedFiles(t *testing.T) {
	service := NewService(&userStoreFake{user: &users.User{ID: "user-1"}}, &objectStoreFake{items: map[string]objectFake{}}, "", "")
	_, err := service.Upload(context.Background(), "user-1", "image/svg+xml", strings.NewReader(`<svg xmlns="http://www.w3.org/2000/svg"/>`))
	var formatError *FormatError
	if !errors.As(err, &formatError) || formatError.Format != "image/svg+xml" || formatError.Error() != "Image format not supported: image/svg+xml" {
		t.Fatalf("SVG error = %v, want image/svg+xml format error", err)
	}
	_, err = service.Upload(context.Background(), "user-1", "image/png", bytes.NewReader(make([]byte, MaxUploadBytes+1)))
	if !errors.Is(err, ErrUploadTooLarge) {
		t.Fatalf("oversized error = %v, want ErrUploadTooLarge", err)
	}
}

func TestAvatarConversionsAreSerializedAndQueuedRequestsCanCancel(t *testing.T) {
	firstRelease, err := acquireConversionSlot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	queued := make(chan func(), 1)
	queueErr := make(chan error, 1)
	go func() {
		release, err := acquireConversionSlot(context.Background())
		if err != nil {
			queueErr <- err
			return
		}
		queued <- release
	}()

	select {
	case release := <-queued:
		release()
		firstRelease()
		t.Fatal("queued conversion acquired the only slot while it was occupied")
	case err := <-queueErr:
		firstRelease()
		t.Fatalf("queued conversion failed: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	firstRelease()

	select {
	case release := <-queued:
		release()
	case <-time.After(time.Second):
		t.Fatal("queued conversion did not start after slot became available")
	}

	blockRelease, err := acquireConversionSlot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancel()
	_, err = acquireConversionSlot(ctx)
	blockRelease()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("queued conversion error = %v, want context.Canceled", err)
	}
}

func TestUploadHidesStorageErrorDetails(t *testing.T) {
	objects := &objectStoreFake{provider: "fs", items: map[string]objectFake{}, putErr: errors.New("bucket credentials leaked")}
	service := NewService(&userStoreFake{user: &users.User{ID: "user-1"}}, objects, "", "")
	_, err := service.Upload(context.Background(), "user-1", "image/png", pngImage(t, 1, 1))
	if !errors.Is(err, ErrAvatarOperationFailed) || strings.Contains(err.Error(), "credentials") {
		t.Fatalf("avatar storage error = %v, want sanitized operation error", err)
	}
}

func TestUploadHidesSourceReadErrorDetails(t *testing.T) {
	service := NewService(&userStoreFake{user: &users.User{ID: "user-1"}}, &objectStoreFake{provider: "fs", items: map[string]objectFake{}}, "", "")
	_, err := service.Upload(context.Background(), "user-1", "image/png", failingReader{})
	if !errors.Is(err, ErrAvatarOperationFailed) || strings.Contains(err.Error(), "/tmp/private") {
		t.Fatalf("avatar read error = %v, want sanitized operation error", err)
	}
}

func TestUploadAcceptsLargeBMPAndConvertsToWebP(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 1024, 1024))
	for y := 0; y < 1024; y++ {
		for x := 0; x < 1024; x++ {
			img.SetRGBA(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 80, A: 255})
		}
	}
	var input bytes.Buffer
	if err := bmp.Encode(&input, img); err != nil {
		t.Fatal(err)
	}
	if input.Len() < 500<<10 || input.Len() > MaxUploadBytes {
		t.Fatalf("BMP size = %d, want between 500 KiB and 5 MiB", input.Len())
	}
	objects := &objectStoreFake{provider: "fs", items: map[string]objectFake{}}
	service := NewService(&userStoreFake{user: &users.User{ID: "user-1"}}, objects, "", "")
	service.now = func() time.Time { return time.UnixMilli(42) }
	if _, err := service.Upload(context.Background(), "user-1", "image/bmp", bytes.NewReader(input.Bytes())); err != nil {
		t.Fatal(err)
	}
	if got := objects.items["user-1-avatar-"+fmt.Sprint(42)].metadata.ContentType; got != "image/webp" {
		t.Fatalf("stored BMP conversion content type = %q, want image/webp", got)
	}
}

func TestPublicHandlerServesLocalAvatarAndRejectsRemoteStorage(t *testing.T) {
	objects := &objectStoreFake{provider: "fs", items: map[string]objectFake{
		"avatar.webp": {data: []byte("RIFF-webp"), metadata: storage.ObjectMetadata{
			ContentType: "image/webp", ContentLength: 9, LastModified: time.Date(2026, 10, 17, 1, 2, 3, 456000000, time.UTC),
		}},
	}}
	handler := NewPublicHandler(NewService(&userStoreFake{}, objects, "", ""))
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/avatars/avatar.webp", nil)
	request.SetPathValue("id", "avatar.webp")
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "RIFF-webp" {
		t.Fatalf("local avatar response = %d %q", response.Code, response.Body.String())
	}
	if response.Header().Get("Content-Type") != "image/webp" || response.Header().Get("Content-Length") != "9" || response.Header().Get("Last-Modified") != "2026-10-17T01:02:03.456Z" || response.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("avatar response headers = %#v", response.Header())
	}

	objects.provider = "aws-s3"
	for _, test := range []struct {
		path string
		want int
	}{{"/api/avatars/avatar.webp", http.StatusForbidden}, {"/api/avatars/missing.webp", http.StatusForbidden}} {
		response = httptest.NewRecorder()
		request = httptest.NewRequest(http.MethodGet, test.path, nil)
		request.SetPathValue("id", strings.TrimPrefix(test.path, "/api/avatars/"))
		handler.ServeHTTP(response, request)
		if response.Code != test.want {
			t.Errorf("remote storage response = %d, want %d", response.Code, test.want)
		}
	}
}

func TestPublicHandlerReportsMissingAvatar(t *testing.T) {
	handler := NewPublicHandler(NewService(&userStoreFake{}, &objectStoreFake{provider: "fs", items: map[string]objectFake{}}, "", ""))
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/avatars/missing.webp", nil)
	request.SetPathValue("id", "missing.webp")
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), "USER_AVATAR_NOT_FOUND") {
		t.Fatalf("missing avatar response = %d %s", response.Code, response.Body.String())
	}
}

func pngImage(t *testing.T, width, height int) io.Reader {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetRGBA(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 120, A: 255})
		}
	}
	var output bytes.Buffer
	if err := png.Encode(&output, img); err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(output.Bytes())
}

type userStoreFake struct {
	user *users.User
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) {
	return 0, errors.New("read /tmp/private-upload failed")
}

func (f *userStoreFake) Get(_ context.Context, id string) (*users.User, error) {
	if f.user == nil || f.user.ID != id {
		return nil, users.ErrNotFound
	}
	copy := *f.user
	return &copy, nil
}

func (f *userStoreFake) UpdateAvatar(_ context.Context, id string, avatarURL *string) (*users.User, error) {
	if f.user == nil || f.user.ID != id {
		return nil, users.ErrNotFound
	}
	f.user.AvatarURL = avatarURL
	copy := *f.user
	return &copy, nil
}

type objectFake struct {
	data     []byte
	metadata storage.ObjectMetadata
}

type objectStoreFake struct {
	provider string
	items    map[string]objectFake
	putErr   error
}

func (f *objectStoreFake) PutObject(_ context.Context, scope, key string, source io.Reader, options storage.PutOptions) (storage.ObjectMetadata, error) {
	if f.putErr != nil {
		return storage.ObjectMetadata{}, f.putErr
	}
	if scope != storage.AvatarScope {
		return storage.ObjectMetadata{}, errors.New("unexpected storage scope")
	}
	data, err := io.ReadAll(source)
	if err != nil {
		return storage.ObjectMetadata{}, err
	}
	metadata := storage.ObjectMetadata{ContentType: options.ContentType, ContentLength: int64(len(data))}
	f.items[key] = objectFake{data: data, metadata: metadata}
	return metadata, nil
}

func (f *objectStoreFake) GetObject(_ context.Context, scope, key string) (io.ReadCloser, storage.ObjectMetadata, error) {
	if scope != storage.AvatarScope {
		return nil, storage.ObjectMetadata{}, errors.New("unexpected storage scope")
	}
	item, exists := f.items[key]
	if !exists {
		return nil, storage.ObjectMetadata{}, fs.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(item.data)), item.metadata, nil
}

func (f *objectStoreFake) DeleteObject(_ context.Context, scope, key string) error {
	if scope != storage.AvatarScope {
		return errors.New("unexpected storage scope")
	}
	delete(f.items, key)
	return nil
}

func (f *objectStoreFake) Provider(scope string) string {
	if scope != storage.AvatarScope {
		return ""
	}
	return f.provider
}

func ptr(value string) *string { return &value }
