// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package byok

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"strconv"
	"strings"
	"sync"
)

const (
	envelopePrefix = "byok:v1:"
	envelopeInfo   = "FydeOS-Notes/Copilot/BYOK/v1"
	nonceSize      = 12
	tagSize        = 16
)

// SensitiveCredential holds plaintext credential bytes that never print.
type SensitiveCredential struct {
	value []byte
}

// NewSensitiveCredential copies value into a new credential.
func NewSensitiveCredential(value []byte) *SensitiveCredential {
	return &SensitiveCredential{value: append([]byte(nil), value...)}
}

// Bytes returns the plaintext; nil for a nil or destroyed credential.
func (c *SensitiveCredential) Bytes() []byte {
	if c == nil {
		return nil
	}
	return c.value
}

// Destroy zeroes and drops the plaintext.
func (c *SensitiveCredential) Destroy() {
	if c == nil {
		return
	}
	clear(c.value)
	c.value = nil
}

func (SensitiveCredential) String() string   { return "[REDACTED]" }
func (SensitiveCredential) GoString() string { return "[REDACTED]" }

// CredentialEnvelopeKey encrypts credentials into v1 envelopes. It is safe
// for concurrent use; Destroy waits for in-flight operations.
type CredentialEnvelopeKey struct {
	mu   sync.RWMutex
	key  []byte
	aead cipher.AEAD
	rand io.Reader
}

// NewCredentialEnvelopeKey derives the envelope key from the root secret.
func NewCredentialEnvelopeKey(rootSecret []byte) (*CredentialEnvelopeKey, error) {
	if len(rootSecret) == 0 {
		return nil, ErrCredentialUnavailable
	}
	key, err := hkdf.Key(sha256.New, rootSecret, nil, envelopeInfo, 32)
	if err != nil {
		return nil, ErrCredentialUnavailable
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, ErrCredentialUnavailable
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, ErrCredentialUnavailable
	}
	return &CredentialEnvelopeKey{key: key, aead: aead, rand: rand.Reader}, nil
}

// Encrypt seals credential under aad and returns a v1 envelope string.
func (k *CredentialEnvelopeKey) Encrypt(credential, aad []byte) (string, error) {
	if k == nil {
		return "", ErrCredentialUnavailable
	}
	k.mu.RLock()
	defer k.mu.RUnlock()
	if k.aead == nil {
		return "", ErrCredentialUnavailable
	}
	nonce := make([]byte, nonceSize, nonceSize+len(credential)+tagSize)
	if _, err := io.ReadFull(k.rand, nonce); err != nil {
		return "", ErrCredentialUnavailable
	}
	sealed := k.aead.Seal(nonce, nonce, credential, aad)
	return envelopePrefix + base64.RawURLEncoding.EncodeToString(sealed), nil
}

// Decrypt opens a v1 envelope. Every failure is ErrCredentialUnavailable.
func (k *CredentialEnvelopeKey) Decrypt(envelope string, aad []byte) (*SensitiveCredential, error) {
	if k == nil {
		return nil, ErrCredentialUnavailable
	}
	encoded, ok := strings.CutPrefix(envelope, envelopePrefix)
	if !ok {
		return nil, ErrCredentialUnavailable
	}
	body, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || base64.RawURLEncoding.EncodeToString(body) != encoded || len(body) < nonceSize+tagSize {
		return nil, ErrCredentialUnavailable
	}
	k.mu.RLock()
	defer k.mu.RUnlock()
	if k.aead == nil {
		return nil, ErrCredentialUnavailable
	}
	plaintext, err := k.aead.Open(nil, body[:nonceSize], body[nonceSize:], aad)
	if err != nil {
		return nil, ErrCredentialUnavailable
	}
	credential := &SensitiveCredential{value: plaintext}
	return credential, nil
}

// Destroy zeroes the key; later operations fail.
func (k *CredentialEnvelopeKey) Destroy() {
	if k == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	clear(k.key)
	k.key = nil
	k.aead = nil
}

// ServerAAD binds a server profile credential to its owner and endpoint.
func ServerAAD(workspaceID, profileID, provider, endpointIdentity string) []byte {
	return []byte(strings.Join([]string{"server", workspaceID, profileID, provider, endpointIdentity}, "\x00"))
}

// LocalAAD binds a local-lease credential to its lease slot and endpoint.
func LocalAAD(workspaceID, userID, leaseID string, index uint64, provider, endpointIdentity string) []byte {
	return []byte(strings.Join([]string{"local", workspaceID, userID, leaseID, strconv.FormatUint(index, 10), provider, endpointIdentity}, "\x00"))
}
