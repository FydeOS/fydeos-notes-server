// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package byok

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

var (
	metadataAddr = netip.MustParseAddr("fd00:ec2::254")

	nonPublicV4 = mustPrefixes("0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24",
		"198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4")
	globalUnicastV6 = netip.MustParsePrefix("2000::/3")
	nonPublicV6     = mustPrefixes("2001::/23", "2001:db8::/32", "2002::/16", "3fff::/20")
	nat64Prefix     = netip.MustParsePrefix("64:ff9b::/96")
	sixToFour       = netip.MustParsePrefix("2002::/16")
	v4Compatible    = netip.MustParsePrefix("::/96")
)

func mustPrefixes(values ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(values))
	for i, v := range values {
		out[i] = netip.MustParsePrefix(v)
	}
	return out
}

func inAny(addr netip.Addr, prefixes []netip.Prefix) bool {
	for _, p := range prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// embeddedIPv4 returns the IPv4 address carried by an IPv6 transition form.
func embeddedIPv4(addr netip.Addr) (netip.Addr, bool) {
	if !addr.Is6() {
		return netip.Addr{}, false
	}
	b := addr.As16()
	switch {
	case nat64Prefix.Contains(addr):
		return netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}), true
	case sixToFour.Contains(addr):
		return netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}), true
	case v4Compatible.Contains(addr) && !addr.IsUnspecified() && !addr.IsLoopback():
		return netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}), true
	}
	return netip.Addr{}, false
}

func isPrivateOrLoopback(addr netip.Addr) bool { return addr.IsPrivate() || addr.IsLoopback() }

func isPublicIPv4(addr netip.Addr) bool {
	return !addr.IsPrivate() && !addr.IsLoopback() && !addr.IsLinkLocalUnicast() &&
		!addr.IsLinkLocalMulticast() && !addr.IsUnspecified() && !addr.IsMulticast() && !inAny(addr, nonPublicV4)
}

func isPublicIPv6(addr netip.Addr) bool {
	return globalUnicastV6.Contains(addr) && !inAny(addr, nonPublicV6) && !addr.IsLoopback() &&
		!addr.IsUnspecified() && !addr.IsPrivate() && !addr.IsLinkLocalUnicast() && !addr.IsMulticast()
}

// addressAllowed applies the custom endpoint address rule for scheme.
func addressAllowed(scheme string, addr netip.Addr, allowPrivate bool) bool {
	addr = addr.Unmap().WithZone("")
	if addr == metadataAddr {
		return false
	}
	judged := addr
	embedded, hasEmbedded := embeddedIPv4(addr)
	if hasEmbedded {
		judged = embedded
	}
	private := isPrivateOrLoopback(judged)
	switch scheme {
	case "http":
		return allowPrivate && private
	case "https":
		if private {
			return allowPrivate
		}
		if judged.Is4() {
			// A 6to4 address is never public, even with a public embedded IPv4.
			return isPublicIPv4(judged) && !sixToFour.Contains(addr)
		}
		return isPublicIPv6(judged)
	}
	return false
}

func resolveHost(ctx context.Context, host string, resolver EndpointResolver) ([]netip.Addr, error) {
	if addr, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{addr}, nil
	}
	lookupCtx, cancel := context.WithTimeout(ctx, dnsTimeout)
	defer cancel()
	if resolver == nil {
		resolver = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	addrs, err := resolver(lookupCtx, host)
	if errors.Is(err, context.DeadlineExceeded) || (err != nil && lookupCtx.Err() != nil) {
		return nil, invalidf("custom endpoint DNS resolution timed out")
	}
	if err != nil || len(addrs) == 0 {
		return nil, invalidf("custom endpoint DNS resolution failed")
	}
	return addrs, nil
}

// guardedDialer dials only addresses passing the address rule. Host names
// resolve once per dial and the connection uses the vetted IPs directly.
type guardedDialer struct {
	scheme       string
	allowPrivate bool
	resolver     EndpointResolver
	pinnedHost   string
	pinned       []netip.Addr
}

func (d guardedDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	addrs := d.pinned
	if d.pinnedHost == "" || !strings.EqualFold(host, d.pinnedHost) || len(addrs) == 0 {
		addrs, err = resolveHost(ctx, host, d.resolver)
	}
	if err != nil {
		return nil, fmt.Errorf("byok: resolve endpoint: %w", ErrTargetUnavailable)
	}
	for _, addr := range addrs {
		if !addressAllowed(d.scheme, addr, d.allowPrivate) {
			return nil, fmt.Errorf("byok: endpoint address refused: %w", ErrTargetUnavailable)
		}
	}
	dialer := net.Dialer{Timeout: dnsTimeout}
	var lastErr error
	for _, addr := range addrs {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(addr.Unmap().String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// newGuardedTransport returns a proxy-free transport restricted by the
// address rule for scheme.
func newGuardedTransport(scheme string, allowPrivate bool, resolver EndpointResolver) *http.Transport {
	return newPinnedTransport(scheme, allowPrivate, resolver, "", nil)
}

// newPinnedTransport is newGuardedTransport that dials host only at the
// already vetted addrs, without resolving it again.
func newPinnedTransport(scheme string, allowPrivate bool, resolver EndpointResolver, host string, addrs []netip.Addr) *http.Transport {
	dialer := guardedDialer{scheme: scheme, allowPrivate: allowPrivate, resolver: resolver, pinnedHost: host, pinned: addrs}
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
		MaxIdleConns:          16,
	}
}

var publicTransport = newGuardedTransport("https", false, nil)

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// NewPublicHTTPSClient returns a client that only reaches public addresses,
// uses no proxy and does not follow redirects.
func NewPublicHTTPSClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: publicTransport, CheckRedirect: noRedirect}
}
