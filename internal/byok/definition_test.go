// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package byok

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func ptr[T any](v T) *T { return &v }

func textCap() Capability {
	return Capability{Input: []string{"text"}, Output: []string{"text"}}
}

func defaultDef(models ...ModelDeclaration) ProfileDefinitionInput {
	return ProfileDefinitionInput{Endpoint: EndpointInput{Kind: "provider_default"}, Models: models}
}

func customDef(models ...ModelDeclaration) ProfileDefinitionInput {
	return ProfileDefinitionInput{Endpoint: EndpointInput{Kind: "openai_compatible", URL: ptr("https://llm.example.com/v1"), Dialect: ptr("chat_completions")}, Models: models}
}

func model(id string, caps ...Capability) ModelDeclaration {
	return ModelDeclaration{ModelID: id, Enabled: true, Capabilities: caps}
}

func TestValidateDefinitionAccepts(t *testing.T) {
	def, err := ValidateDefinition("openai", defaultDef(model(" gpt-6.1-sol ", textCap()), ModelDeclaration{ModelID: "text-embedding-3-small", Capabilities: []Capability{{Input: []string{"text"}, Output: []string{"embedding"}}}}))
	if err != nil {
		t.Fatal(err)
	}
	if def.Endpoint != (Endpoint{Kind: "provider_default"}) || def.Models[0].ModelID != "gpt-6.1-sol" || def.Models[1].Enabled {
		t.Fatalf("definition = %#v", def)
	}
	if encoded, _ := json.Marshal(def); strings.Contains(string(encoded), "null") {
		t.Fatalf("normalized definition has null slices: %s", encoded)
	}
	custom, err := ValidateDefinition("openai", customDef(model("my-own-model", Capability{Input: []string{"text", "audio"}, Output: []string{"rerank"}})))
	if err != nil || custom.Endpoint.URL != "https://llm.example.com/v1" || custom.Endpoint.Dialect != "chat_completions" {
		t.Fatalf("custom = %#v, %v", custom, err)
	}
}

func TestValidateDefinitionRejects(t *testing.T) {
	many := make([]ModelDeclaration, 129)
	for i := range many {
		many[i] = model(fmt.Sprintf("m%d", i), textCap())
	}
	cases := map[string]struct {
		provider string
		def      ProfileDefinitionInput
	}{
		"provider":          {"mistral", defaultDef(model("x", textCap()))},
		"too many":          {"openai", customDef(many...)},
		"zero models":       {"openai", defaultDef()},
		"duplicate trimmed": {"openai", customDef(model("a", textCap()), model(" a", textCap()))},
		"long id":           {"openai", customDef(model(strings.Repeat("a", 513), textCap()))},
		"no capability":     {"openai", customDef(model("a"))},
		"unknown value":     {"openai", customDef(model("a", Capability{Input: []string{"video"}, Output: []string{"text"}}))},
		"duplicate value":   {"openai", customDef(model("a", Capability{Input: []string{"text", "text"}, Output: []string{"text"}}))},
		"kinds no sources":  {"openai", customDef(model("a", Capability{Input: []string{"text", "image"}, Output: []string{"text"}, AttachmentKinds: []string{"image"}}))},
		"kind not input":    {"openai", customDef(model("a", Capability{Input: []string{"text"}, Output: []string{"text"}, AttachmentKinds: []string{"image"}, AttachmentSource: []string{"data"}}))},
		"empty output":      {"openai", customDef(model("a", Capability{Input: []string{"text"}}))},
		"not in catalog":    {"openai", defaultDef(model("unknown-model", textCap()))},
		"exceeds catalog":   {"openai", defaultDef(model("text-embedding-3-small", textCap()))},
		"fal text output":   {"fal", defaultDef(model("flux-1/schnell", textCap()))},
		"anthropic custom":  {"anthropic", ProfileDefinitionInput{Endpoint: EndpointInput{Kind: "openai_compatible", URL: ptr("https://x.example"), Dialect: ptr("responses")}, Models: []ModelDeclaration{model("a", textCap())}}},
		"missing dialect":   {"openai", ProfileDefinitionInput{Endpoint: EndpointInput{Kind: "openai_compatible", URL: ptr("https://x.example")}, Models: []ModelDeclaration{model("a", textCap())}}},
		"bad dialect":       {"openai", ProfileDefinitionInput{Endpoint: EndpointInput{Kind: "openai_compatible", URL: ptr("https://x.example"), Dialect: ptr("grpc")}, Models: []ModelDeclaration{model("a", textCap())}}},
		"userinfo":          {"openai", ProfileDefinitionInput{Endpoint: EndpointInput{Kind: "openai_compatible", URL: ptr("https://u:p@x.example"), Dialect: ptr("responses")}, Models: []ModelDeclaration{model("a", textCap())}}},
		"ftp":               {"openai", ProfileDefinitionInput{Endpoint: EndpointInput{Kind: "openai_compatible", URL: ptr("ftp://x.example"), Dialect: ptr("responses")}, Models: []ModelDeclaration{model("a", textCap())}}},
		"default with url":  {"openai", ProfileDefinitionInput{Endpoint: EndpointInput{Kind: "provider_default", URL: ptr("https://x.example")}, Models: []ModelDeclaration{model("gpt-6.1-sol", textCap())}}},
	}
	for name, tc := range cases {
		_, err := ValidateDefinition(tc.provider, tc.def)
		if !errors.Is(err, ErrInvalidInput) || !strings.HasPrefix(err.Error(), "invalid BYOK input: ") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestCanonicalizeEndpoint(t *testing.T) {
	cases := map[string]string{
		"HTTPS://API.Example.com:443/v1/./x/../?q=1#f": "https://api.example.com/v1",
		" http://example.com:80/ ":                     "http://example.com",
		"https://example.com:8443/a/b/":                "https://example.com:8443/a/b",
		"https://BÜCHER.example/v1":                    "https://xn--bcher-kva.example/v1",
		"https://[2001:DB8::1]:8080/v1":                "https://[2001:db8::1]:8080/v1",
		"https://[2001:db8::1]:443":                    "https://[2001:db8::1]",
		"https://example.com/a/%2E%2e/b/%2e/c":         "https://example.com/b/c",
		"https://example.com/../../a":                  "https://example.com/a",
		"https://10.0.0.1:9000/v1//":                   "https://10.0.0.1:9000/v1",
	}
	for input, want := range cases {
		got, err := CanonicalizeEndpoint(input)
		if err != nil || got != want {
			t.Errorf("%q => %q, %v; want %q", input, got, err, want)
			continue
		}
		if again, _ := CanonicalizeEndpoint(got); again != got {
			t.Errorf("not idempotent: %q => %q", got, again)
		}
	}
	for _, bad := range []string{"", "not a url", "ftp://x", "https://user@x", "https://", "https://x:99999", "mailto:a@b"} {
		if _, err := CanonicalizeEndpoint(bad); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestCatalogInvariants(t *testing.T) {
	catalog, err := ByokCatalog()
	if err != nil {
		t.Fatal(err)
	}
	copy := catalog
	copy.Version = ""
	encoded, _ := json.Marshal(copy)
	sum := sha256.Sum256(encoded)
	if catalog.Version != hex.EncodeToString(sum[:])[:16] {
		t.Fatalf("version = %q", catalog.Version)
	}
	for _, provider := range catalog.Providers {
		recommended := false
		for _, m := range provider.Models {
			recommended = recommended || m.Recommended
			if _, err := ValidateDefinition(provider.Provider, defaultDef(ModelDeclaration{ModelID: m.ModelID, Enabled: true, Capabilities: m.Capabilities})); err != nil {
				t.Errorf("%s/%s invalid: %v", provider.Provider, m.ModelID, err)
			}
		}
		if !recommended {
			t.Errorf("%s has no recommended model", provider.Provider)
		}
	}
}

func TestModelSupportsServerAction(t *testing.T) {
	cases := []struct {
		provider, model, op string
		want                bool
	}{
		{"openai", "gpt-image-2.5-flare", "image_edit", true},
		{"fal", "flux-1/schnell", "image_edit", false},
		{"fal", "flux-1/schnell", "image_generate", true},
		{"gemini", "gemini-3.8-flash", "transcript", true},
		{"openai", "text-embedding-3-small", "embedding", true},
		{"openai", "gpt-6.1-sol", "chat", false},
		{"openai", "missing", "embedding", false},
	}
	for _, tc := range cases {
		if got := ModelSupportsServerAction(tc.provider, tc.model, tc.op); got != tc.want {
			t.Errorf("%+v = %v", tc, got)
		}
	}
}
