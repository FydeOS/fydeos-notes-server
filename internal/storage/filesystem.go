// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package storage

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type filesystemBackend struct {
	root   string
	bucket string
}

func (b *filesystemBackend) DeletePrefix(_ context.Context, prefix string) (int64, error) {
	if err := validatePrefix(prefix); err != nil {
		return 0, err
	}
	root := filepath.Join(b.root, b.bucket)
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return 0, errors.New("filesystem storage bucket is not a directory")
	}
	var deleted int64
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root || entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(relative)
		if strings.HasSuffix(key, ".metadata.json") || !strings.HasPrefix(key, prefix) {
			return nil
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.Remove(path + ".metadata.json"); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		deleted++
		return nil
	})
	return deleted, err
}
