// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package storage

import (
	"net/http"
	"testing"
	"time"
)

func TestUploadTokenMatchesLegacyRustFormat(t *testing.T) {
	fields := []any{"ws", "file", "text/plain", 7}
	token, err := signUploadToken("secret", "/api/storage/upload", fields, 1_700_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if token != "NXaxFbKQjOOnqo5ggDsuK1jf8w5LlM7tLRWRoj_EmtY" {
		t.Fatalf("legacy upload token = %q", token)
	}
	if !verifyUploadToken("secret", "/api/storage/upload", fields, 1_700_000_000, token) {
		t.Fatal("valid legacy upload token was rejected")
	}
	if verifyUploadToken("secret", "/api/storage/upload", fields, 1_700_000_001, token) {
		t.Fatal("token with changed expiry was accepted")
	}
	if verifyUploadToken("secret", "/api/storage/upload", []any{"ws", "file", "image/png", 7}, 1_700_000_000, token) {
		t.Fatal("token with changed fields was accepted")
	}
}

func TestDecodeProviderConfigPresignedUploadSettings(t *testing.T) {
	for _, test := range []struct {
		name         string
		raw          string
		wantEnabled  bool
		wantProxy    bool
		wantPrefix   string
		wantTTL      int
		wantSignType bool
	}{
		{
			name:        "proxy upload",
			raw:         `{"provider":"cloudflare-r2","bucket":"notes","config":{"accountId":"account","usePresignedURL":{"enabled":true,"urlPrefix":"https://cdn.example.test/storage/","signKey":"secret"}}}`,
			wantEnabled: true, wantProxy: true, wantPrefix: "https://cdn.example.test/storage/", wantTTL: 60, wantSignType: true,
		},
		{
			name:        "direct upload",
			raw:         `{"provider":"aws-s3","bucket":"notes","config":{"region":"us-east-1","usePresignedURL":{"enabled":true,"urlPrefix":"https://cdn.example.test","signKey":""},"presign":{"expiresInSeconds":300,"signContentTypeForPut":false}}}`,
			wantEnabled: true, wantPrefix: "https://cdn.example.test", wantTTL: 300,
		},
		{
			name:         "disabled upload",
			raw:          `{"provider":"aws-s3","bucket":"notes","config":{"region":"us-east-1","usePresignedURL":{"enabled":false,"urlPrefix":"https://cdn.example.test","signKey":"secret"}}}`,
			wantTTL:      60,
			wantSignType: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			config, err := decodeProviderConfig([]byte(test.raw))
			if err != nil {
				t.Fatal(err)
			}
			if config.PresignedUploadEnabled != test.wantEnabled || config.ProxyUpload != test.wantProxy || config.UploadURLPrefix != test.wantPrefix || config.PresignExpiresInSeconds != test.wantTTL || config.PresignSignContentType != test.wantSignType {
				t.Fatalf("upload settings = enabled:%t proxy:%t prefix:%q ttl:%d signContentType:%t", config.PresignedUploadEnabled, config.ProxyUpload, config.UploadURLPrefix, config.PresignExpiresInSeconds, config.PresignSignContentType)
			}
			if test.wantEnabled {
				runtime := &Runtime{configs: map[string]ScopeConfig{BlobScope: config}}
				settings, enabled := runtime.UploadURLSettings(BlobScope)
				if !enabled || settings.PresignTTL != time.Duration(test.wantTTL)*time.Second {
					t.Fatalf("runtime upload URL settings = %+v, enabled:%t", settings, enabled)
				}
			}
		})
	}
}

func TestBuildProxyUploadURLPreservesConfiguredPrefix(t *testing.T) {
	got, err := BuildProxyUploadURL("https://cdn.example.test/storage/", "/api/storage/upload", map[string]string{"key": "a b", "token": "sig"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://cdn.example.test/storage/api/storage/upload?key=a+b&token=sig" {
		t.Fatalf("proxy upload URL = %q", got)
	}
	if _, err := BuildProxyUploadURL("https://cdn.example.test/path?bad=1", "/api/storage/upload", nil); err == nil {
		t.Fatal("URL prefix with query was accepted")
	}
}

func TestApplyPresignedURLPrefixRewritesOnlyOrigin(t *testing.T) {
	request := PresignedRequest{
		URL:       "https://s3.example.test/notes/blob?X-Amz-Signature=signature",
		Headers:   http.Header{"Content-Type": []string{"text/plain"}},
		ExpiresAt: time.Unix(1_700_000_000, 0),
	}
	got, err := ApplyPresignedURLPrefix(request, "https://cdn.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != "https://cdn.example.test/notes/blob?X-Amz-Signature=signature" || got.Headers.Get("Content-Type") != "text/plain" || !got.ExpiresAt.Equal(request.ExpiresAt) {
		t.Fatalf("rewritten presigned request = %+v", got)
	}
	if _, err := ApplyPresignedURLPrefix(request, "https://cdn.example.test/storage/"); err == nil {
		t.Fatal("direct-upload URL prefix with a path was accepted")
	}
}
