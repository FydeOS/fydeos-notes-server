// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package config

import (
	"context"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

type websocketConfigReader struct {
	value []byte
	err   error
}

func (r websocketConfigReader) QueryRow(context.Context, string, ...any) pgx.Row {
	return websocketConfigRow{value: r.value, err: r.err}
}

type websocketConfigRow struct {
	value []byte
	err   error
}

func (r websocketConfigRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	value, ok := dest[0].(*[]byte)
	if !ok {
		return os.ErrInvalid
	}
	*value = append((*value)[:0], r.value...)
	return nil
}

func env(values map[string]string) LookupEnv {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func TestLoadUsesLegacyStartupFile(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("NODE_ENV", "production")
	t.Setenv("AFFINE_ENV", "production")
	t.Setenv("SERVER_FLAVOR", "graphql")
	t.Setenv("REDIS_SERVER_URL", "redis://127.0.0.1:6379/0")
	t.Setenv("DEPLOYMENT_TYPE", "selfhosted")
	t.Setenv("DATABASE_URL", " ")
	t.Setenv("AFFINE_SERVER_HOST", "environment.example.test")
	t.Setenv("AFFINE_SERVER_PORT", "3010")
	t.Setenv("AFFINE_SERVER_HTTPS", "false")
	t.Setenv("AFFINE_SERVER_SUB_PATH", "/environment")
	t.Setenv("AFFINE_SERVER_EXTERNAL_URL", "https://environment.example.test/environment")
	t.Setenv("LISTEN_ADDR", "0.0.0.0")
	if err := os.WriteFile("config.json", []byte(`{
		"deployment":{"type":"cloud"},
		"db":{"datasourceUrl":"postgresql://fixture:synthetic-secret@127.0.0.1:5432/filedb"},
		"server":{"host":"file.example.test","hosts":["file.example.test"],"port":4321,"https":true,
		"listenAddr":"127.0.0.1","path":"/file/","externalUrl":"https://file.example.test/file"}
	}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Deployment != "affine" {
		t.Fatalf("file deployment was ignored: %s", cfg.Deployment)
	}
	if cfg.DatabaseURL != "postgresql://fixture:synthetic-secret@127.0.0.1:5432/filedb" {
		t.Fatal("file database URL was ignored")
	}
	if cfg.Host != "file.example.test" || cfg.Port != 4321 || !cfg.HTTPS || cfg.ListenAddr != "127.0.0.1" || cfg.Path != "file" || cfg.ExternalURL != "https://file.example.test/file" || strings.Join(cfg.Hosts, ",") != "file.example.test" {
		t.Fatal("file server configuration did not override environment defaults")
	}
	if cfg.RuntimeConfigPath == "" {
		t.Fatal("runtime configuration path lost")
	}
}

func TestLoadUsesExplicitRuntimeConfigPath(t *testing.T) {
	workingDir := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "runtime.json")
	t.Chdir(workingDir)
	t.Setenv("AFFINE_BACKEND_RUNTIME_CONFIG_PATH", configPath)
	if err := os.WriteFile(configPath, []byte(`{"deployment":{"type":"cloud"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("config.json", []byte(`{"deployment":{"type":"selfhosted"}}`), 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RuntimeConfigPath != configPath || cfg.Deployment != "affine" {
		t.Fatalf("explicit runtime config was ignored: path=%q deployment=%q", cfg.RuntimeConfigPath, cfg.Deployment)
	}
}

func TestLoadFydeOSManagedCopilotEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name    string
		values  map[string]string
		want    bool
		wantErr bool
	}{
		{name: "not configured", values: map[string]string{}},
		{name: "configured", values: map[string]string{
			"COPILOT_OPENAI_API_KEY":     "secret",
			"COPILOT_OPENAI_BASE_URL":    "https://api.example.test/v1",
			"COPILOT_DEFAULT_CHAT_MODEL": "model-id",
		}, want: true},
		{name: "partial configuration", values: map[string]string{"COPILOT_OPENAI_API_KEY": "secret"}, wantErr: true},
		{name: "unsupported dialect", values: map[string]string{
			"COPILOT_OPENAI_API_KEY":     "secret",
			"COPILOT_OPENAI_BASE_URL":    "https://api.example.test/v1",
			"COPILOT_DEFAULT_CHAT_MODEL": "model-id",
			"COPILOT_OPENAI_API_DIALECT": "legacy",
		}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := loadFydeOSManagedCopilot(env(tc.values))
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("loadFydeOSManagedCopilot() = %v, %v; want %v, error=%v", got, err, tc.want, tc.wantErr)
			}
		})
	}
	cfg, err := LoadFrom(env(map[string]string{
		"COPILOT_OPENAI_API_KEY":     "secret",
		"COPILOT_OPENAI_BASE_URL":    "https://api.example.test/v1/",
		"COPILOT_DEFAULT_CHAT_MODEL": "model-id",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.FydeOSManagedCopilot || cfg.FydeOSManagedCopilotAPIKey != "secret" || cfg.FydeOSManagedCopilotBaseURL != "https://api.example.test/v1" || cfg.FydeOSManagedCopilotModel != "model-id" {
		t.Fatalf("managed Copilot runtime config = %#v", cfg)
	}
}

func TestLoadRedisURLAcceptsPasswordOnlyURLAndIgnoresBlankSplitOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"redis":{"host":"file.example.test","username":"file-user","password":"file-password","db":3}}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		values map[string]string
		want   string
	}{
		{"password URL", map[string]string{"REDIS_SERVER_URL": "redis://:url-password@127.0.0.1:6379/2"}, "redis://:url-password@127.0.0.1:6379/2"},
		{"blank split", map[string]string{"REDIS_SERVER_HOST": " ", "REDIS_SERVER_USERNAME": "", "REDIS_SERVER_PASSWORD": " "}, "redis://file-user:file-password@file.example.test:6379/3"},
		{"blank primary alias", map[string]string{"REDIS_SERVER_USERNAME": " ", "REDIS_SERVER_USER": "alias-user"}, "redis://alias-user:file-password@file.example.test:6379/3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := loadRedisURL(env(tc.values), path)
			if err != nil || got != tc.want {
				t.Fatalf("legacy Redis source selection differs; error=%v", err)
			}
		})
	}
}

func TestLoadFromDefaultsAndLegacyDeploymentTypes(t *testing.T) {
	got, err := LoadFrom(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if got.ListenAddr != "0.0.0.0" || got.Port != 3010 || got.Flavor != "allinone" {
		t.Fatalf("unexpected server defaults: %+v", got)
	}
	if got.Deployment != "selfhosted" || got.Version != "0.27.5" {
		t.Fatalf("unexpected deployment defaults: %+v", got)
	}
	if got.DatabaseURL != "postgresql://localhost:5432/affine" {
		t.Fatal("legacy default database URL was not preserved")
	}
	if got.WebsocketMaxHttpBufferSize != DefaultWebsocketMaxHttpBufferSize {
		t.Fatalf("websocket payload limit = %d, want %d", got.WebsocketMaxHttpBufferSize, DefaultWebsocketMaxHttpBufferSize)
	}
	if got.RedisURL != "redis://localhost:6379/0" {
		t.Fatalf("default Redis URL = %q", got.RedisURL)
	}

	got, err = LoadFrom(env(map[string]string{
		"NODE_ENV":        "development",
		"DEPLOYMENT_TYPE": "cloud",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got.Deployment != "affine" {
		t.Fatalf("cloud should normalize to affine, got %q", got.Deployment)
	}
}

func TestStartupFilePrecedenceAndDottedKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	for _, tc := range []struct {
		name, document, wantDeployment, wantDatabase string
		values                                       map[string]string
	}{
		{"file cloud wins invalid legacy env", `{"deployment":{"type":"cloud"},"db":{"datasourceUrl":"postgresql://127.0.0.1/file"}}`, "affine", "postgresql://127.0.0.1/file", map[string]string{"DEPLOYMENT_TYPE": "invalid"}},
		{"file selfhost overrides dev", `{"deployment":{"type":"selfhosted"}}`, "selfhosted", "postgresql://localhost:5432/affine", map[string]string{"NODE_ENV": "development"}},
		{"database environment wins", `{"db":{"datasourceUrl":"postgresql://127.0.0.1/file"}}`, "selfhosted", "postgresql://127.0.0.1/environment", map[string]string{"DATABASE_URL": "postgresql://127.0.0.1/environment"}},
		{"blank environment uses file", `{"db":{"datasourceUrl":"postgresql://127.0.0.1/file"}}`, "selfhosted", "postgresql://127.0.0.1/file", map[string]string{"DATABASE_URL": "\t "}},
		{"blank values use legacy default", `{"db":{"datasourceUrl":" "}}`, "selfhosted", "postgresql://localhost:5432/affine", map[string]string{"DATABASE_URL": ""}},
		{"nullable datasource", `{"db":{"datasourceUrl":null}}`, "affine", "postgresql://localhost:5432/affine", map[string]string{"NODE_ENV": "development"}},
		{"legacy deployment fallback", `{}`, "affine", "postgresql://localhost:5432/affine", map[string]string{"DEPLOYMENT_TYPE": "cloud"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tc.document), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := loadFromPath(env(tc.values), path)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Deployment != tc.wantDeployment || cfg.DatabaseURL != tc.wantDatabase {
				t.Fatal("startup source precedence differs")
			}
		})
	}
	if err := os.WriteFile(path, []byte(`{"redis":{"ioredis.tls":{},"db":4},"server":{"externalUrl":null,"port":4321}}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadFromPath(env(map[string]string{"AFFINE_SERVER_EXTERNAL_URL": "https://env.example.test"}), path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RedisURL != "rediss://localhost:6379/4" || cfg.ExternalURL != "" || cfg.Port != 4321 {
		t.Fatal("dotted Redis TLS or nullable external URL did not apply")
	}
	if cfg.ListenAddr != "0.0.0.0" || cfg.Host != "localhost" {
		t.Fatal("partial file lost server defaults")
	}
}

func TestStartupFileRejectsMalformedValuesWithoutLeakingContents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	for _, document := range []string{
		`null`, `[]`, `{"db":`, `{"deployment":{"type":"synthetic-secret"}}`,
		`{"server":"synthetic-secret"}`, `{"server":{"port":0}}`, `{"server":{"port":65536}}`,
		`{"server":{"port":4321.5}}`, `{"server":{"port":"synthetic-secret"}}`,
		`{"server":{"https":"synthetic-secret"}}`, `{"server":{"hosts":["ok",5]}}`,
		`{"server":{"host":null}}`, `{"server":{"listenAddr":false}}`,
		`{"server":{"externalUrl":"https://user:synthetic-secret@example.test"}}`,
		`{"server":{"externalUrl":"https://example.test?secret=synthetic-secret"}}`,
		`{"server":{"path":"../synthetic-secret"}}`,
		`{"db":{"datasourceUrl":{"secret":"synthetic-secret"}}}`,
		`{"redis":null}`, `{"redis":{"port":0}}`, `{"redis":{"db":11}}`, `{"redis":{"username":null}}`,
		`{"redis":{"password":42}}`, `{"redis":{"ioredis":null}}`,
	} {
		if err := os.WriteFile(path, []byte(document), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := loadFromPath(env(map[string]string{"DATABASE_URL": "postgresql://127.0.0.1/override", "REDIS_SERVER_URL": "redis://127.0.0.1:6379"}), path)
		if err == nil {
			t.Fatalf("invalid synthetic configuration was accepted: %s", document)
		}
		if strings.Contains(err.Error(), "synthetic-secret") {
			t.Fatal("configuration contents leaked into an error")
		}
	}
	if _, err := loadFromPath(env(nil), filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("missing selected config file was silently ignored")
	}
}

func TestStartupDeploymentFallsBackForNonStringFileValues(t *testing.T) {
	for _, tc := range []struct {
		name, document string
	}{
		{"null type", `{"deployment":{"type":null}}`},
		{"number type", `{"deployment":{"type":42}}`},
		{"boolean type", `{"deployment":{"type":false}}`},
		{"object type", `{"deployment":{"type":{"nested":"cloud"}}}`},
		{"array type", `{"deployment":{"type":["cloud"]}}`},
		{"null module", `{"deployment":null}`},
		{"string module", `{"deployment":"cloud"}`},
		{"array module", `{"deployment":["cloud"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(tc.document), 0600); err != nil {
				t.Fatal(err)
			}
			for _, deployment := range []string{"selfhosted", "cloud"} {
				cfg, err := loadFromPath(env(map[string]string{"DEPLOYMENT_TYPE": deployment}), path)
				if err != nil {
					t.Fatal(err)
				}
				want := deployment
				if want == "cloud" {
					want = "affine"
				}
				if cfg.Deployment != want {
					t.Fatalf("deployment = %q, want environment fallback %q", cfg.Deployment, want)
				}
			}
			if _, err := loadFromPath(env(map[string]string{"DEPLOYMENT_TYPE": "invalid"}), path); err == nil {
				t.Fatal("fallback bypassed environment validation")
			}
		})
	}
}

func TestStartupRedisNumericEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name, document, port, database, want string
	}{
		{"environment zero port", `{"redis":{"port":6380,"db":4}}`, "0", "", "redis://localhost:0/4"},
		{"environment database eleven", `{"redis":{"port":6380,"db":4}}`, "", "11", "redis://localhost:6380/11"},
		{"unsigned limits", `{"redis":{"port":6380,"db":4}}`, "65535", "255", "redis://localhost:65535/255"},
		{"overflow fallback", `{"redis":{"port":6380,"db":4}}`, "65536", "256", "redis://localhost:6380/4"},
		{"whitespace fallback", `{"redis":{"port":6380,"db":4}}`, " 6381", "5 ", "redis://localhost:6380/4"},
		{"negative fallback", `{"redis":{"port":6380,"db":4}}`, "-0", "-0", "redis://localhost:6380/4"},
		{"explicit positive sign", `{"redis":{"port":6380,"db":4}}`, "+6381", "+5", "redis://localhost:6381/5"},
		{"invalid fallback", `{"redis":{"port":6380,"db":4}}`, "invalid", "invalid", "redis://localhost:6380/4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(tc.document), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := loadFromPath(env(map[string]string{"REDIS_SERVER_PORT": tc.port, "REDIS_SERVER_DATABASE": tc.database}), path)
			if err != nil || cfg.RedisURL != tc.want {
				t.Fatalf("Redis numeric source selection differs: got %q, error=%v", cfg.RedisURL, err)
			}
		})
	}
}

func TestReadWebsocketMaxHttpBufferSizeUsesLegacyConfigAndValidates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if got, err := ReadWebsocketMaxHttpBufferSize(context.Background(), nil, path); err != nil || got != DefaultWebsocketMaxHttpBufferSize {
		t.Fatalf("default websocket payload limit = %d, %v", got, err)
	}
	if err := os.WriteFile(path, []byte(`{"websocket":{"maxHttpBufferSize":4096}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadWebsocketMaxHttpBufferSize(context.Background(), nil, path); err != nil || got != 4096 {
		t.Fatalf("legacy config websocket payload limit = %d, %v", got, err)
	}
	if got, err := ReadWebsocketMaxHttpBufferSize(context.Background(), websocketConfigReader{value: []byte("8192")}, path); err != nil || got != 8192 {
		t.Fatalf("database websocket payload limit = %d, %v", got, err)
	}
	for _, value := range []string{"0", "-1", "null", "4096.5", `"4096"`} {
		t.Run(value, func(t *testing.T) {
			if _, err := ReadWebsocketMaxHttpBufferSize(context.Background(), websocketConfigReader{value: []byte(value)}, path); err == nil {
				t.Fatalf("invalid database websocket payload limit %s was accepted", value)
			}
		})
	}
}

func TestLoadRedisURLReadsLegacyFileAndEnvironmentOverrides(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"redis":{"host":"cache.example.test","port":6380,"db":4,"username":"file-user","password":"file-pass","ioredis":{"tls":{}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	fileURL, err := loadRedisURL(env(nil), configPath)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(fileURL)
	if err != nil || parsed.Scheme != "rediss" || parsed.Host != "cache.example.test:6380" || parsed.Path != "/4" || parsed.User.Username() != "file-user" {
		t.Fatalf("Redis URL from legacy config = %q, %v", fileURL, err)
	}
	password, _ := parsed.User.Password()
	if password != "file-pass" {
		t.Fatalf("Redis password was not preserved")
	}

	values := map[string]string{
		"REDIS_SERVER_HOST":     "env.example.test",
		"REDIS_SERVER_PORT":     "6381",
		"REDIS_SERVER_DATABASE": "3",
		"REDIS_SERVER_USERNAME": "env-user",
		"REDIS_SERVER_PASSWORD": "env-pass",
	}
	envURL, err := loadRedisURL(env(values), configPath)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err = url.Parse(envURL)
	if err != nil || parsed.Scheme != "rediss" || parsed.Host != "env.example.test:6381" || parsed.Path != "/3" || parsed.User.Username() != "env-user" {
		t.Fatalf("Redis URL from legacy environment = %q, %v", envURL, err)
	}
	password, _ = parsed.User.Password()
	if password != "env-pass" {
		t.Fatalf("Redis environment password was not preserved")
	}

	urlOverride, err := loadRedisURL(env(map[string]string{"REDIS_SERVER_URL": "redis://url-user:url-pass@url.example.test:6382/5"}), configPath)
	if err != nil || urlOverride != "redis://url-user:url-pass@url.example.test:6382/5" {
		t.Fatalf("REDIS_SERVER_URL override = %q, %v", urlOverride, err)
	}
}

func TestLoadRedisURLDoesNotExposeSecretsInErrors(t *testing.T) {
	_, err := loadRedisURL(env(map[string]string{"REDIS_SERVER_URL": "secret-not-a-url"}), "")
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("invalid Redis URL error = %v", err)
	}
}

func TestPublicURLsUseSharedConfiguredBaseAndOrigin(t *testing.T) {
	cases := []struct {
		name   string
		config Config
		base   string
		origin string
	}{
		{
			name:   "external base path",
			config: Config{ExternalURL: "https://notes.example.test/notes/v1/", Host: "internal", Port: 3010},
			base:   "https://notes.example.test/notes/v1",
			origin: "https://notes.example.test",
		},
		{
			name:   "loopback listener port",
			config: Config{Host: "localhost", Port: 3010, Path: "notes/v1"},
			base:   "http://localhost:3010/notes/v1",
			origin: "http://localhost:3010",
		},
		{
			name:   "public host ignores internal listener port",
			config: Config{Host: "notes.example.test", Port: 3010, HTTPS: true},
			base:   "https://notes.example.test:3010",
			origin: "https://notes.example.test",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := PublicBaseURL(test.config); got != test.base {
				t.Fatalf("public base URL = %q, want %q", got, test.base)
			}
			if got := PublicOrigin(test.config); got != test.origin {
				t.Fatalf("public origin = %q, want %q", got, test.origin)
			}
		})
	}
}

func TestLoadFromExistingServerEnvironment(t *testing.T) {
	got, err := LoadFrom(env(map[string]string{
		"NODE_ENV":                   "production",
		"AFFINE_ENV":                 "beta",
		"DEPLOYMENT_TYPE":            "selfhosted",
		"SERVER_FLAVOR":              "graphql",
		"LISTEN_ADDR":                "127.0.0.1",
		"AFFINE_SERVER_PORT":         "4321",
		"AFFINE_SERVER_SUB_PATH":     "/notes/v1/",
		"AFFINE_SERVER_HOST":         "notes.example.test",
		"AFFINE_SERVER_HOSTS":        "notes.example.test, localhost,",
		"AFFINE_SERVER_HTTPS":        "true",
		"AFFINE_SERVER_EXTERNAL_URL": "https://notes.example.test/notes/v1",
		"NOTES_SERVER_VERSION":       "0.1.0-test",
		"DATABASE_URL":               "postgresql://notes:test@localhost:5432/notes",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got.Flavor != "graphql" || got.Port != 4321 || got.Path != "notes/v1" {
		t.Fatalf("server config was not adapted: %+v", got)
	}
	if got.ExternalURL != "https://notes.example.test/notes/v1" || !got.HTTPS {
		t.Fatalf("external URL or HTTPS was not adapted: %+v", got)
	}
	if got.DatabaseURL != "postgresql://notes:test@localhost:5432/notes" {
		t.Fatalf("DATABASE_URL was not adapted: %q", got.DatabaseURL)
	}
	if strings.Join(got.Hosts, "|") != "notes.example.test|localhost" {
		t.Fatalf("hosts not parsed: %#v", got.Hosts)
	}
}

func TestLoadFromTrustedProxyCIDRs(t *testing.T) {
	got, err := LoadFrom(env(map[string]string{
		"TRUSTED_PROXY_CIDRS":      "10.0.0.0/8, 2001:db8:abcd::/48",
		"TRUSTED_CLOUDFLARE_CIDRS": "173.245.48.0/20",
		"TRUSTED_X_REAL_IP_CIDRS":  "192.0.2.0/24",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("2001:db8:abcd::/48"),
	}
	if len(got.TrustedProxyCIDRs) != len(want) {
		t.Fatalf("trusted proxy CIDRs = %#v, want %#v", got.TrustedProxyCIDRs, want)
	}
	for i := range want {
		if got.TrustedProxyCIDRs[i] != want[i] {
			t.Fatalf("trusted proxy CIDR %d = %q, want %q", i, got.TrustedProxyCIDRs[i], want[i])
		}
	}
	if len(got.TrustedCloudflareCIDRs) != 1 || got.TrustedCloudflareCIDRs[0] != netip.MustParsePrefix("173.245.48.0/20") {
		t.Fatalf("trusted Cloudflare CIDRs = %#v", got.TrustedCloudflareCIDRs)
	}
	if len(got.TrustedXRealIPCIDRs) != 1 || got.TrustedXRealIPCIDRs[0] != netip.MustParsePrefix("192.0.2.0/24") {
		t.Fatalf("trusted X-Real-IP CIDRs = %#v", got.TrustedXRealIPCIDRs)
	}
}

func TestLoadFromRejectsInvalidTrustedProxyCIDRs(t *testing.T) {
	for _, value := range []string{"not-a-cidr", "192.0.2.1", "10.0.0.0/33", "10.0.0.0/8,,192.0.2.0/24"} {
		t.Run(value, func(t *testing.T) {
			_, err := LoadFrom(env(map[string]string{"TRUSTED_PROXY_CIDRS": value}))
			if err == nil || strings.Contains(err.Error(), value) {
				t.Fatalf("invalid CIDR config error = %v", err)
			}
		})
	}
}

func TestLoadFromRejectsInvalidTrustedCloudflareCIDRs(t *testing.T) {
	for _, value := range []string{"not-a-cidr", "192.0.2.1", "10.0.0.0/33", "10.0.0.0/8,,192.0.2.0/24"} {
		t.Run(value, func(t *testing.T) {
			_, err := LoadFrom(env(map[string]string{"TRUSTED_CLOUDFLARE_CIDRS": value}))
			if err == nil || strings.Contains(err.Error(), value) {
				t.Fatalf("invalid Cloudflare CIDR config error = %v", err)
			}
		})
	}
}

func TestLoadFromRejectsInvalidTrustedXRealIPCIDRs(t *testing.T) {
	for _, value := range []string{"not-a-cidr", "192.0.2.1", "10.0.0.0/33", "10.0.0.0/8,,192.0.2.0/24"} {
		t.Run(value, func(t *testing.T) {
			_, err := LoadFrom(env(map[string]string{"TRUSTED_X_REAL_IP_CIDRS": value}))
			if err == nil || strings.Contains(err.Error(), value) {
				t.Fatalf("invalid X-Real-IP CIDR config error = %v", err)
			}
		})
	}
}

func TestLoadFromRejectsInvalidValues(t *testing.T) {
	cases := []map[string]string{
		{"SERVER_FLAVOR": "unknown"},
		{"AFFINE_SERVER_PORT": "0"},
		{"AFFINE_SERVER_PORT": "not-a-port"},
		{"AFFINE_SERVER_HTTPS": "yes"},
		{"AFFINE_SERVER_SUB_PATH": "../admin"},
		{"AFFINE_SERVER_EXTERNAL_URL": "/relative"},
		{"AFFINE_SERVER_EXTERNAL_URL": "https://user:secret@notes.example.test"},
		{"AFFINE_SERVER_EXTERNAL_URL": "https://notes.example.test?token=secret"},
		{"AFFINE_SERVER_EXTERNAL_URL": "https://notes.example.test#fragment"},
	}
	for _, values := range cases {
		t.Run(strings.Join(keys(values), ","), func(t *testing.T) {
			if _, err := LoadFrom(env(values)); err == nil {
				t.Fatalf("expected invalid config to fail: %#v", values)
			} else if strings.Contains(err.Error(), "secret") {
				t.Fatalf("configuration error exposed a secret: %v", err)
			}
		})
	}
}

func TestLoadFromLegacyFydeOSOAuthEnvironment(t *testing.T) {
	values := map[string]string{
		"OAUTH_FYDEOS_ACCOUNT_SERVER":        "https://account.example.test",
		"OAUTH_FYDEOS_CLIENT_ID":             "notes-client",
		"OAUTH_FYDEOS_CLIENT_SECRET":         "test-secret",
		"OAUTH_FYDEOS_ALLOW_PRIVATE_NETWORK": "true",
	}
	got, err := LoadFrom(env(values))
	if err != nil {
		t.Fatal(err)
	}
	if !got.FydeOSOAuth.Configured || got.FydeOSOAuth.AccountServer != values["OAUTH_FYDEOS_ACCOUNT_SERVER"] || got.FydeOSOAuth.ClientID != "notes-client" || got.FydeOSOAuth.ClientSecret != "test-secret" || !got.FydeOSOAuth.AllowPrivateNetwork {
		t.Fatalf("FydeOS OAuth environment = %+v", got.FydeOSOAuth)
	}
}

func TestLoadFromRejectsPartialFydeOSOAuthEnvironment(t *testing.T) {
	_, err := LoadFrom(env(map[string]string{"OAUTH_FYDEOS_CLIENT_ID": "partial"}))
	if err == nil || strings.Contains(err.Error(), "partial") {
		t.Fatalf("partial or secret-bearing error = %v", err)
	}

	_, err = LoadFrom(env(map[string]string{
		"OAUTH_FYDEOS_ACCOUNT_SERVER":        "https://account.example.test",
		"OAUTH_FYDEOS_CLIENT_ID":             "notes-client",
		"OAUTH_FYDEOS_CLIENT_SECRET":         "test-secret",
		"OAUTH_FYDEOS_ALLOW_PRIVATE_NETWORK": "tru",
	}))
	if err == nil || strings.Contains(err.Error(), "test-secret") {
		t.Fatalf("invalid boolean or secret-bearing error = %v", err)
	}
}

func keys(values map[string]string) []string {
	result := make([]string, 0, len(values))
	for key := range values {
		result = append(result, key)
	}
	return result
}
