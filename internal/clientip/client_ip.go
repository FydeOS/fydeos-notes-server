// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package clientip

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

const (
	maxClientIPHeaderBytes  = 128
	maxForwardedHeaderBytes = 4096
	maxForwardedHeaderHops  = 64
)

func RequestClientIP(r *http.Request, trustedProxies, trustedCloudflare, trustedXRealIP []netip.Prefix) string {
	peer, ok := remoteAddress(r.RemoteAddr)
	if !ok {
		return r.RemoteAddr
	}
	trustedForForwardedChain := isTrustedProxy(peer, trustedProxies) || isTrustedProxy(peer, trustedCloudflare)
	trustedForXRealIP := isTrustedProxy(peer, trustedXRealIP)
	if !trustedForForwardedChain && !trustedForXRealIP {
		return peer.String()
	}
	if isTrustedProxy(peer, trustedCloudflare) {
		if address, ok := singleHeaderAddress(r.Header.Values("CF-Connecting-IP")); ok {
			return address.String()
		}
	}
	if trustedForForwardedChain {
		if address, ok := forwardedHeaderAddress(r.Header.Values("X-Forwarded-For"), trustedProxies, trustedCloudflare); ok {
			return address.String()
		}
	}
	if trustedForXRealIP {
		if address, ok := singleHeaderAddress(r.Header.Values("X-Real-IP")); ok {
			return address.String()
		}
	}
	return peer.String()
}

func IsTrustedPeer(r *http.Request, trustedProxies, trustedCloudflare []netip.Prefix) bool {
	peer, ok := remoteAddress(r.RemoteAddr)
	return ok && (isTrustedProxy(peer, trustedProxies) || isTrustedProxy(peer, trustedCloudflare))
}

func remoteAddress(value string) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(value)
	if err == nil {
		return parseAddress(host)
	}
	return parseAddress(value)
}

func headerAddress(value string) (netip.Addr, bool) {
	if len(value) > maxClientIPHeaderBytes || strings.Contains(value, ",") {
		return netip.Addr{}, false
	}
	return parseAddress(value)
}

func singleHeaderAddress(values []string) (netip.Addr, bool) {
	if len(values) != 1 {
		return netip.Addr{}, false
	}
	return headerAddress(values[0])
}

func forwardedHeaderAddress(values []string, trustedProxies, trustedCloudflare []netip.Prefix) (netip.Addr, bool) {
	if len(values) == 0 {
		return netip.Addr{}, false
	}
	length := len(values) - 1
	for _, value := range values {
		if len(value) > maxForwardedHeaderBytes-length {
			return netip.Addr{}, false
		}
		length += len(value)
	}
	return forwardedAddress(strings.Join(values, ","), trustedProxies, trustedCloudflare)
}

func forwardedAddress(value string, trustedProxies, trustedCloudflare []netip.Prefix) (netip.Addr, bool) {
	if len(value) > maxForwardedHeaderBytes {
		return netip.Addr{}, false
	}
	if strings.TrimSpace(value) == "" || strings.Count(value, ",")+1 > maxForwardedHeaderHops {
		return netip.Addr{}, false
	}
	parts := strings.Split(value, ",")
	addresses := make([]netip.Addr, len(parts))
	for i, part := range parts {
		address, ok := parseAddress(part)
		if !ok {
			return netip.Addr{}, false
		}
		addresses[i] = address
	}
	for i := len(addresses) - 1; i >= 0; i-- {
		if !isTrustedProxy(addresses[i], trustedProxies) && !isTrustedProxy(addresses[i], trustedCloudflare) {
			return addresses[i], true
		}
	}
	return netip.Addr{}, false
}

func parseAddress(value string) (netip.Addr, bool) {
	if len(value) > maxClientIPHeaderBytes {
		return netip.Addr{}, false
	}
	value = strings.TrimSpace(value)
	if len(value) == 0 {
		return netip.Addr{}, false
	}
	address, err := netip.ParseAddr(value)
	if err != nil {
		return netip.Addr{}, false
	}
	if address.Zone() != "" {
		address = address.WithZone("")
	}
	return address.Unmap(), true
}

func isTrustedProxy(address netip.Addr, trustedProxies []netip.Prefix) bool {
	for _, prefix := range trustedProxies {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}
