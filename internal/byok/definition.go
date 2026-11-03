// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package byok

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/net/idna"
)

const (
	endpointProviderDefault  = "provider_default"
	endpointOpenAICompatible = "openai_compatible"
	maxModels                = 128
	maxModelIDBytes          = 512
)

// ValidateDefinition validates and normalizes a profile definition for provider.
func ValidateDefinition(provider string, input ProfileDefinitionInput) (ProfileDefinition, error) {
	if !isSupportedProvider(provider) {
		return ProfileDefinition{}, invalidf("unsupported BYOK provider")
	}
	if len(input.Models) > maxModels {
		return ProfileDefinition{}, invalidf("too many BYOK models")
	}
	endpoint, err := validateEndpoint(provider, input.Endpoint)
	if err != nil {
		return ProfileDefinition{}, err
	}
	if len(input.Models) == 0 {
		return ProfileDefinition{}, invalidf("models is required")
	}
	definition := ProfileDefinition{Endpoint: endpoint, Models: make([]ModelDeclaration, 0, len(input.Models))}
	seen := map[string]bool{}
	for _, model := range input.Models {
		id := strings.TrimSpace(model.ModelID)
		if id == "" {
			return ProfileDefinition{}, invalidf("modelId is required")
		}
		if len(id) > maxModelIDBytes {
			return ProfileDefinition{}, invalidf("modelId is too long")
		}
		if seen[id] {
			return ProfileDefinition{}, invalidf("duplicate modelId")
		}
		seen[id] = true
		if len(model.Capabilities) == 0 {
			return ProfileDefinition{}, invalidf("capabilities is required")
		}
		declaration := ModelDeclaration{ModelID: id, Enabled: model.Enabled, Capabilities: make([]Capability, 0, len(model.Capabilities))}
		for _, capability := range model.Capabilities {
			normalized, err := validateCapability(capability)
			if err != nil {
				return ProfileDefinition{}, err
			}
			declaration.Capabilities = append(declaration.Capabilities, normalized)
		}
		if err := checkUpperBound(provider, endpoint, declaration); err != nil {
			return ProfileDefinition{}, err
		}
		definition.Models = append(definition.Models, declaration)
	}
	return definition, nil
}

func validateEndpoint(provider string, input EndpointInput) (Endpoint, error) {
	switch input.Kind {
	case endpointProviderDefault:
		if input.URL == nil && input.Dialect == nil {
			return Endpoint{Kind: endpointProviderDefault}, nil
		}
	case endpointOpenAICompatible:
		if provider != "openai" || input.URL == nil || input.Dialect == nil {
			break
		}
		if *input.Dialect != "responses" && *input.Dialect != "chat_completions" {
			break
		}
		canonical, err := CanonicalizeEndpoint(*input.URL)
		if err != nil {
			break
		}
		return Endpoint{Kind: endpointOpenAICompatible, URL: canonical, Dialect: *input.Dialect}, nil
	}
	return Endpoint{}, invalidf("invalid BYOK endpoint")
}

func validateField(name string, values, allowed []string, required bool) ([]string, error) {
	if required && len(values) == 0 {
		return nil, invalidf("capability %s is required", name)
	}
	out := make([]string, 0, len(values))
	for _, v := range values {
		if !slices.Contains(allowed, v) {
			return nil, invalidf("unsupported capability %s value", name)
		}
		if slices.Contains(out, v) {
			return nil, invalidf("duplicate capability %s value", name)
		}
		out = append(out, v)
	}
	return out, nil
}

func validateCapability(c Capability) (Capability, error) {
	var out Capability
	var err error
	if out.Input, err = validateField("input", c.Input, allowedInputs, true); err != nil {
		return Capability{}, err
	}
	if out.Output, err = validateField("output", c.Output, allowedOutputs, true); err != nil {
		return Capability{}, err
	}
	if out.Features, err = validateField("features", c.Features, allowedFeatures, false); err != nil {
		return Capability{}, err
	}
	if out.AttachmentKinds, err = validateField("attachmentKinds", c.AttachmentKinds, allowedKinds, false); err != nil {
		return Capability{}, err
	}
	if out.AttachmentSource, err = validateField("attachmentSources", c.AttachmentSource, allowedSources, false); err != nil {
		return Capability{}, err
	}
	if (len(out.AttachmentKinds) == 0) != (len(out.AttachmentSource) == 0) {
		return Capability{}, invalidf("attachment kinds and sources must be declared together")
	}
	if !subset(out.AttachmentKinds, out.Input) {
		return Capability{}, invalidf("attachment kinds must also be declared as inputs")
	}
	return out, nil
}

func checkUpperBound(provider string, endpoint Endpoint, model ModelDeclaration) error {
	exceeded := invalidf("declared capability exceeds provider or model upper bound")
	if provider == "fal" {
		for _, c := range model.Capabilities {
			if !subset(c.Output, []string{"image"}) || !subset(c.Input, []string{"text", "image"}) {
				return exceeded
			}
		}
	}
	if endpoint.Kind != endpointProviderDefault {
		return nil
	}
	catalog, ok := catalogModel(provider, model.ModelID)
	if !ok {
		return exceeded
	}
	for _, c := range model.Capabilities {
		if !satisfies(catalog.Capabilities, c) {
			return exceeded
		}
	}
	return nil
}

// CanonicalizeEndpoint normalizes a custom endpoint base URL.
func CanonicalizeEndpoint(value string) (string, error) {
	bad := invalidf("invalid BYOK endpoint")
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.User != nil || parsed.Opaque != "" {
		return "", bad
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", bad
	}
	host := parsed.Hostname()
	if host == "" {
		return "", bad
	}
	if strings.Contains(parsed.Host, "[") {
		addr, err := netip.ParseAddr(host)
		if err != nil || !addr.Is6() || addr.Zone() != "" {
			return "", bad
		}
		host = "[" + strings.ToLower(addr.String()) + "]"
	} else if addr, err := netip.ParseAddr(host); err == nil {
		if !addr.Is4() {
			return "", bad
		}
		host = addr.String()
	} else {
		ascii, err := idna.Lookup.ToASCII(host)
		if err != nil || ascii == "" {
			return "", bad
		}
		host = strings.ToLower(ascii)
	}
	port := parsed.Port()
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 0 || n > 65535 {
			return "", bad
		}
		port = strconv.Itoa(n)
		if (scheme == "http" && n == 80) || (scheme == "https" && n == 443) {
			port = ""
		}
	}
	result := scheme + "://" + host
	if port != "" {
		result += ":" + port
	}
	return result + canonicalPath(parsed.EscapedPath()), nil
}

func isDotSegment(segment string) bool {
	return strings.EqualFold(segment, ".") || strings.EqualFold(segment, "%2e")
}

func isDotDotSegment(segment string) bool {
	switch strings.ToLower(segment) {
	case "..", ".%2e", "%2e.", "%2e%2e":
		return true
	}
	return false
}

func canonicalPath(path string) string {
	if path == "" {
		return ""
	}
	var stack []string
	for _, segment := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		switch {
		case isDotSegment(segment):
		case isDotDotSegment(segment):
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		default:
			stack = append(stack, segment)
		}
	}
	for len(stack) > 0 && stack[len(stack)-1] == "" {
		stack = stack[:len(stack)-1]
	}
	if len(stack) == 0 {
		return ""
	}
	return "/" + strings.Join(stack, "/")
}

// endpointIdentity is the AAD component naming the credential's endpoint.
func endpointIdentity(endpoint Endpoint) string {
	if endpoint.Kind == endpointProviderDefault {
		return "default"
	}
	return endpoint.URL
}

// definitionFingerprint is the lowercase hex SHA-256 of the definition JSON.
func definitionFingerprint(definition ProfileDefinition) string {
	encoded, _ := json.Marshal(definition)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// compactDefinitionJSON encodes a definition without HTML escaping.
func compactDefinitionJSON(definition ProfileDefinition) []byte {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(definition)
	return bytes.TrimRight(buffer.Bytes(), "\n")
}

func findModel(definition ProfileDefinition, modelID string) (ModelDeclaration, bool) {
	for _, model := range definition.Models {
		if model.ModelID == modelID {
			return model, true
		}
	}
	return ModelDeclaration{}, false
}
