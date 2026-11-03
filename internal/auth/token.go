// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package auth

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	accessTokenType     = "session_access"
	accessTokenIssuer   = "affine"
	accessTokenAudience = "affine-client"
	clockSkew           = 30 * time.Second
)

var b64 = base64.RawURLEncoding

type accessClaims struct {
	Sub string `json:"sub"`
	Sid string `json:"sid"`
	Typ string `json:"typ"`
	Iss string `json:"iss"`
	Aud string `json:"aud"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
}

func signAccessToken(key signingKey, userID, sessionID string, now time.Time, ttl time.Duration) (string, error) {
	secret, err := key.secretBytes()
	if err != nil {
		return "", err
	}
	header, _ := json.Marshal(struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
		Kid string `json:"kid"`
	}{"HS256", "JWT", key.ID})
	claims, _ := json.Marshal(accessClaims{Sub: userID, Sid: sessionID, Typ: accessTokenType, Iss: accessTokenIssuer, Aud: accessTokenAudience, Iat: now.Unix(), Exp: now.Add(ttl).Unix()})
	signingInput := b64.EncodeToString(header) + "." + b64.EncodeToString(claims)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signingInput))
	return signingInput + "." + b64.EncodeToString(mac.Sum(nil)), nil
}

// parseAccessTokenKid returns the kid of a structurally valid token header.
func parseAccessTokenKid(token string) (string, []string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", nil, ErrAccessTokenInvalid
	}
	raw, err := b64.DecodeString(parts[0])
	if err != nil {
		return "", nil, ErrAccessTokenInvalid
	}
	var header map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&header); err != nil || dec.More() || len(header) != 3 {
		return "", nil, ErrAccessTokenInvalid
	}
	alg, _ := header["alg"].(string)
	typ, _ := header["typ"].(string)
	kid, _ := header["kid"].(string)
	if alg != "HS256" || typ != "JWT" || !idPattern.MatchString(kid) {
		return "", nil, ErrAccessTokenInvalid
	}
	return kid, parts, nil
}

func verifyAccessToken(parts []string, key signingKey, now time.Time) (accessClaims, error) {
	var claims accessClaims
	secret, err := key.secretBytes()
	if err != nil {
		return claims, ErrAccessTokenInvalid
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil {
		return claims, ErrAccessTokenInvalid
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return claims, ErrAccessTokenInvalid
	}
	payload, err := b64.DecodeString(parts[1])
	if err != nil || json.Unmarshal(payload, &claims) != nil {
		return claims, ErrAccessTokenInvalid
	}
	if claims.Sub == "" || claims.Sid == "" || claims.Typ != accessTokenType || claims.Iss != accessTokenIssuer || claims.Aud != accessTokenAudience {
		return claims, ErrAccessTokenInvalid
	}
	if claims.Iat > now.Add(clockSkew).Unix() || claims.Exp <= claims.Iat {
		return claims, ErrAccessTokenInvalid
	}
	if claims.Exp+int64(clockSkew/time.Second) <= now.Unix() {
		return claims, ErrAccessTokenExpired
	}
	return claims, nil
}

// signingKey is one entry of the persisted auth.session.signingKeys array.
type signingKey struct {
	ID          string     `json:"id"`
	Secret      string     `json:"secret"`
	Status      string     `json:"status"`
	CreatedAt   *time.Time `json:"createdAt,omitempty"`
	Source      string     `json:"source"`
	RetiredAt   *time.Time `json:"retiredAt,omitempty"`
	VerifyUntil *time.Time `json:"verifyUntil,omitempty"`
}

func (k signingKey) secretBytes() ([]byte, error) {
	raw, err := b64.DecodeString(k.Secret)
	if err != nil || len(raw) < 32 || b64.EncodeToString(raw) != k.Secret {
		return nil, errors.New("invalid signing key secret")
	}
	return raw, nil
}

type keyring []signingKey

var errInvalidKeyring = errors.New("auth: invalid signing keyring")

func parseKeyring(raw []byte) (keyring, error) {
	var keys keyring
	if err := json.Unmarshal(raw, &keys); err != nil {
		return nil, errInvalidKeyring
	}
	if err := keys.validate(0); err != nil {
		return nil, err
	}
	return keys, nil
}

// validate checks the persisted invariants; accessTTL>0 also enforces the
// admin verification window.
func (ks keyring) validate(accessTTL time.Duration) error {
	seen := map[string]bool{}
	active := 0
	for _, k := range ks {
		if !idPattern.MatchString(k.ID) || seen[k.ID] {
			return errInvalidKeyring
		}
		seen[k.ID] = true
		if _, err := k.secretBytes(); err != nil {
			return errInvalidKeyring
		}
		if k.Source != "auto" && k.Source != "admin" {
			return errInvalidKeyring
		}
		switch k.Status {
		case "active":
			if k.RetiredAt != nil || k.VerifyUntil != nil {
				return errInvalidKeyring
			}
			active++
		case "retiring":
			if k.RetiredAt == nil || k.VerifyUntil == nil {
				return errInvalidKeyring
			}
			if accessTTL > 0 && k.VerifyUntil.Before(k.RetiredAt.Add(accessTTL+clockSkew)) {
				return errInvalidKeyring
			}
		default:
			return errInvalidKeyring
		}
	}
	if active != 1 {
		return errInvalidKeyring
	}
	return nil
}

func (ks keyring) active() signingKey {
	for _, k := range ks {
		if k.Status == "active" {
			return k
		}
	}
	return signingKey{}
}

func (ks keyring) find(id string) (signingKey, bool) {
	for _, k := range ks {
		if k.ID == id {
			return k, true
		}
	}
	return signingKey{}, false
}

func randomB64(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return b64.EncodeToString(buf)
}

func newAutoKeyring(now time.Time) keyring {
	created := now.UTC()
	return keyring{{
		ID:        fmt.Sprintf("auth-%d-%s", now.UnixMilli(), randomB64(9)),
		Secret:    randomB64(32),
		Status:    "active",
		CreatedAt: &created,
		Source:    "auto",
	}}
}
