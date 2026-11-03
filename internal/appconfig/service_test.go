// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package appconfig

import (
	"math"
	"reflect"
	"testing"
)

func TestAuthConfigCatalogDefaultsAndValidation(t *testing.T) {
	wantDefaults := map[string]any{
		"allowSignup":                    true,
		"allowSignupForOauth":            true,
		"requireEmailDomainVerification": false,
		"newAccountActionDelay":          int64(86400),
		"session.ttl":                    int64(1296000),
		"session.ttr":                    int64(604800),
		"token.accessTokenTtl":           int64(900),
		"token.refreshIdleTtl":           int64(2592000),
		"token.refreshAbsoluteTtl":       int64(15552000),
		"token.refreshGracePeriod":       int64(30),
		"token.refreshRetention":         int64(2592000),
		"trustedCloudflareHeaders":       false,
		"signInRateLimit":                map[string]any{"ttl": int64(60000), "ipLimit": int64(20), "emailLimit": int64(5)},
		"passwordRequirements":           map[string]any{"min": float64(8), "max": float64(32)},
	}
	updates := make([]Update, 0, len(descriptors))
	for _, item := range descriptors {
		want, ok := wantDefaults[item.key]
		if !ok {
			t.Errorf("unexpected auth config descriptor %q", item.key)
			continue
		}
		if !reflect.DeepEqual(item.defaultValue, want) {
			t.Errorf("default for auth.%s = %#v, want %#v", item.key, item.defaultValue, want)
		}
		updates = append(updates, Update{Module: "auth", Key: item.key, Value: item.defaultValue, HasValue: true})
	}
	if len(descriptors) != len(wantDefaults) {
		t.Fatalf("auth config descriptor count = %d, want %d", len(descriptors), len(wantDefaults))
	}
	results := (&Service{}).Validate(updates)
	if len(results) != 14 {
		t.Fatalf("auth config descriptor count = %d, want 14", len(results))
	}
	for _, result := range results {
		if !result.Valid {
			t.Errorf("default for auth.%s rejected: %s", result.Key, *result.Error)
		}
	}

	for _, update := range []Update{
		{Module: "auth", Key: "allowSignup", Value: "true", HasValue: true},
		{Module: "auth", Key: "newAccountActionDelay", Value: int64(-1), HasValue: true},
		{Module: "auth", Key: "session.ttl", Value: 1.5, HasValue: true},
		{Module: "auth", Key: "session.ttl", Value: math.Ldexp(1, 63), HasValue: true},
		{Module: "auth", Key: "token.accessTokenTtl", Value: int64(30), HasValue: true},
		{Module: "auth", Key: "token.refreshGracePeriod", Value: int64(61), HasValue: true},
		{Module: "auth", Key: "trustedCloudflareHeaders", Value: 1, HasValue: true},
		{Module: "auth", Key: "signInRateLimit", Value: map[string]any{"ttl": 60, "ipLimit": 20}, HasValue: true},
		{Module: "auth", Key: "passwordRequirements", Value: map[string]any{"min": 10, "max": 10}, HasValue: true},
	} {
		result := (&Service{}).Validate([]Update{update})[0]
		if result.Valid || result.Error == nil {
			t.Errorf("invalid config auth.%s accepted", update.Key)
		}
	}
}

func TestValidationRejectsDuplicateAndRedactsSecretValues(t *testing.T) {
	secret := "must-not-be-returned"
	results := (&Service{}).Validate([]Update{
		{Module: "auth", Key: "allowSignup", Value: true, HasValue: true},
		{Module: "auth", Key: "allowSignup", Value: false, HasValue: true},
		{Module: "crypto", Key: "privateKey", Value: secret, HasValue: true},
	})
	if results[0].Valid || results[1].Valid {
		t.Fatal("duplicate paths must be rejected as a batch")
	}
	if results[2].Value != nil || results[2].Valid {
		t.Fatal("unsupported secret config value was not redacted and rejected")
	}
}

func TestAuthConfigValidationMatrix(t *testing.T) {
	check := func(key string, value any, wantValid bool) {
		t.Helper()
		result := (&Service{}).Validate([]Update{{Module: "auth", Key: key, Value: value, HasValue: true}})[0]
		if result.Valid != wantValid {
			t.Errorf("auth.%s with %#v: valid = %t, want %t (error: %v)", key, value, result.Valid, wantValid, result.Error)
		}
	}

	for _, key := range []string{
		"allowSignup", "allowSignupForOauth", "requireEmailDomainVerification", "trustedCloudflareHeaders",
	} {
		check(key, "true", false)
	}

	for _, key := range []string{
		"newAccountActionDelay", "session.ttl", "session.ttr",
		"token.accessTokenTtl", "token.refreshIdleTtl", "token.refreshAbsoluteTtl",
		"token.refreshGracePeriod", "token.refreshRetention",
	} {
		check(key, "600", false)
		check(key, 1.5, false)
	}

	for _, key := range []string{"session.ttl", "session.ttr"} {
		check(key, int64(math.MinInt64), true)
		check(key, int64(math.MaxInt64), true)
	}

	ranges := []struct {
		key     string
		minimum int64
		maximum int64
	}{
		{key: "newAccountActionDelay", minimum: 0, maximum: math.MaxInt64},
		{key: "token.accessTokenTtl", minimum: 60, maximum: 3600},
		{key: "token.refreshIdleTtl", minimum: 3600, maximum: 31536000},
		{key: "token.refreshAbsoluteTtl", minimum: 3600, maximum: 63072000},
		{key: "token.refreshGracePeriod", minimum: 0, maximum: 60},
		{key: "token.refreshRetention", minimum: 3600, maximum: 31536000},
	}
	for _, item := range ranges {
		check(item.key, item.minimum, true)
		check(item.key, item.maximum, true)
		check(item.key, item.minimum-1, false)
		check(item.key, item.maximum+1, false)
	}
	check("newAccountActionDelay", float64(math.MaxInt64), false)

	check("signInRateLimit", true, false)
	for _, field := range []string{"ttl", "ipLimit", "emailLimit"} {
		value := map[string]any{"ttl": 60000, "ipLimit": 20, "emailLimit": 5}
		delete(value, field)
		check("signInRateLimit", value, false)

		for _, invalid := range []any{"1", 1.5, 0, -1} {
			value := map[string]any{"ttl": 60000, "ipLimit": 20, "emailLimit": 5}
			value[field] = invalid
			check("signInRateLimit", value, false)
		}
		largeValue := map[string]any{"ttl": 60000, "ipLimit": 20, "emailLimit": 5}
		largeValue[field] = 1e20
		check("signInRateLimit", largeValue, true)
	}
	check("signInRateLimit", map[string]any{"ttl": 60000, "ipLimit": 20, "emailLimit": 5, "extra": true}, false)
	check("signInRateLimit", map[string]any{"ttl": 1, "ipLimit": 1, "emailLimit": 1}, true)

	check("passwordRequirements", true, false)
	for _, field := range []string{"min", "max"} {
		value := map[string]any{"min": 8, "max": 32}
		delete(value, field)
		check("passwordRequirements", value, false)
		for _, invalid := range []any{"8", true} {
			value := map[string]any{"min": 8, "max": 32}
			value[field] = invalid
			check("passwordRequirements", value, false)
		}
	}
	check("passwordRequirements", map[string]any{"min": 0.99, "max": 32}, false)
	check("passwordRequirements", map[string]any{"min": 8, "max": 100.01}, false)
	check("passwordRequirements", map[string]any{"min": 32, "max": 32}, false)
	check("passwordRequirements", map[string]any{"min": 40, "max": 32}, false)
	check("passwordRequirements", map[string]any{"min": 1, "max": 100}, true)
	check("passwordRequirements", map[string]any{"min": 1.5, "max": 2.5}, true)
	check("passwordRequirements", map[string]any{"min": 8, "max": 32, "extra": true}, false)
}
