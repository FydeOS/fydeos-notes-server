// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package byok

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func mustKey(t *testing.T, root string) *CredentialEnvelopeKey {
	t.Helper()
	key, err := NewCredentialEnvelopeKey([]byte(root))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(key.Destroy)
	return key
}

func TestEnvelopeRoundTrip(t *testing.T) {
	key := mustKey(t, "root")
	aad := ServerAAD("w", "p", "openai", "default")
	envelope, err := key.Encrypt([]byte("sk-secret"), aad)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(envelope, "byok:v1:") || strings.Contains(envelope, "=") {
		t.Fatalf("envelope = %q", envelope)
	}
	credential, err := key.Decrypt(envelope, aad)
	if err != nil || string(credential.Bytes()) != "sk-secret" {
		t.Fatalf("decrypt = %v, %v", credential, err)
	}
}

func TestEnvelopeKnownAnswer(t *testing.T) {
	key := mustKey(t, "known-answer-root")
	key.rand = bytes.NewReader(bytes.Repeat([]byte{7}, nonceSize))
	aad := ServerAAD("ws", "pid", "openai", "default")
	envelope, err := key.Encrypt([]byte("plaintext-key"), aad)
	if err != nil {
		t.Fatal(err)
	}
	// Independently derived from the spec parameters.
	derived, _ := hkdf.Key(sha256.New, []byte("known-answer-root"), nil, "FydeOS-Notes/Copilot/BYOK/v1", 32)
	block, _ := aes.NewCipher(derived)
	gcm, _ := cipher.NewGCM(block)
	nonce := bytes.Repeat([]byte{7}, 12)
	want := "byok:v1:" + base64.RawURLEncoding.EncodeToString(gcm.Seal(append([]byte(nil), nonce...), nonce, []byte("plaintext-key"), aad))
	if envelope != want {
		t.Fatalf("envelope = %q, want %q", envelope, want)
	}
	const pinned = "byok:v1:BwcHBwcHBwcHBwcH0O9fCF-3ejjWqfcSVR4tnsquBXwAQI7BLcb0ShY"
	if envelope != pinned {
		t.Fatalf("envelope drifted from pinned vector: %q", envelope)
	}
}

func TestEnvelopeRejections(t *testing.T) {
	key := mustKey(t, "root")
	aad := ServerAAD("w", "p", "openai", "default")
	envelope, _ := key.Encrypt([]byte("secret"), aad)
	body := strings.TrimPrefix(envelope, "byok:v1:")
	raw, _ := base64.RawURLEncoding.DecodeString(body)
	tampered := append([]byte(nil), raw...)
	tampered[len(tampered)-1] ^= 1
	cases := map[string]struct {
		key      *CredentialEnvelopeKey
		envelope string
		aad      []byte
	}{
		"different aad":  {key, envelope, ServerAAD("w", "p", "openai", "https://x")},
		"different root": {mustKey(t, "other"), envelope, aad},
		"tampered":       {key, "byok:v1:" + base64.RawURLEncoding.EncodeToString(tampered), aad},
		"missing prefix": {key, body, aad},
		"padded":         {key, "byok:v1:" + base64.URLEncoding.EncodeToString(raw), aad},
		"truncated":      {key, "byok:v1:" + base64.RawURLEncoding.EncodeToString(raw[:20]), aad},
		"bad base64":     {key, "byok:v1:***", aad},
	}
	if len(body)%4 == 0 {
		delete(cases, "padded")
	}
	for name, tc := range cases {
		if _, err := tc.key.Decrypt(tc.envelope, tc.aad); !errors.Is(err, ErrCredentialUnavailable) || err != ErrCredentialUnavailable {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if _, err := NewCredentialEnvelopeKey(nil); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatal("empty root accepted")
	}
}

func TestEnvelopeDestroy(t *testing.T) {
	key, _ := NewCredentialEnvelopeKey([]byte("root"))
	envelope, _ := key.Encrypt([]byte("secret"), nil)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				if c, err := key.Decrypt(envelope, nil); err == nil {
					c.Destroy()
				}
			}
		}()
	}
	key.Destroy()
	wg.Wait()
	if _, err := key.Encrypt([]byte("x"), nil); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatal("destroyed key encrypted")
	}
	if _, err := key.Decrypt(envelope, nil); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatal("destroyed key decrypted")
	}
	var nilKey *CredentialEnvelopeKey
	if _, err := nilKey.Encrypt(nil, nil); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatal("nil key encrypted")
	}
}

func TestSensitiveCredentialRedaction(t *testing.T) {
	source := []byte("top-secret")
	credential := NewSensitiveCredential(source)
	source[0] = 'X'
	if string(credential.Bytes()) != "top-secret" {
		t.Fatal("constructor did not copy")
	}
	printed := fmt.Sprintf("%v %#v %s %+v", credential, *credential, credential, credential)
	if strings.Contains(printed, "secret") {
		t.Fatalf("printed secret: %s", printed)
	}
	backing := credential.Bytes()
	credential.Destroy()
	if credential.Bytes() != nil || !bytes.Equal(backing, make([]byte, len(backing))) {
		t.Fatal("destroy did not zero")
	}
	var nilCredential *SensitiveCredential
	nilCredential.Destroy()
	if nilCredential.Bytes() != nil {
		t.Fatal("nil bytes")
	}
}

func TestAADEncoding(t *testing.T) {
	if got := string(ServerAAD("w", "p", "openai", "default")); got != "server\x00w\x00p\x00openai\x00default" {
		t.Fatalf("server aad = %q", got)
	}
	if got := string(LocalAAD("w", "u", "l", 3, "openai", "default")); got != "local\x00w\x00u\x00l\x003\x00openai\x00default" {
		t.Fatalf("local aad = %q", got)
	}
}
