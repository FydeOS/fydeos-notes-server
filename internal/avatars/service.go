// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package avatars

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/disintegration/imaging"
	"github.com/fydeos/fydeos-notes-server/internal/storage"
	"github.com/fydeos/fydeos-notes-server/internal/users"
	"github.com/gen2brain/webp"
	_ "golang.org/x/image/bmp"
)

const (
	MaxUploadBytes = 5 << 20
	MaxDimension   = 16384
	MaxPixels      = 40_000_000
	MaxAvatarEdge  = 512
)

var (
	ErrUploadTooLarge        = errors.New("avatar upload is too large")
	ErrAvatarOperationFailed = errors.New("avatar operation failed")
	avatarConversionSlots    = make(chan struct{}, 1)
)

type FormatError struct {
	Format string
}

func (e *FormatError) Error() string {
	return "Image format not supported: " + e.Format
}

type UserRepository interface {
	Get(context.Context, string) (*users.User, error)
	UpdateAvatar(context.Context, string, *string) (*users.User, error)
}

type ObjectStore interface {
	PutObject(context.Context, string, string, io.Reader, storage.PutOptions) (storage.ObjectMetadata, error)
	GetObject(context.Context, string, string) (io.ReadCloser, storage.ObjectMetadata, error)
	DeleteObject(context.Context, string, string) error
	Provider(string) string
}

type Service struct {
	users      UserRepository
	objects    ObjectStore
	publicPath string
	baseURL    string
	now        func() time.Time
}

func NewService(repository UserRepository, objects ObjectStore, publicPath, baseURL string) *Service {
	if publicPath == "" {
		publicPath = "/api/avatars/"
	}
	return &Service{
		users: repository, objects: objects, publicPath: publicPath, baseURL: strings.TrimRight(baseURL, "/"), now: time.Now,
	}
}

func (s *Service) Upload(ctx context.Context, userID, declaredContentType string, source io.Reader) (*users.User, error) {
	if source == nil {
		return nil, &FormatError{Format: "unknown"}
	}
	converted, err := readAndConvert(ctx, declaredContentType, source)
	if err != nil {
		return nil, err
	}

	user, err := s.users.Get(ctx, userID)
	if err != nil {
		if errors.Is(err, users.ErrNotFound) {
			return nil, err
		}
		return nil, ErrAvatarOperationFailed
	}
	key := fmt.Sprintf("%s-avatar-%d", userID, s.now().UnixMilli())
	length := int64(len(converted))
	if _, err := s.objects.PutObject(ctx, storage.AvatarScope, key, bytes.NewReader(converted), storage.PutOptions{
		ContentType: "image/webp", ContentLength: &length,
	}); err != nil {
		return nil, ErrAvatarOperationFailed
	}
	avatarURL := s.avatarURL(key)
	if user.AvatarURL != nil {
		if err := s.objects.DeleteObject(ctx, storage.AvatarScope, objectKey(*user.AvatarURL)); err != nil {
			return nil, ErrAvatarOperationFailed
		}
	}
	updated, err := s.users.UpdateAvatar(ctx, userID, &avatarURL)
	if err != nil {
		if errors.Is(err, users.ErrNotFound) {
			return nil, err
		}
		return nil, ErrAvatarOperationFailed
	}
	return updated, nil
}

func readAndConvert(ctx context.Context, declaredContentType string, source io.Reader) ([]byte, error) {
	release, err := acquireConversionSlot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	raw, err := io.ReadAll(io.LimitReader(source, MaxUploadBytes+1))
	if err != nil {
		return nil, ErrAvatarOperationFailed
	}
	if len(raw) > MaxUploadBytes {
		return nil, ErrUploadTooLarge
	}
	format := sniffFormat(raw, declaredContentType)
	if !strings.HasPrefix(format, "image/") {
		return nil, &FormatError{Format: format}
	}
	converted, err := convert(raw)
	if err != nil {
		return nil, &FormatError{Format: format}
	}
	return converted, nil
}

func acquireConversionSlot(ctx context.Context) (func(), error) {
	select {
	case avatarConversionSlots <- struct{}{}:
		return func() { <-avatarConversionSlots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Service) Get(ctx context.Context, key string) (io.ReadCloser, storage.ObjectMetadata, error) {
	return s.objects.GetObject(ctx, storage.AvatarScope, key)
}

func (s *Service) DeleteByURL(ctx context.Context, link string) error {
	if s == nil || s.objects == nil {
		return ErrAvatarOperationFailed
	}
	key := objectKey(link)
	if key == "." || key == "" {
		return nil
	}
	return s.objects.DeleteObject(ctx, storage.AvatarScope, key)
}

func (s *Service) PublicStorage() bool {
	provider := s.objects.Provider(storage.AvatarScope)
	return provider == "fs" || provider == "assetpack"
}

func (s *Service) avatarURL(key string) string {
	link := strings.TrimRight(s.publicPath, "/") + "/" + key
	if strings.HasPrefix(link, "/") {
		return s.baseURL + link
	}
	return link
}

func objectKey(link string) string {
	parsed, err := url.Parse(link)
	if err == nil {
		link = parsed.Path
	}
	return path.Base(strings.TrimRight(link, "/"))
}

func sniffFormat(data []byte, declared string) string {
	if len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP" {
		return "image/webp"
	}
	if len(data) >= 2 && data[0] == 'B' && data[1] == 'M' {
		return "image/bmp"
	}
	if len(data) >= 8 && bytes.Equal(data[:8], []byte{137, 80, 78, 71, 13, 10, 26, 10}) {
		return "image/png"
	}
	if len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff {
		return "image/jpeg"
	}
	if len(data) >= 6 && (string(data[:6]) == "GIF87a" || string(data[:6]) == "GIF89a") {
		return "image/gif"
	}
	detected := http.DetectContentType(data[:min(len(data), 512)])
	mediaType, _, err := mime.ParseMediaType(detected)
	if err == nil && strings.HasPrefix(strings.ToLower(mediaType), "image/") {
		return strings.ToLower(mediaType)
	}
	declaredType, _, err := mime.ParseMediaType(declared)
	if err == nil && strings.HasPrefix(strings.ToLower(declaredType), "image/") {
		return strings.ToLower(declaredType)
	}
	return "unknown"
}

func convert(data []byte) ([]byte, error) {
	config, detectedFormat, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width <= 0 || config.Height <= 0 || config.Width > MaxDimension || config.Height > MaxDimension || int64(config.Width)*int64(config.Height) > MaxPixels {
		return nil, errors.New("image dimensions are not supported")
	}
	var source image.Image
	if detectedFormat == "webp" {
		source, err = webp.Decode(bytes.NewReader(data), webp.Options{AutoRotate: true})
	} else {
		source, err = imaging.Decode(bytes.NewReader(data), imaging.AutoOrientation(true))
	}
	if err != nil {
		return nil, err
	}
	width, height := source.Bounds().Dx(), source.Bounds().Dy()
	if width > MaxAvatarEdge || height > MaxAvatarEdge {
		scale := float64(MaxAvatarEdge) / float64(max(width, height))
		width, height = max(1, int(float64(width)*scale)), max(1, int(float64(height)*scale))
		source = imaging.Resize(source, width, height, imaging.Lanczos)
	}
	var output bytes.Buffer
	if err := webp.Encode(&output, source, webp.Options{Quality: 80, Method: 4}); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}
