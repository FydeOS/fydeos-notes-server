// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"

	"github.com/jackc/pgx/v5"
)

var errNoRowsForTest = pgx.ErrNoRows

func (c fakeConfig) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	key, _ := args[0].(string)
	v, ok := c[key]
	return fakeRow{value: v, ok: ok}
}

func hmacSign(secret []byte, input string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(input))
	return b64.EncodeToString(mac.Sum(nil))
}
