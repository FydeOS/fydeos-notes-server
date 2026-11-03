// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

const (
	maxProviderResponse = 1 << 20
	userAgent           = "FydeOS-Notes-Server"
)

var errResponseTooLarge = errors.New("provider response too large")

// originOf returns scheme://host[:port] (lowercase) and the dial address.
func originOf(raw string) (origin, hostport string, ok bool) {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Hostname() == "" {
		return "", "", false
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" {
		switch scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		default:
			return "", "", false
		}
	}
	hp := net.JoinHostPort(host, port)
	return scheme + "://" + hp, hp, true
}

// egressPolicy lists the origins a provider client may contact; the value
// says whether private network addresses are allowed for that origin.
type egressPolicy map[string]bool

func (p egressPolicy) add(raw string, private bool) {
	if raw == "" {
		return
	}
	if origin, _, ok := originOf(raw); ok {
		p[origin] = p[origin] || private
	}
}

// providerEgress derives the allowed origins for a provider.
func providerEgress(p OAuthProviderConfig) egressPolicy {
	policy := egressPolicy{}
	issuerOrigin := ""
	if p.Name == providerOIDC && p.AllowPrivateNetwork {
		issuerOrigin, _, _ = originOf(p.Issuer)
	}
	for _, endpoint := range []string{p.TokenURL, p.UserInfoURL, p.JWKSURL, githubEmailsURL(p)} {
		origin, _, ok := originOf(endpoint)
		if !ok {
			continue
		}
		private := (p.Name == providerFydeOS && p.AllowPrivateNetwork) || (issuerOrigin != "" && origin == issuerOrigin)
		policy.add(endpoint, private)
	}
	return policy
}

func githubEmailsURL(p OAuthProviderConfig) string {
	if p.Name != providerGitHub || p.UserInfoURL == "" {
		return ""
	}
	return strings.TrimRight(p.UserInfoURL, "/") + "/emails"
}

// newRestrictedClient builds an SSRF-safe client for the given policy.
func newRestrictedClient(policy egressPolicy) *http.Client {
	privateByHostPort := map[string]bool{}
	for origin, private := range policy {
		hp := origin[strings.Index(origin, "://")+3:]
		privateByHostPort[hp] = privateByHostPort[hp] || private
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			private, ok := privateByHostPort[strings.ToLower(addr)]
			if !ok {
				return nil, ErrProviderUnavailable
			}
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, ErrProviderUnavailable
			}
			ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if err != nil || len(ips) == 0 {
				return nil, fmt.Errorf("%w: resolve %s", ErrProviderUnavailable, host)
			}
			for _, ip := range ips {
				if !private && !isPublicIP(ip) {
					return nil, fmt.Errorf("%w: %s resolves to a non-public address", ErrProviderUnavailable, host)
				}
			}
			var lastErr error
			for _, ip := range ips {
				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.Unmap().String(), port))
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
			return nil, lastErr
		},
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
	}
	return &http.Client{
		Transport: &guardTransport{base: transport, policy: policy},
		Timeout:   10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

func isPublicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsValid() && ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() &&
		!ip.IsLinkLocalUnicast() && !ip.IsUnspecified() && !ip.IsMulticast() && !cgnat.Contains(ip)
}

type guardTransport struct {
	base   http.RoundTripper
	policy egressPolicy
}

func (t *guardTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	origin, _, ok := originOf(req.URL.String())
	if !ok {
		return nil, ErrProviderUnavailable
	}
	private, allowed := t.policy[origin]
	if !allowed || (!strings.HasPrefix(origin, "https://") && !private) {
		return nil, fmt.Errorf("%w: origin %s not allowed", ErrProviderUnavailable, origin)
	}
	req = req.Clone(req.Context())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	resp.Body = &cappedBody{rc: resp.Body, remaining: maxProviderResponse}
	return resp, nil
}

type cappedBody struct {
	rc        io.ReadCloser
	remaining int64
}

func (b *cappedBody) Read(p []byte) (int, error) {
	if b.remaining < 0 {
		return 0, errResponseTooLarge
	}
	if int64(len(p)) > b.remaining+1 {
		p = p[:b.remaining+1]
	}
	n, err := b.rc.Read(p)
	b.remaining -= int64(n)
	if b.remaining < 0 {
		return n - int(-b.remaining), errResponseTooLarge
	}
	return n, err
}

func (b *cappedBody) Close() error { return b.rc.Close() }

// readCapped reads a whole (already capped) response body.
func readCapped(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxProviderResponse+1))
	if err != nil {
		if errors.Is(err, errResponseTooLarge) {
			return nil, ErrInvalidOAuthResponse
		}
		return nil, fmt.Errorf("%w: %v", ErrProviderUnavailable, err)
	}
	if len(body) > maxProviderResponse {
		return nil, ErrInvalidOAuthResponse
	}
	return body, nil
}

type discoveryDocument struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
}

// discoverOIDC fetches and validates the OIDC discovery document.
func discoverOIDC(ctx context.Context, p OAuthProviderConfig) (OAuthProviderConfig, error) {
	issuer, err := url.Parse(p.Issuer)
	if err != nil || !issuer.IsAbs() || issuer.Host == "" || issuer.User != nil || issuer.RawQuery != "" || issuer.ForceQuery || issuer.Fragment != "" {
		return p, errors.New("invalid OIDC issuer")
	}
	if issuer.Scheme != "https" && !(issuer.Scheme == "http" && p.AllowPrivateNetwork) {
		return p, errors.New("OIDC issuer must use https")
	}
	issuerOrigin, _, _ := originOf(p.Issuer)
	policy := egressPolicy{}
	policy.add(p.Issuer, p.AllowPrivateNetwork)
	client := newRestrictedClient(policy)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.Issuer+"/.well-known/openid-configuration", nil)
	if err != nil {
		return p, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return p, err
	}
	defer resp.Body.Close()
	body, err := readCapped(resp.Body)
	if err != nil {
		return p, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return p, fmt.Errorf("OIDC discovery returned status %d", resp.StatusCode)
	}
	var doc discoveryDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		return p, fmt.Errorf("decode OIDC discovery: %w", err)
	}
	if strings.TrimRight(doc.Issuer, "/") != p.Issuer {
		return p, errors.New("OIDC discovery issuer mismatch")
	}
	for _, endpoint := range []string{doc.AuthorizationEndpoint, doc.TokenEndpoint, doc.UserinfoEndpoint, doc.JWKSURI} {
		u, err := url.Parse(endpoint)
		if endpoint == "" || err != nil || !u.IsAbs() || u.Host == "" {
			return p, errors.New("OIDC discovery endpoint missing or invalid")
		}
		if u.Scheme == "https" {
			continue
		}
		origin, _, _ := originOf(endpoint)
		if !(p.AllowPrivateNetwork && u.Scheme == "http" && origin == issuerOrigin) {
			return p, errors.New("OIDC discovery endpoint must use https")
		}
	}
	p.AuthorizationURL = doc.AuthorizationEndpoint
	p.TokenURL = doc.TokenEndpoint
	p.UserInfoURL = doc.UserinfoEndpoint
	p.JWKSURL = doc.JWKSURI
	return p, nil
}
