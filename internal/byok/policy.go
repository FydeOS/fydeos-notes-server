// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package byok

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/entitlements"
)

const (
	CustomEndpointDisabled    = "disabled"
	CustomEndpointEnabled     = "enabled"
	CustomEndpointUnavailable = "unavailable"

	dnsTimeout = 5 * time.Second
)

// Policy is the runtime BYOK policy.
type Policy struct {
	Enabled                  bool
	AllowedProviders         []string
	CustomEndpointMode       string
	PrivateEndpointSupported bool
}

func readPolicyValue(ctx context.Context, reader PolicyConfigReader, path, key string, target any) error {
	var r config.RuntimeConfigReader
	if reader != nil {
		r = reader
	}
	raw, found, err := config.ReadRuntimeValue(ctx, r, path, key)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("invalid runtime configuration %s", key)
	}
	return nil
}

// LoadPolicy reads the BYOK policy from runtime configuration.
func LoadPolicy(ctx context.Context, reader PolicyConfigReader, runtimePath, deployment string) (Policy, error) {
	enabled := true
	providers := []string{"openai", "anthropic", "gemini", "fal"}
	allowCustom, allowPrivate := false, false
	if err := readPolicyValue(ctx, reader, runtimePath, "copilot.byok.enabled", &enabled); err != nil {
		return Policy{}, err
	}
	if err := readPolicyValue(ctx, reader, runtimePath, "copilot.byok.allowedProviders", &providers); err != nil {
		return Policy{}, err
	}
	if err := readPolicyValue(ctx, reader, runtimePath, "copilot.byok.allowCustomEndpoint", &allowCustom); err != nil {
		return Policy{}, err
	}
	if err := readPolicyValue(ctx, reader, runtimePath, "copilot.byok.allowPrivateEndpoint", &allowPrivate); err != nil {
		return Policy{}, err
	}
	allowed := make([]string, 0, len(providers))
	for _, provider := range providers {
		if !isSupportedProvider(provider) || slices.Contains(allowed, provider) {
			return Policy{}, errors.New("invalid runtime configuration copilot.byok.allowedProviders")
		}
		allowed = append(allowed, provider)
	}
	slices.Sort(allowed)
	policy := Policy{Enabled: enabled, AllowedProviders: allowed, CustomEndpointMode: CustomEndpointDisabled}
	if entitlements.ParseDeployment(deployment) == entitlements.Cloud {
		policy.CustomEndpointMode = CustomEndpointUnavailable
	} else if allowCustom {
		policy.CustomEndpointMode = CustomEndpointEnabled
	}
	policy.PrivateEndpointSupported = policy.CustomEndpointMode == CustomEndpointEnabled && allowPrivate
	return policy, nil
}

// Allows reports whether the policy permits provider at endpoint.
func (p Policy) Allows(provider string, endpoint Endpoint) bool {
	if !p.Enabled || !slices.Contains(p.AllowedProviders, provider) {
		return false
	}
	switch endpoint.Kind {
	case endpointProviderDefault:
		return true
	case endpointOpenAICompatible:
		return provider == "openai" && p.CustomEndpointMode == CustomEndpointEnabled
	}
	return false
}

// Admit checks policy and, for custom endpoints, resolves and vets the host.
func (p Policy) Admit(ctx context.Context, provider string, endpoint Endpoint, resolver EndpointResolver) error {
	_, err := p.admit(ctx, provider, endpoint, resolver)
	return err
}

// admit is Admit returning the vetted addresses of a custom endpoint host.
func (p Policy) admit(ctx context.Context, provider string, endpoint Endpoint, resolver EndpointResolver) ([]netip.Addr, error) {
	if !p.Allows(provider, endpoint) {
		return nil, ErrTargetUnavailable
	}
	if endpoint.Kind != endpointOpenAICompatible {
		return nil, nil
	}
	parsed, err := url.Parse(endpoint.URL)
	if err != nil || parsed.Hostname() == "" {
		return nil, invalidf("invalid BYOK endpoint")
	}
	host := parsed.Hostname()
	if strings.EqualFold(host, "localhost") && !p.PrivateEndpointSupported {
		return nil, invalidf("custom endpoint host is not allowed")
	}
	addrs, err := resolveHost(ctx, host, resolver)
	if err != nil {
		return nil, err
	}
	for _, addr := range addrs {
		if !addressAllowed(parsed.Scheme, addr, p.PrivateEndpointSupported) {
			return nil, invalidf("custom endpoint address is not allowed")
		}
	}
	return addrs, nil
}

// customTransport returns the dial-guarded transport for a custom endpoint,
// pinned to the addresses vetted by admit.
func (p Policy) customTransport(endpoint Endpoint, addrs []netip.Addr, resolver EndpointResolver) *http.Transport {
	parsed, _ := url.Parse(endpoint.URL)
	return newPinnedTransport(parsed.Scheme, p.PrivateEndpointSupported, resolver, parsed.Hostname(), addrs)
}
