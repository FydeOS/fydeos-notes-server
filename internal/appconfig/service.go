// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package appconfig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var ErrUnavailable = errors.New("app config service is unavailable")

type InvalidInputError struct {
	Message string
}

func (e *InvalidInputError) Error() string { return e.Message }

type Update struct {
	Module   string
	Key      string
	Value    any
	HasValue bool
	Clear    bool
}

type Validation struct {
	Module string
	Key    string
	Value  any
	Valid  bool
	Error  *string
}

type Snapshot struct {
	Values   map[string]any
	Metadata map[string]any
}

type Service struct {
	db   Database
	path string
}

type Database interface {
	config.RuntimeConfigReader
	Begin(context.Context) (pgx.Tx, error)
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

type descriptor struct {
	key          string
	owner        owner
	defaultValue any
}

type owner uint8

const (
	native owner = iota
	node
)

var descriptors = []descriptor{
	{key: "allowSignup", owner: native, defaultValue: true},
	{key: "allowSignupForOauth", owner: native, defaultValue: true},
	{key: "requireEmailDomainVerification", owner: native, defaultValue: false},
	{key: "newAccountActionDelay", owner: native, defaultValue: int64(86400)},
	{key: "session.ttl", owner: native, defaultValue: int64(1296000)},
	{key: "session.ttr", owner: native, defaultValue: int64(604800)},
	{key: "token.accessTokenTtl", owner: native, defaultValue: int64(900)},
	{key: "token.refreshIdleTtl", owner: native, defaultValue: int64(2592000)},
	{key: "token.refreshAbsoluteTtl", owner: native, defaultValue: int64(15552000)},
	{key: "token.refreshGracePeriod", owner: native, defaultValue: int64(30)},
	{key: "token.refreshRetention", owner: native, defaultValue: int64(2592000)},
	{key: "trustedCloudflareHeaders", owner: node, defaultValue: false},
	{key: "signInRateLimit", owner: node, defaultValue: map[string]any{"ttl": int64(60000), "ipLimit": int64(20), "emailLimit": int64(5)}},
	{key: "passwordRequirements", owner: node, defaultValue: map[string]any{"min": float64(8), "max": float64(32)}},
}

var secretKeys = map[string]struct{}{
	"calendar.google": {}, "captcha.config": {}, "copilot.exa": {}, "copilot.unsplash": {},
	"mailer.fallbackSMTP.password": {}, "mailer.SMTP.password": {}, "telemetry.ga4.apiSecret": {},
	"copilot.providers.profiles": {}, "copilot.storage": {}, "crypto.privateKey": {}, "db.datasourceUrl": {},
	"indexer.provider.apiKey": {}, "indexer.provider.password": {}, "oauth.providers.apple": {},
	"oauth.providers.fydeos": {}, "oauth.providers.github": {}, "oauth.providers.google": {}, "oauth.providers.oidc": {},
	"payment.revenuecat": {}, "payment.stripe": {}, "redis.ioredis": {}, "redis.password": {},
	"storages.avatar.storage": {}, "storages.blob.storage": {},
}

func New(db Database, path string) *Service {
	return &Service{db: db, path: path}
}

func (s *Service) Read(ctx context.Context) (Snapshot, error) {
	if s == nil || s.db == nil {
		return Snapshot{}, ErrUnavailable
	}
	values := map[string]any{}
	metadata := map[string]any{}
	moduleMetadata := map[string]any{}
	for _, item := range descriptors {
		key := "auth." + item.key
		base, foundInFile, err := s.readValue(ctx, nil, item, key)
		if err != nil {
			return Snapshot{}, err
		}
		value := item.defaultValue
		if foundInFile {
			value = base
		}
		effective, found, err := s.readValue(ctx, s.db, item, key)
		if err != nil {
			return Snapshot{}, err
		}
		source := "default"
		if foundInFile {
			source = "file"
		}
		var exists bool
		if err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM app_configs WHERE id=$1)`, key).Scan(&exists); err != nil {
			return Snapshot{}, fmt.Errorf("%w: read config source", ErrUnavailable)
		}
		if exists {
			source = "database"
			value = effective
		} else if found {
			value = effective
		}
		setPath(values, item.key, value)
		setPath(moduleMetadata, item.key, map[string]any{"source": source, "settable": true})
	}
	metadata["auth"] = moduleMetadata
	return Snapshot{Values: map[string]any{"auth": values}, Metadata: metadata}, nil
}

func (s *Service) Validate(updates []Update) []Validation {
	results := make([]Validation, len(updates))
	counts := map[string]int{}
	for _, update := range updates {
		counts[update.Module+"."+update.Key]++
	}
	for i, update := range updates {
		result := Validation{Module: update.Module, Key: update.Key, Valid: true}
		key := update.Module + "." + update.Key
		if _, secret := secretKeys[key]; !secret {
			result.Value = update.Value
		}
		if counts[key] > 1 {
			result.Valid = false
			result.Error = stringPointer("Duplicate config [" + update.Key + "]")
		} else if update.Module != "auth" || descriptorByKey(update.Key) == nil {
			result.Valid = false
			result.Error = stringPointer("Unknown config [" + update.Key + "]")
		} else if !update.Clear {
			if !update.HasValue {
				result.Valid = false
				result.Error = stringPointer("A value is required when setting config")
			} else if hint := validateValue(update.Key, update.Value); hint != "" {
				result.Valid = false
				result.Error = stringPointer(hint)
			}
		}
		results[i] = result
	}
	return results
}

func (s *Service) Update(ctx context.Context, actor string, updates []Update) (map[string]any, error) {
	if s == nil || s.db == nil {
		return nil, ErrUnavailable
	}
	validation := s.Validate(updates)
	var invalid []string
	for _, result := range validation {
		if !result.Valid {
			invalid = append(invalid, *result.Error)
		}
	}
	if len(invalid) != 0 {
		return nil, &InvalidInputError{Message: strings.Join(invalid, "\n")}
	}
	if len(updates) == 0 {
		return map[string]any{}, nil
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: begin config update", ErrUnavailable)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('app-config-paths', 0))`); err != nil {
		return nil, fmt.Errorf("%w: lock config update", ErrUnavailable)
	}
	for _, update := range updates {
		key := "auth." + update.Key
		if update.Clear {
			if _, err := tx.Exec(ctx, `DELETE FROM app_configs WHERE id=$1`, key); err != nil {
				return nil, fmt.Errorf("%w: clear config", ErrUnavailable)
			}
			continue
		}
		raw, err := json.Marshal(update.Value)
		if err != nil {
			return nil, fmt.Errorf("%w: encode config", ErrUnavailable)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO app_configs(id,value,last_updated_by,created_at,updated_at)
			VALUES($1,$2::jsonb,$3,clock_timestamp(),clock_timestamp())
			ON CONFLICT(id) DO UPDATE SET value=excluded.value,last_updated_by=excluded.last_updated_by,updated_at=clock_timestamp()`, key, string(raw), actor); err != nil {
			return nil, fmt.Errorf("%w: save config", ErrUnavailable)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("%w: commit config update", ErrUnavailable)
	}
	snapshot, err := s.Read(ctx)
	if err != nil {
		return nil, err
	}
	result := map[string]any{}
	for _, update := range updates {
		if _, secret := secretKeys["auth."+update.Key]; secret {
			continue
		}
		if value, ok := getPath(snapshot.Values["auth"], update.Key); ok {
			setPath(result, update.Key, value)
		}
	}
	return map[string]any{"auth": result}, nil
}

func (s *Service) readValue(ctx context.Context, reader config.RuntimeConfigReader, item descriptor, key string) (any, bool, error) {
	var (
		raw   json.RawMessage
		found bool
		err   error
	)
	if item.owner == native {
		raw, found, err = config.ReadRuntimeValue(ctx, reader, s.path, key)
	} else {
		raw, found, err = config.ReadNodeRuntimeValue(ctx, reader, s.path, key)
	}
	if err != nil || !found {
		return nil, found, err
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, false, fmt.Errorf("%w: decode config", ErrUnavailable)
	}
	return value, true, nil
}

func descriptorByKey(key string) *descriptor {
	for i := range descriptors {
		if descriptors[i].key == key {
			return &descriptors[i]
		}
	}
	return nil
}

func validateValue(key string, value any) string {
	if key == "trustedCloudflareHeaders" {
		if _, ok := value.(bool); ok {
			return ""
		}
		return "Expected boolean"
	}
	if strings.HasPrefix(key, "token.") || key == "newAccountActionDelay" || strings.HasPrefix(key, "session.") {
		n, ok := integer(value)
		if !ok {
			return "Expected integer"
		}
		minimum, maximum := int64(math.MinInt64), int64(math.MaxInt64)
		switch key {
		case "newAccountActionDelay":
			minimum = 0
		case "token.accessTokenTtl":
			minimum, maximum = 60, 3600
		case "token.refreshIdleTtl", "token.refreshRetention":
			minimum, maximum = 3600, 31536000
		case "token.refreshAbsoluteTtl":
			minimum, maximum = 3600, 63072000
		case "token.refreshGracePeriod":
			minimum, maximum = 0, 60
		}
		if n < minimum || n > maximum {
			return "Value is outside the allowed range"
		}
		return ""
	}
	if key == "allowSignup" || key == "allowSignupForOauth" || key == "requireEmailDomainVerification" {
		if _, ok := value.(bool); ok {
			return ""
		}
		return "Expected boolean"
	}
	if key == "signInRateLimit" {
		object, ok := value.(map[string]any)
		if !ok || len(object) != 3 {
			return "Expected an object with ttl, ipLimit and emailLimit"
		}
		for _, field := range []string{"ttl", "ipLimit", "emailLimit"} {
			n, ok := number(object[field])
			if !ok || n <= 0 || math.Trunc(n) != n {
				return "Expected positive integer values for ttl, ipLimit and emailLimit"
			}
		}
		return ""
	}
	if key == "passwordRequirements" {
		object, ok := value.(map[string]any)
		if !ok || len(object) != 2 {
			return "Expected an object with min and max"
		}
		minimum, minOK := number(object["min"])
		maximum, maxOK := number(object["max"])
		if !minOK || !maxOK || minimum < 1 || maximum > 100 || minimum >= maximum {
			return "Minimum length of password must be less than maximum length"
		}
		return ""
	}
	return "Unknown config [" + key + "]"
}

func integer(value any) (int64, bool) {
	switch number := value.(type) {
	case int:
		return int64(number), true
	case int32:
		return int64(number), true
	case int64:
		return number, true
	case json.Number:
		parsed, err := number.Int64()
		return parsed, err == nil
	case float64:
		limit := math.Ldexp(1, 63)
		if math.Trunc(number) == number && number >= -limit && number < limit {
			return int64(number), true
		}
	}
	return 0, false
}

func number(value any) (float64, bool) {
	switch n := value.(type) {
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		parsed, err := n.Float64()
		return parsed, err == nil
	case float64:
		return n, !math.IsNaN(n) && !math.IsInf(n, 0)
	default:
		return 0, false
	}
}

func setPath(target map[string]any, path string, value any) {
	parts := strings.Split(path, ".")
	current := target
	for _, part := range parts[:len(parts)-1] {
		next, ok := current[part].(map[string]any)
		if !ok {
			next = map[string]any{}
			current[part] = next
		}
		current = next
	}
	current[parts[len(parts)-1]] = value
}

func getPath(value any, path string) (any, bool) {
	current, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	parts := strings.Split(path, ".")
	for i, part := range parts {
		value, exists := current[part]
		if !exists {
			return nil, false
		}
		if i == len(parts)-1 {
			return value, true
		}
		current, ok = value.(map[string]any)
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func stringPointer(value string) *string { return &value }
