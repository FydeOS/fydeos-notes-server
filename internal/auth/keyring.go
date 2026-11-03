// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const signingKeysConfigKey = "auth.session.signingKeys"

// NativeSessionDB is the database surface used by native sessions and the
// signing keyring.
type NativeSessionDB interface {
	RowQuerier
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Begin(context.Context) (pgx.Tx, error)
}

type beginner interface {
	Begin(context.Context) (pgx.Tx, error)
}

type execer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

var errNoTransactions = errors.New("auth: database does not support transactions")

func rollback(tx pgx.Tx) { _ = tx.Rollback(context.Background()) }

// readConfigInt reads an integer runtime value (file baseline, DB override).
func readConfigInt(ctx context.Context, db RowQuerier, cfg config.Config, key string, def int64) (int64, error) {
	raw, found, err := config.ReadRuntimeValue(ctx, db, cfg.RuntimeConfigPath, key)
	if err != nil {
		return 0, err
	}
	if !found {
		return def, nil
	}
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil || value != math.Trunc(value) || math.IsInf(value, 0) {
		return 0, fmt.Errorf("invalid %s", key)
	}
	return int64(value), nil
}

func readConfigBool(ctx context.Context, db RowQuerier, cfg config.Config, key string, def bool) (bool, error) {
	raw, found, err := config.ReadRuntimeValue(ctx, db, cfg.RuntimeConfigPath, key)
	if err != nil {
		return false, err
	}
	if !found {
		return def, nil
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, fmt.Errorf("invalid %s", key)
	}
	return value, nil
}

type tokenPolicy struct {
	access, idle, absolute, grace time.Duration
}

func loadTokenPolicy(ctx context.Context, db RowQuerier, cfg config.Config) (tokenPolicy, error) {
	const day = 86400
	specs := []struct {
		key           string
		def, min, max int64
	}{
		{"auth.token.accessTokenTtl", 900, 60, 3600},
		{"auth.token.refreshIdleTtl", 30 * day, 3600, 365 * day},
		{"auth.token.refreshAbsoluteTtl", 180 * day, 3600, 730 * day},
		{"auth.token.refreshGracePeriod", 30, 0, 60},
	}
	var values [4]time.Duration
	for i, s := range specs {
		v, err := readConfigInt(ctx, db, cfg, s.key, s.def)
		if err != nil {
			return tokenPolicy{}, err
		}
		if v < s.min || v > s.max {
			return tokenPolicy{}, fmt.Errorf("%s out of range", s.key)
		}
		values[i] = time.Duration(v) * time.Second
	}
	return tokenPolicy{access: values[0], idle: values[1], absolute: values[2], grace: values[3]}, nil
}

// loadKeyring reads the keyring, creating a fresh one when absent.
func loadKeyring(ctx context.Context, db RowQuerier) (keyring, error) {
	for attempt := 0; attempt < 2; attempt++ {
		var raw []byte
		err := db.QueryRow(ctx, `SELECT value FROM app_configs WHERE id=$1`, signingKeysConfigKey).Scan(&raw)
		if err == nil {
			return parseKeyring(raw)
		}
		if !errors.Is(err, pgx.ErrNoRows) || attempt > 0 {
			return nil, fmt.Errorf("read signing keyring: %w", err)
		}
		encoded, _ := json.Marshal(newAutoKeyring(time.Now()))
		const insert = `INSERT INTO app_configs(id,value,created_at,updated_at) VALUES($1,$2::jsonb,clock_timestamp(),clock_timestamp()) ON CONFLICT (id) DO NOTHING`
		switch d := db.(type) {
		case pgx.Tx:
			_, err = d.Exec(ctx, insert, signingKeysConfigKey, string(encoded))
		case execer:
			_, err = d.Exec(ctx, insert, signingKeysConfigKey, string(encoded))
		case beginner:
			var tx pgx.Tx
			if tx, err = d.Begin(ctx); err == nil {
				if _, err = tx.Exec(ctx, insert, signingKeysConfigKey, string(encoded)); err == nil {
					err = tx.Commit(ctx)
				}
				rollback(tx)
			}
		default:
			err = errNoTransactions
		}
		if err != nil {
			return nil, fmt.Errorf("create signing keyring: %w", err)
		}
	}
	return nil, errInvalidKeyring
}

type AuthSigningKeyMetadata struct {
	ID          string     `json:"id"`
	Status      string     `json:"status"`
	Source      string     `json:"source"`
	CreatedAt   *time.Time `json:"createdAt,omitempty"`
	RetiredAt   *time.Time `json:"retiredAt,omitempty"`
	VerifyUntil *time.Time `json:"verifyUntil,omitempty"`
	CanDelete   bool       `json:"canDelete"`
}

type AuthSigningKeyRing struct {
	db  NativeSessionDB
	cfg config.Config
}

func NewAuthSigningKeyRing(db NativeSessionDB, cfg config.Config) *AuthSigningKeyRing {
	return &AuthSigningKeyRing{db: db, cfg: cfg}
}

func keyMetadata(keys keyring, now time.Time) []AuthSigningKeyMetadata {
	out := make([]AuthSigningKeyMetadata, 0, len(keys))
	for _, k := range keys {
		out = append(out, AuthSigningKeyMetadata{
			ID: k.ID, Status: k.Status, Source: k.Source, CreatedAt: k.CreatedAt, RetiredAt: k.RetiredAt, VerifyUntil: k.VerifyUntil,
			CanDelete: k.Status == "retiring" && k.VerifyUntil != nil && !k.VerifyUntil.After(now),
		})
	}
	return out
}

func (s *AuthSigningKeyRing) Metadata(ctx context.Context) ([]AuthSigningKeyMetadata, error) {
	policy, err := loadTokenPolicy(ctx, s.db, s.cfg)
	if err != nil {
		return nil, err
	}
	keys, err := loadKeyring(ctx, s.db)
	if err != nil {
		return nil, err
	}
	if err := keys.validate(policy.access); err != nil {
		return nil, err
	}
	return keyMetadata(keys, time.Now()), nil
}

// mutate runs fn on the locked keyring and persists the result.
func (s *AuthSigningKeyRing) mutate(ctx context.Context, actorID string, fn func(keys keyring, now time.Time, policy tokenPolicy) (keyring, error)) ([]AuthSigningKeyMetadata, error) {
	policy, err := loadTokenPolicy(ctx, s.db, s.cfg)
	if err != nil {
		return nil, err
	}
	if _, err := loadKeyring(ctx, s.db); err != nil {
		return nil, err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer rollback(tx)
	var one int
	if err := tx.QueryRow(ctx, `SELECT 1 FROM users WHERE id=$1 FOR UPDATE`, actorID).Scan(&one); err != nil {
		return nil, fmt.Errorf("lock signing key actor: %w", err)
	}
	var raw []byte
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT value, clock_timestamp() FROM app_configs WHERE id=$1 FOR UPDATE`, signingKeysConfigKey).Scan(&raw, &now); err != nil {
		return nil, fmt.Errorf("lock signing keyring: %w", err)
	}
	keys, err := parseKeyring(raw)
	if err != nil {
		return nil, err
	}
	if err := keys.validate(policy.access); err != nil {
		return nil, err
	}
	keys, err = fn(keys, now, policy)
	if err != nil {
		return nil, err
	}
	if err := keys.validate(policy.access); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(keys)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE app_configs SET value=$2::jsonb, updated_at=clock_timestamp(), last_updated_by=$3 WHERE id=$1`, signingKeysConfigKey, string(encoded), actorID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return keyMetadata(keys, now), nil
}

func (s *AuthSigningKeyRing) Rotate(ctx context.Context, actorID, expectedActiveKeyID string) ([]AuthSigningKeyMetadata, error) {
	return s.mutate(ctx, actorID, func(keys keyring, now time.Time, policy tokenPolicy) (keyring, error) {
		if keys.active().ID != expectedActiveKeyID {
			return nil, ErrSigningKeyChanged
		}
		now = now.UTC()
		verifyUntil := now.Add(policy.access + clockSkew)
		next := make(keyring, 0, len(keys)+1)
		for _, k := range keys {
			if k.Status == "active" {
				retired := now
				k.Status, k.RetiredAt, k.VerifyUntil = "retiring", &retired, &verifyUntil
			}
			next = append(next, k)
		}
		created := now
		next = append(next, signingKey{
			ID:     fmt.Sprintf("auth-%d-%s", now.UnixMilli(), randomB64(9)),
			Secret: randomB64(32), Status: "active", CreatedAt: &created, Source: "admin",
		})
		return next, nil
	})
}

func (s *AuthSigningKeyRing) Delete(ctx context.Context, actorID, keyID string) ([]AuthSigningKeyMetadata, error) {
	return s.mutate(ctx, actorID, func(keys keyring, now time.Time, _ tokenPolicy) (keyring, error) {
		k, ok := keys.find(keyID)
		if !ok {
			return nil, ErrSigningKeyNotFound
		}
		if k.Status != "retiring" || k.VerifyUntil == nil || k.VerifyUntil.After(now) {
			return nil, ErrSigningKeyCannotDelete
		}
		next := make(keyring, 0, len(keys))
		for _, existing := range keys {
			if existing.ID != keyID {
				next = append(next, existing)
			}
		}
		return next, nil
	})
}
