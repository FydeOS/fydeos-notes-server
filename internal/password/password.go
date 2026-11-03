// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package password

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	argonMemory      uint32 = 19 * 1024
	argonIterations  uint32 = 2
	argonParallelism uint8  = 1
	argonSaltLength         = 16
	argonKeyLength          = 32
)

func Hash(plain string) (string, error) {
	salt := make([]byte, argonSaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate password salt: %w", err)
	}
	key := argon2.IDKey([]byte(plain), salt, argonIterations, argonMemory, argonParallelism, argonKeyLength)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		argonMemory,
		argonIterations,
		argonParallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

func Verify(encoded, plain string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || (parts[1] != "argon2id" && parts[1] != "argon2i") || parts[2] != "v=19" {
		return false
	}
	var memory, iterations, parallelism uint32
	seen := map[string]bool{}
	for _, item := range strings.Split(parts[3], ",") {
		key, value, ok := strings.Cut(item, "=")
		if !ok || seen[key] {
			return false
		}
		parsed, err := strconv.ParseUint(value, 10, 32)
		if err != nil {
			return false
		}
		seen[key] = true
		switch key {
		case "m":
			memory = uint32(parsed)
		case "t":
			iterations = uint32(parsed)
		case "p":
			parallelism = uint32(parsed)
		default:
			return false
		}
	}
	if len(seen) != 3 || memory < 8*parallelism || memory > 262144 || iterations == 0 || iterations > 10 || parallelism == 0 || parallelism > 16 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < 8 || len(salt) > 64 {
		return false
	}
	expected, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(expected) < 16 || len(expected) > 64 {
		return false
	}
	var actual []byte
	if parts[1] == "argon2id" {
		actual = argon2.IDKey([]byte(plain), salt, iterations, memory, uint8(parallelism), uint32(len(expected)))
	} else {
		actual = argon2.Key([]byte(plain), salt, iterations, memory, uint8(parallelism), uint32(len(expected)))
	}
	return subtle.ConstantTimeCompare(actual, expected) == 1
}
