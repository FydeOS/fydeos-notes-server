// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package auth

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/jackc/pgx/v5"
)

const (
	privateKeyConfigKey   = "crypto.privateKey"
	envelopePrefix        = "fydeos-private-key:"
	envelopeV1Prefix      = "fydeos-private-key:v1:"
	envelopeInfo          = "FydeOS/Notes/PrivateKeyEnvelope/v1"
	maxEnvelopeEncoded    = 8192
	successorPrefix       = "auth-successor:v1:"
	successorInfo         = "FydeOS-Notes/Auth/RefreshSuccessor/v1"
	successorAADLabel     = "auth-refresh-successor"
	privateKeyBootstrapLk = "notes-private-key-bootstrap"
)

// PrivateKeyWrappingSecret returns the secret used to seal the app private key.
func PrivateKeyWrappingSecret() string {
	if v := os.Getenv("NOTES_PRIVATE_KEY_SECRET"); strings.TrimSpace(v) != "" {
		return v
	}
	return os.Getenv("OAUTH_FYDEOS_CLIENT_SECRET")
}

func envelopeGCM(secret string, salt []byte) (cipher.AEAD, error) {
	key, err := hkdf.Key(sha256.New, []byte(secret), salt, envelopeInfo, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func sealPrivateKey(secret, pemText string) (string, error) {
	if strings.TrimSpace(secret) == "" {
		return "", errors.New("private key wrapping secret is required")
	}
	salt := make([]byte, 32)
	nonce := make([]byte, 12)
	_, _ = rand.Read(salt)
	_, _ = rand.Read(nonce)
	gcm, err := envelopeGCM(secret, salt)
	if err != nil {
		return "", err
	}
	body := append(append(salt, nonce...), gcm.Seal(nil, nonce, []byte(pemText), []byte(envelopeInfo))...)
	return envelopeV1Prefix + b64.EncodeToString(body), nil
}

func openPrivateKey(secret, envelope string) (string, error) {
	if strings.TrimSpace(secret) == "" {
		return "", errors.New("private key wrapping secret is required")
	}
	if !strings.HasPrefix(envelope, envelopeV1Prefix) {
		return "", errors.New("unsupported private key envelope")
	}
	encoded := strings.TrimPrefix(envelope, envelopeV1Prefix)
	if len(encoded) > maxEnvelopeEncoded {
		return "", errors.New("private key envelope too large")
	}
	body, err := b64.DecodeString(encoded)
	if err != nil || len(body) <= 60 {
		return "", errors.New("malformed private key envelope")
	}
	gcm, err := envelopeGCM(secret, body[:32])
	if err != nil {
		return "", err
	}
	plain, err := gcm.Open(nil, body[32:44], body[44:], []byte(envelopeInfo))
	if err != nil {
		return "", errors.New("private key envelope could not be decrypted")
	}
	return string(plain), nil
}

// LoadAppPrivateKey resolves the app private key: DB, bootstrap, then file.
func LoadAppPrivateKey(ctx context.Context, db interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, cfg config.Config, bootstrapPrivateKey string) (string, error) {
	if db != nil {
		var raw []byte
		err := db.QueryRow(ctx, `SELECT value FROM app_configs WHERE id=$1`, privateKeyConfigKey).Scan(&raw)
		switch {
		case err == nil:
			var value string
			if json.Unmarshal(raw, &value) == nil {
				if strings.HasPrefix(value, envelopePrefix) {
					return openPrivateKey(PrivateKeyWrappingSecret(), value)
				}
				if strings.TrimSpace(value) != "" {
					return value, nil
				}
			}
		case errors.Is(err, pgx.ErrNoRows):
		default:
			return "", fmt.Errorf("read app private key: %w", err)
		}
	}
	if strings.TrimSpace(bootstrapPrivateKey) != "" {
		return bootstrapPrivateKey, nil
	}
	if cfg.RuntimeConfigPath != "" {
		raw, found, err := config.ReadRuntimeValue(ctx, nil, cfg.RuntimeConfigPath, privateKeyConfigKey)
		if err != nil {
			return "", err
		}
		var value string
		if found && json.Unmarshal(raw, &value) == nil && strings.TrimSpace(value) != "" {
			return value, nil
		}
	}
	return "", nil
}

// validP256PEM checks for exactly one SEC1 or PKCS#8 P-256 key block.
func validP256PEM(text string) error {
	block, rest := pem.Decode([]byte(text))
	if block == nil || len(bytes.TrimSpace(rest)) != 0 {
		return errors.New("private key must be a single PEM block")
	}
	var key any
	var err error
	switch block.Type {
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(block.Bytes)
	case "PRIVATE KEY":
		key, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	default:
		return errors.New("unsupported private key PEM type")
	}
	if err != nil {
		return fmt.Errorf("parse private key: %w", err)
	}
	ec, ok := key.(*ecdsa.PrivateKey)
	if !ok || ec.Curve != elliptic.P256() {
		return errors.New("private key must be on curve P-256")
	}
	return nil
}

func generateP256PEM() (string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", err
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})), nil
}

// EnsureStablePrivateKey seals (or creates) the app private key in the DB.
func EnsureStablePrivateKey(ctx context.Context, db interface {
	Begin(context.Context) (pgx.Tx, error)
}, secret, bootstrap string) error {
	if strings.TrimSpace(secret) == "" {
		return errors.New("NOTES_PRIVATE_KEY_SECRET is required to seal the app private key")
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, privateKeyBootstrapLk); err != nil {
		return err
	}
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT value FROM app_configs WHERE id=$1 FOR UPDATE`, privateKeyConfigKey).Scan(&raw)
	var pemText string
	switch {
	case err == nil:
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return errors.New("stored app private key is not a string")
		}
		if strings.HasPrefix(value, envelopePrefix) {
			plain, err := openPrivateKey(secret, value)
			if err != nil {
				return err
			}
			if err := validP256PEM(plain); err != nil {
				return err
			}
			return tx.Commit(ctx)
		}
		if err := validP256PEM(value); err != nil {
			return err
		}
		pemText = value
	case errors.Is(err, pgx.ErrNoRows):
		var dependants bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM ai_workspace_byok_configs)
			OR EXISTS(SELECT 1 FROM calendar_accounts WHERE access_token IS NOT NULL OR refresh_token IS NOT NULL)
			OR EXISTS(SELECT 1 FROM auth_refresh_tokens WHERE successor_ciphertext IS NOT NULL)`).Scan(&dependants); err != nil {
			return fmt.Errorf("check encrypted dependants: %w", err)
		}
		if dependants {
			return errors.New("app private key is missing but encrypted data exists")
		}
		if strings.TrimSpace(bootstrap) != "" {
			if err := validP256PEM(bootstrap); err != nil {
				return err
			}
			pemText = bootstrap
		} else if pemText, err = generateP256PEM(); err != nil {
			return err
		}
	default:
		return err
	}
	envelope, err := sealPrivateKey(secret, pemText)
	if err != nil {
		return err
	}
	encoded, _ := json.Marshal(envelope)
	if _, err := tx.Exec(ctx, `INSERT INTO app_configs(id,value,created_at,updated_at) VALUES($1,$2::jsonb,clock_timestamp(),clock_timestamp())
		ON CONFLICT (id) DO UPDATE SET value=EXCLUDED.value, updated_at=EXCLUDED.updated_at`, privateKeyConfigKey, string(encoded)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func successorGCM(privateKey string) (cipher.AEAD, error) {
	key, err := hkdf.Key(sha256.New, []byte(privateKey), make([]byte, 32), successorInfo, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func successorAAD(sourceID, sessionID string) []byte {
	return []byte(successorAADLabel + "\x00" + sourceID + "\x00" + sessionID)
}

func sealSuccessor(privateKey, token, sourceID, sessionID string) (string, error) {
	gcm, err := successorGCM(privateKey)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, 12)
	_, _ = rand.Read(nonce)
	return successorPrefix + b64.EncodeToString(append(nonce, gcm.Seal(nil, nonce, []byte(token), successorAAD(sourceID, sessionID))...)), nil
}

func openSuccessor(privateKey, envelope, sourceID, sessionID string) (string, error) {
	if !strings.HasPrefix(envelope, successorPrefix) {
		return "", errors.New("malformed successor envelope")
	}
	body, err := b64.DecodeString(strings.TrimPrefix(envelope, successorPrefix))
	if err != nil || len(body) <= 12+16 {
		return "", errors.New("malformed successor envelope")
	}
	gcm, err := successorGCM(privateKey)
	if err != nil {
		return "", err
	}
	plain, err := gcm.Open(nil, body[:12], body[12:], successorAAD(sourceID, sessionID))
	if err != nil {
		return "", errors.New("successor envelope could not be decrypted")
	}
	return string(plain), nil
}
