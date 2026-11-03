// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type filesystemMetadata struct {
	ContentType   string  `json:"contentType"`
	ContentLength int64   `json:"contentLength"`
	LastModified  int64   `json:"lastModified"`
	ChecksumCRC32 *string `json:"checksumCRC32"`
}

func (b *filesystemBackend) PutObject(_ context.Context, key string, body io.Reader, options PutOptions) (ObjectMetadata, error) {
	if body == nil {
		return ObjectMetadata{}, errors.New("object body is required")
	}
	path, err := b.objectPath(key, true)
	if err != nil {
		return ObjectMetadata{}, err
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return ObjectMetadata{}, fmt.Errorf("read filesystem object body: %w", err)
	}
	if options.ContentLength != nil && *options.ContentLength != int64(len(data)) {
		return ObjectMetadata{}, errors.New("filesystem object content length mismatch")
	}
	checksum := checksumCRC32(data)
	if options.ChecksumCRC32 != "" && options.ChecksumCRC32 != checksum {
		return ObjectMetadata{}, errors.New("filesystem object checksum mismatch")
	}
	contentType := objectContentType(options.ContentType, data)
	lastModified := time.Now().UTC().Truncate(time.Millisecond)
	metadata := ObjectMetadata{
		ContentType: contentType, ContentLength: int64(len(data)), LastModified: lastModified, ChecksumCRC32: checksum,
	}
	checksumValue := checksum
	metadataJSON, err := json.Marshal(filesystemMetadata{
		ContentType: contentType, ContentLength: int64(len(data)), LastModified: lastModified.UnixMilli(), ChecksumCRC32: &checksumValue,
	})
	if err != nil {
		return ObjectMetadata{}, fmt.Errorf("encode filesystem object metadata: %w", err)
	}
	if err := writeFileAtomic(path, data); err != nil {
		return ObjectMetadata{}, fmt.Errorf("write filesystem object: %w", err)
	}
	if err := writeFileAtomic(path+".metadata.json", metadataJSON); err != nil {
		return ObjectMetadata{}, fmt.Errorf("write filesystem object metadata: %w", err)
	}
	return metadata, nil
}

func (b *filesystemBackend) GetObject(_ context.Context, key string) (io.ReadCloser, ObjectMetadata, error) {
	path, err := b.objectPath(key, false)
	if err != nil {
		return nil, ObjectMetadata{}, err
	}
	metadata, err := b.readMetadata(path)
	if err != nil {
		return nil, ObjectMetadata{}, err
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ObjectMetadata{}, osNotExistError(key)
	}
	if err != nil {
		return nil, ObjectMetadata{}, fmt.Errorf("open filesystem object: %w", err)
	}
	return file, metadata, nil
}

func (b *filesystemBackend) GetObjectRange(_ context.Context, key string, offset, length int64) ([]byte, error) {
	if err := validateObjectReadRange(offset, length); err != nil {
		return nil, err
	}
	path, err := b.objectPath(key, false)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, osNotExistError(key)
	}
	if err != nil {
		return nil, fmt.Errorf("open filesystem object range: %w", err)
	}
	defer file.Close()
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek filesystem object range: %w", err)
	}
	body := make([]byte, int(length))
	if _, err := io.ReadFull(file, body); err != nil {
		return nil, fmt.Errorf("read filesystem object range: %w", err)
	}
	return body, nil
}

func (b *filesystemBackend) HeadObject(_ context.Context, key string) (*ObjectMetadata, error) {
	path, err := b.objectPath(key, false)
	if err != nil {
		return nil, err
	}
	metadata, err := b.readMetadata(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &metadata, nil
}

func (b *filesystemBackend) DeleteObject(_ context.Context, key string) error {
	path, err := b.objectPath(key, false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var firstErr error
	for _, target := range []string{path, path + ".metadata.json"} {
		if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return fmt.Errorf("delete filesystem object: %w", firstErr)
	}
	return nil
}

func (b *filesystemBackend) PresignPut(context.Context, string, PutOptions, time.Duration) (PresignedRequest, error) {
	return PresignedRequest{}, ErrOperationUnsupported
}

func (b *filesystemBackend) PresignGet(context.Context, string, time.Duration) (PresignedRequest, error) {
	return PresignedRequest{}, ErrOperationUnsupported
}

func (b *filesystemBackend) CreateMultipartUpload(context.Context, string, PutOptions, time.Duration) (MultipartUpload, error) {
	return MultipartUpload{}, ErrOperationUnsupported
}

func (b *filesystemBackend) PresignUploadPart(context.Context, string, string, int32, int64, time.Duration) (PresignedRequest, error) {
	return PresignedRequest{}, ErrOperationUnsupported
}

func (b *filesystemBackend) UploadPart(context.Context, string, string, int32, io.Reader, int64) (string, error) {
	return "", ErrOperationUnsupported
}

func (b *filesystemBackend) ListMultipartUploadParts(context.Context, string, string) ([]MultipartPart, error) {
	return nil, ErrOperationUnsupported
}

func (b *filesystemBackend) CompleteMultipartUpload(context.Context, string, string, []CompletedPart) error {
	return ErrOperationUnsupported
}

func (b *filesystemBackend) AbortMultipartUpload(context.Context, string, string) error {
	return ErrOperationUnsupported
}

func (b *filesystemBackend) objectPath(key string, createParents bool) (string, error) {
	if err := validateObjectKey(key); err != nil {
		return "", err
	}
	root := filepath.Join(b.root, b.bucket)
	if createParents {
		if err := os.MkdirAll(root, 0o700); err != nil {
			return "", fmt.Errorf("create filesystem bucket: %w", err)
		}
	}
	if err := ensureDirectory(root); err != nil {
		return "", err
	}
	segments := strings.Split(key, "/")
	current := root
	for _, segment := range segments[:len(segments)-1] {
		current = filepath.Join(current, segment)
		if createParents {
			if err := os.Mkdir(current, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return "", fmt.Errorf("create filesystem object directory: %w", err)
			}
		}
		if err := ensureDirectory(current); err != nil {
			return "", err
		}
	}
	path := filepath.Join(current, segments[len(segments)-1])
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || info.IsDir() {
			return "", errors.New("filesystem object path is not a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if info, err := os.Lstat(path + ".metadata.json"); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || info.IsDir() {
			return "", errors.New("filesystem object metadata path is not a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return path, nil
}

func (b *filesystemBackend) readMetadata(path string) (ObjectMetadata, error) {
	data, err := os.ReadFile(path + ".metadata.json")
	if errors.Is(err, os.ErrNotExist) {
		return ObjectMetadata{}, os.ErrNotExist
	}
	if err != nil {
		return ObjectMetadata{}, fmt.Errorf("read filesystem object metadata: %w", err)
	}
	var metadata filesystemMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return ObjectMetadata{}, fmt.Errorf("parse filesystem object metadata: %w", err)
	}
	return ObjectMetadata{
		ContentType: defaultContentType(metadata.ContentType), ContentLength: metadata.ContentLength,
		LastModified: time.UnixMilli(metadata.LastModified).UTC(), ChecksumCRC32: valueString(metadata.ChecksumCRC32),
	}, nil
}

func ensureDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("filesystem storage path is not a directory")
	}
	return nil
}

func writeFileAtomic(path string, body []byte) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".notes-object-*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(body); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func valueString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
