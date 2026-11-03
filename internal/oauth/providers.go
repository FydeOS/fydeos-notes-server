// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/fydeos/fydeos-notes-server/internal/config"
)

// ConfigReader reads runtime configuration rows.
type ConfigReader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// OAuthProviderConfig is the resolved configuration of one provider.
type OAuthProviderConfig struct {
	Name, Label                                      string
	Enabled                                          bool
	ClientID, ClientSecret, Issuer, AccountServer    string
	AllowPrivateNetwork                              bool
	Args                                             map[string]string
	AuthorizationURL, TokenURL, UserInfoURL, JWKSURL string
}

// OAuthProviderLoader loads one provider configuration by name.
type OAuthProviderLoader interface {
	LoadProvider(context.Context, string) (OAuthProviderConfig, error)
}

const (
	providerGoogle = "google"
	providerGitHub = "github"
	providerApple  = "apple"
	providerOIDC   = "oidc"
	providerFydeOS = "fydeos"
)

var providerLabels = map[string]string{
	providerGoogle: "Google",
	providerGitHub: "GitHub",
	providerApple:  "Apple",
	providerOIDC:   "OIDC",
	providerFydeOS: "FydeOS",
}

var catalogOrder = []string{providerGoogle, providerGitHub, providerApple, providerFydeOS, providerOIDC}

// canonicalProvider maps a lowercase or label name to (name, label).
func canonicalProvider(name string) (string, string, bool) {
	if label, ok := providerLabels[name]; ok {
		return name, label, true
	}
	for canonical, label := range providerLabels {
		if label == name {
			return canonical, label, true
		}
	}
	return "", "", false
}

const (
	googleIssuer = "https://accounts.google.com"
	githubIssuer = "https://github.com"
	appleIssuer  = "https://appleid.apple.com"
)

// applyDefaultEndpoints fills unset well-known endpoints.
func applyDefaultEndpoints(p *OAuthProviderConfig) {
	set := func(dst *string, v string) {
		if *dst == "" {
			*dst = v
		}
	}
	switch p.Name {
	case providerGoogle:
		set(&p.AuthorizationURL, "https://accounts.google.com/o/oauth2/v2/auth")
		set(&p.TokenURL, "https://oauth2.googleapis.com/token")
		set(&p.UserInfoURL, "https://www.googleapis.com/oauth2/v2/userinfo")
	case providerGitHub:
		set(&p.AuthorizationURL, "https://github.com/login/oauth/authorize")
		set(&p.TokenURL, "https://github.com/login/oauth/access_token")
		set(&p.UserInfoURL, "https://api.github.com/user")
	case providerApple:
		set(&p.AuthorizationURL, "https://appleid.apple.com/auth/authorize")
		set(&p.TokenURL, "https://appleid.apple.com/auth/token")
		set(&p.JWKSURL, "https://appleid.apple.com/auth/keys")
	case providerFydeOS:
		if p.AccountServer != "" {
			set(&p.AuthorizationURL, p.AccountServer+"/signin")
			set(&p.TokenURL, p.AccountServer+"/oauth2/token")
			set(&p.UserInfoURL, p.AccountServer+"/oauth2/accountInfo")
		}
	}
}

// providerIssuer is the issuer used for the connected-account namespace.
func providerIssuer(p OAuthProviderConfig) string {
	switch p.Name {
	case providerGoogle:
		return googleIssuer
	case providerGitHub:
		return githubIssuer
	case providerApple:
		return appleIssuer
	case providerFydeOS:
		return p.AccountServer
	default:
		return p.Issuer
	}
}

// RuntimeOAuthProviderSource loads providers from runtime configuration.
type RuntimeOAuthProviderSource struct {
	reader ConfigReader
	cfg    config.Config
	// discover is replaceable in tests.
	discover func(context.Context, OAuthProviderConfig) (OAuthProviderConfig, error)
}

func NewRuntimeOAuthProviderSource(reader ConfigReader, cfg config.Config) *RuntimeOAuthProviderSource {
	return &RuntimeOAuthProviderSource{reader: reader, cfg: cfg, discover: discoverOIDC}
}

type rawProvider struct {
	ClientID            string
	ClientSecret        string
	Issuer              string
	AccountServer       string
	AllowPrivateNetwork bool
	Args                map[string]string
}

func decodeRawProvider(raw json.RawMessage, key string) (rawProvider, error) {
	var out rawProvider
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return out, fmt.Errorf("invalid runtime configuration %s: must be an object", key)
	}
	str := func(field string, dst *string) error {
		v, ok := fields[field]
		if !ok {
			return nil
		}
		if err := strictUnmarshal(v, dst); err != nil {
			return fmt.Errorf("invalid runtime configuration %s.%s", key, field)
		}
		return nil
	}
	for field, dst := range map[string]*string{"clientId": &out.ClientID, "clientSecret": &out.ClientSecret, "issuer": &out.Issuer, "accountServer": &out.AccountServer} {
		if err := str(field, dst); err != nil {
			return out, err
		}
	}
	if v, ok := fields["allowPrivateNetwork"]; ok {
		if err := strictUnmarshal(v, &out.AllowPrivateNetwork); err != nil {
			return out, fmt.Errorf("invalid runtime configuration %s.allowPrivateNetwork", key)
		}
	}
	out.Args = map[string]string{}
	if v, ok := fields["args"]; ok {
		var args map[string]json.RawMessage
		if err := strictUnmarshal(v, &args); err != nil {
			return out, fmt.Errorf("invalid runtime configuration %s.args", key)
		}
		for k, av := range args {
			var s string
			if err := strictUnmarshal(av, &s); err != nil {
				return out, fmt.Errorf("invalid runtime configuration %s.args.%s", key, k)
			}
			out.Args[k] = s
		}
	}
	return out, nil
}

// strictUnmarshal rejects explicit null.
func strictUnmarshal(raw json.RawMessage, dst any) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return errors.New("null value")
	}
	return json.Unmarshal(raw, dst)
}

func (s *RuntimeOAuthProviderSource) LoadProvider(ctx context.Context, name string) (OAuthProviderConfig, error) {
	canonical, label, ok := canonicalProvider(name)
	if !ok {
		return OAuthProviderConfig{}, ErrUnknownProvider
	}
	result := OAuthProviderConfig{Name: canonical, Label: label, Args: map[string]string{}}
	key := "oauth.providers." + canonical
	raw, found, err := config.ReadRuntimeValue(ctx, s.reader, s.cfg.RuntimeConfigPath, key)
	if err != nil {
		return result, err
	}
	var rp rawProvider
	rp.Args = map[string]string{}
	if found {
		if rp, err = decodeRawProvider(raw, key); err != nil {
			return result, err
		}
	}
	if canonical == providerFydeOS {
		return s.loadFydeOS(result, rp, found)
	}
	if !found {
		return result, nil
	}
	enabled := strings.TrimSpace(rp.ClientID) != "" && (strings.TrimSpace(rp.ClientSecret) != "" ||
		(canonical == providerApple && hasKey(rp.Args, "privateKey") && rp.Args["keyId"] != "" && rp.Args["teamId"] != ""))
	if !enabled {
		return result, nil
	}
	if rp.Issuer != "" && canonical != providerOIDC {
		return result, fmt.Errorf("invalid runtime configuration %s: issuer is only supported for OIDC", key)
	}
	if rp.AccountServer != "" {
		return result, fmt.Errorf("invalid runtime configuration %s: accountServer is not supported", key)
	}
	result.Enabled = true
	result.ClientID = rp.ClientID
	result.ClientSecret = rp.ClientSecret
	result.Issuer = strings.TrimRight(rp.Issuer, "/")
	result.AllowPrivateNetwork = rp.AllowPrivateNetwork
	result.Args = rp.Args
	if canonical == providerOIDC {
		if strings.TrimSpace(result.Issuer) == "" {
			return result, ErrProviderDisabled
		}
		discovered, err := s.discover(ctx, result)
		if err != nil {
			return result, fmt.Errorf("%w: %v", ErrProviderDisabled, err)
		}
		return discovered, nil
	}
	applyDefaultEndpoints(&result)
	return result, nil
}

func hasKey(m map[string]string, k string) bool {
	_, ok := m[k]
	return ok
}

func (s *RuntimeOAuthProviderSource) loadFydeOS(result OAuthProviderConfig, rp rawProvider, found bool) (OAuthProviderConfig, error) {
	if found && (len(rp.Args) > 0 || rp.Issuer != "") {
		return result, errors.New("invalid runtime configuration oauth.providers.fydeos: args and issuer are not supported")
	}
	if env := s.cfg.FydeOSOAuth; env.Configured {
		rp = rawProvider{AccountServer: env.AccountServer, ClientID: env.ClientID, ClientSecret: env.ClientSecret, AllowPrivateNetwork: env.AllowPrivateNetwork}
	}
	blank := func(v string) bool { return strings.TrimSpace(v) == "" }
	if blank(rp.AccountServer) && blank(rp.ClientID) && blank(rp.ClientSecret) && !rp.AllowPrivateNetwork {
		return result, nil
	}
	if blank(rp.AccountServer) || blank(rp.ClientID) || blank(rp.ClientSecret) {
		return result, errors.New("invalid FydeOS OAuth configuration: accountServer, clientId and clientSecret are required together")
	}
	origin, err := normalizeAccountServer(rp.AccountServer, rp.AllowPrivateNetwork)
	if err != nil {
		return result, err
	}
	result.Enabled = true
	result.ClientID = rp.ClientID
	result.ClientSecret = rp.ClientSecret
	result.AccountServer = origin
	result.AllowPrivateNetwork = rp.AllowPrivateNetwork
	applyDefaultEndpoints(&result)
	return result, nil
}

func normalizeAccountServer(raw string, allowPrivate bool) (string, error) {
	invalid := errors.New("invalid FydeOS account server origin")
	if raw != strings.TrimSpace(raw) || strings.IndexFunc(raw, isControl) >= 0 {
		return "", invalid
	}
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.Opaque != "" {
		return "", invalid
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	switch u.Scheme {
	case "https":
		if port == "443" {
			port = ""
		}
	case "http":
		if !allowPrivate || !isLoopbackName(host) {
			return "", invalid
		}
		if port == "80" {
			port = ""
		}
	default:
		return "", invalid
	}
	for _, r := range host {
		if r > 127 {
			return "", invalid
		}
	}
	h := host
	if strings.Contains(h, ":") {
		h = "[" + h + "]"
	}
	if port != "" {
		h += ":" + port
	}
	return u.Scheme + "://" + h, nil
}

func isLoopbackName(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) }

// ProviderCatalog reports the providers that are currently usable.
type ProviderCatalog struct {
	source OAuthProviderLoader
}

func NewProviderCatalog(reader ConfigReader, cfg config.Config) *ProviderCatalog {
	return NewProviderCatalogWithOAuthProviderSource(NewRuntimeOAuthProviderSource(reader, cfg))
}

func NewProviderCatalogWithOAuthProviderSource(source OAuthProviderLoader) *ProviderCatalog {
	return &ProviderCatalog{source: source}
}

func (c *ProviderCatalog) AvailableProviders(ctx context.Context) ([]string, error) {
	out := []string{}
	for _, name := range catalogOrder {
		p, err := c.source.LoadProvider(ctx, name)
		if errors.Is(err, ErrProviderDisabled) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if p.Enabled {
			out = append(out, name)
		}
	}
	return out, nil
}
