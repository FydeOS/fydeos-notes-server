// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type RuntimeConfigReader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// ReadRuntimeValue merges a native app_configs leaf over the config.json
// baseline. Object overrides retain unspecified baseline fields; null replaces.
func ReadRuntimeValue(ctx context.Context, reader RuntimeConfigReader, path, key string) (json.RawMessage, bool, error) {
	return readRuntimeValue(ctx, reader, path, key, true)
}

// ReadNodeRuntimeValue reads a Node-owned descriptor. Unlike Native descriptors,
// a database object replaces the entire file value, including omitted fields.
func ReadNodeRuntimeValue(ctx context.Context, reader RuntimeConfigReader, path, key string) (json.RawMessage, bool, error) {
	return readRuntimeValue(ctx, reader, path, key, false)
}

func readRuntimeValue(ctx context.Context, reader RuntimeConfigReader, path, key string, mergeObjects bool) (json.RawMessage, bool, error) {
	baseline, found, err := runtimeFileValue(path, key)
	if err != nil {
		return nil, false, err
	}
	if reader != nil {
		var raw []byte
		err := reader.QueryRow(ctx, `SELECT value FROM app_configs WHERE id=$1`, key).Scan(&raw)
		switch {
		case err == nil:
			var override any
			if err := decodeRuntimeJSON(raw, &override); err != nil {
				return nil, false, fmt.Errorf("invalid runtime configuration %s", key)
			}
			if mergeObjects {
				baseline = mergeRuntimeValue(baseline, override)
			} else {
				baseline = override
			}
			found = true
		case errors.Is(err, pgx.ErrNoRows):
		default:
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "42P01" {
				return nil, false, fmt.Errorf("read runtime configuration %s: %w", key, err)
			}
		}
	}
	if !found {
		return nil, false, nil
	}
	raw, err := json.Marshal(baseline)
	return raw, true, err
}

func ReadWebsocketMaxHttpBufferSize(ctx context.Context, reader RuntimeConfigReader, path string) (int64, error) {
	raw, found, err := ReadRuntimeValue(ctx, reader, path, "websocket.maxHttpBufferSize")
	if err != nil {
		return 0, err
	}
	if !found {
		return DefaultWebsocketMaxHttpBufferSize, nil
	}
	var limit int64
	if err := json.Unmarshal(raw, &limit); err != nil || limit <= 0 {
		return 0, fmt.Errorf("invalid runtime configuration websocket.maxHttpBufferSize")
	}
	return limit, nil
}

func runtimeFileValue(path, key string) (any, bool, error) {
	if path == "" {
		return nil, false, nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read runtime configuration: %w", err)
	}
	root, err := decodeRuntimeDocument(raw)
	if err != nil {
		return nil, false, err
	}
	return runtimeDocumentValue(root, key)
}

func decodeRuntimeDocument(raw []byte) (map[string]any, error) {
	var root map[string]any
	if err := decodeRuntimeJSON(raw, &root); err != nil || root == nil {
		return nil, errors.New("invalid runtime configuration document")
	}
	for name, value := range root {
		module, ok := value.(map[string]any)
		if !ok {
			continue
		}
		keys := make([]string, 0, len(module))
		for key := range module {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		expanded := map[string]any{}
		for _, key := range keys {
			parts := strings.Split(key, ".")
			target := expanded
			for _, part := range parts[:len(parts)-1] {
				next, ok := target[part].(map[string]any)
				if !ok {
					next = map[string]any{}
					target[part] = next
				}
				target = next
			}
			target[parts[len(parts)-1]] = module[key]
		}
		root[name] = expanded
	}
	return root, nil
}

func runtimeDocumentValue(root map[string]any, key string) (any, bool, error) {
	var value any = root
	for _, part := range strings.Split(key, ".") {
		if value == nil {
			return nil, false, nil
		}
		object, ok := value.(map[string]any)
		if !ok {
			return nil, false, errors.New("invalid runtime configuration path")
		}
		value, ok = object[part]
		if !ok {
			return nil, false, nil
		}
	}
	return value, true, nil
}

func decodeRuntimeJSON(raw []byte, target any) error {
	if !json.Valid(raw) {
		return errors.New("invalid JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	return decoder.Decode(target)
}

func mergeRuntimeValue(base, override any) any {
	a, aOK := base.(map[string]any)
	b, bOK := override.(map[string]any)
	if !aOK || !bOK {
		return override
	}
	for key, value := range b {
		a[key] = mergeRuntimeValue(a[key], value)
	}
	return a
}
