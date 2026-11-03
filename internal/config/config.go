// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package config

import (
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	NodeEnv                     string
	Namespace                   string
	Deployment                  string
	Flavor                      string
	Version                     string
	ListenAddr                  string
	Port                        int
	Path                        string
	Host                        string
	Hosts                       []string
	HTTPS                       bool
	ExternalURL                 string
	DatabaseURL                 string
	RedisURL                    string
	RuntimeConfigPath           string
	WebsocketMaxHttpBufferSize  int64
	TrustedProxyCIDRs           []netip.Prefix
	TrustedCloudflareCIDRs      []netip.Prefix
	TrustedXRealIPCIDRs         []netip.Prefix
	FydeOSOAuth                 FydeOSOAuthEnvironment
	FydeOSManagedCopilot        bool
	FydeOSManagedCopilotAPIKey  string
	FydeOSManagedCopilotBaseURL string
	FydeOSManagedCopilotModel   string
}

const DefaultWebsocketMaxHttpBufferSize int64 = 100_000_000

func PublicBaseURL(cfg Config) string {
	if cfg.ExternalURL != "" {
		return strings.TrimRight(cfg.ExternalURL, "/")
	}

	scheme := "http"
	defaultPort := 80
	if cfg.HTTPS {
		scheme = "https"
		defaultPort = 443
	}
	host := cfg.Host
	if host == "" {
		host = "localhost"
	}
	if _, _, err := net.SplitHostPort(host); err != nil && cfg.Port != 0 && cfg.Port != defaultPort {
		host = net.JoinHostPort(strings.Trim(host, "[]"), strconv.Itoa(cfg.Port))
	}
	basePath := strings.Trim(cfg.Path, "/")
	if basePath != "" {
		basePath = "/" + basePath
	}
	return (&url.URL{Scheme: scheme, Host: host, Path: basePath}).String()
}

func PublicOrigin(cfg Config) string {
	if cfg.ExternalURL != "" {
		parsed, err := url.Parse(cfg.ExternalURL)
		if err == nil {
			parsed.Path = ""
			parsed.RawPath = ""
			parsed.RawQuery = ""
			parsed.ForceQuery = false
			parsed.Fragment = ""
			defaultPort := "80"
			if parsed.Scheme == "https" {
				defaultPort = "443"
			}
			if parsed.Port() == defaultPort {
				parsed.Host = hostWithoutPort(parsed.Hostname())
			}
			return strings.TrimRight(parsed.String(), "/")
		}
	}
	scheme := "http"
	if cfg.HTTPS {
		scheme = "https"
	}
	host := cfg.Host
	if host == "" {
		host = "localhost"
	}
	parsed, err := url.Parse("//" + host)
	if err != nil || parsed.Hostname() == "" {
		return ""
	}
	defaultPort := 80
	if cfg.HTTPS {
		defaultPort = 443
	}
	if parsed.Port() == strconv.Itoa(defaultPort) {
		parsed.Host = hostWithoutPort(parsed.Hostname())
	} else if parsed.Port() == "" && cfg.Port != 0 && cfg.Port != defaultPort && isLoopbackHost(parsed.Hostname()) {
		parsed.Host = net.JoinHostPort(parsed.Hostname(), strconv.Itoa(cfg.Port))
	}
	parsed.Scheme = scheme
	return parsed.Scheme + "://" + parsed.Host
}

func hostWithoutPort(host string) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

type FydeOSOAuthEnvironment struct {
	Configured          bool
	AccountServer       string
	ClientID            string
	ClientSecret        string
	AllowPrivateNetwork bool
}

type LookupEnv func(string) (string, bool)

func Load() (Config, error) {
	configPath, err := startupConfigPath()
	if err != nil {
		return Config{}, err
	}
	return loadFromPath(os.LookupEnv, configPath)
}

func LoadFrom(lookup LookupEnv) (Config, error) {
	return loadFromPath(lookup, "")
}

func loadFromPath(lookup LookupEnv, configPath string) (Config, error) {
	if lookup == nil {
		return Config{}, fmt.Errorf("environment lookup is required")
	}
	root, err := readStartupDocument(configPath)
	if err != nil {
		return Config{}, err
	}

	nodeEnv := valueOr(lookup, "NODE_ENV", "production")
	if !oneOf(nodeEnv, "development", "test", "production") {
		return Config{}, fmt.Errorf("invalid NODE_ENV %q", nodeEnv)
	}

	namespace := valueOr(lookup, "AFFINE_ENV", "production")
	if !oneOf(namespace, "dev", "beta", "production") {
		return Config{}, fmt.Errorf("invalid AFFINE_ENV %q", namespace)
	}

	flavor := valueOr(lookup, "SERVER_FLAVOR", "allinone")
	if !oneOf(flavor, "allinone", "graphql", "sync", "renderer", "front", "worker", "script") {
		return Config{}, fmt.Errorf("invalid SERVER_FLAVOR %q", flavor)
	}

	deployment := valueOr(lookup, "DEPLOYMENT_TYPE", "selfhosted")
	if nodeEnv == "development" && !present(lookup, "DEPLOYMENT_TYPE") {
		deployment = "affine"
	}
	// The legacy selector only overrides the environment for a string leaf.
	if module, ok := root["deployment"].(map[string]any); ok {
		if selected, ok := module["type"].(string); ok {
			if !oneOf(selected, "cloud", "selfhosted") {
				return Config{}, fmt.Errorf("invalid runtime configuration deployment.type")
			}
			deployment = selected
		}
	}
	if deployment == "cloud" {
		deployment = "affine"
	}
	if !oneOf(deployment, "affine", "selfhosted") {
		return Config{}, fmt.Errorf("invalid DEPLOYMENT_TYPE %q", deployment)
	}

	port := 3010
	if raw, ok := lookup("AFFINE_SERVER_PORT"); ok {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 65535 {
			return Config{}, fmt.Errorf("invalid AFFINE_SERVER_PORT %q", raw)
		}
		port = parsed
	}

	https := false
	if raw, ok := lookup("AFFINE_SERVER_HTTPS"); ok {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("invalid AFFINE_SERVER_HTTPS %q", raw)
		}
		https = parsed
	}

	path := strings.Trim(valueOr(lookup, "AFFINE_SERVER_SUB_PATH", ""), "/")
	if strings.Contains(path, "?") || strings.Contains(path, "#") || strings.Contains(path, "..") {
		return Config{}, fmt.Errorf("invalid AFFINE_SERVER_SUB_PATH %q", path)
	}

	externalURL := valueOr(lookup, "AFFINE_SERVER_EXTERNAL_URL", "")
	if externalURL != "" {
		parsed, err := url.ParseRequestURI(externalURL)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
			return Config{}, fmt.Errorf("AFFINE_SERVER_EXTERNAL_URL must be an absolute base URL without userinfo, query, or fragment")
		}
	}

	hosts := splitList(valueOr(lookup, "AFFINE_SERVER_HOSTS", ""))
	trustedProxyCIDRs, err := loadCIDRs(lookup, "TRUSTED_PROXY_CIDRS")
	if err != nil {
		return Config{}, err
	}
	trustedCloudflareCIDRs, err := loadCIDRs(lookup, "TRUSTED_CLOUDFLARE_CIDRS")
	if err != nil {
		return Config{}, err
	}
	trustedXRealIPCIDRs, err := loadCIDRs(lookup, "TRUSTED_X_REAL_IP_CIDRS")
	if err != nil {
		return Config{}, err
	}
	fydeOSOAuth, err := loadFydeOSOAuthEnvironment(lookup)
	if err != nil {
		return Config{}, err
	}
	fydeOSManagedCopilot, err := loadFydeOSManagedCopilot(lookup)
	if err != nil {
		return Config{}, err
	}
	redisURL, err := redisURLFromDocument(lookup, root)
	if err != nil {
		return Config{}, err
	}
	databaseURL := nonBlankValueOr(lookup, "DATABASE_URL", "")
	if value, found, err := runtimeDocumentValue(root, "db.datasourceUrl"); err != nil {
		return Config{}, fmt.Errorf("invalid runtime configuration db.datasourceUrl")
	} else if found && value != nil {
		configured, ok := value.(string)
		if !ok {
			return Config{}, fmt.Errorf("invalid runtime configuration db.datasourceUrl")
		}
		if databaseURL == "" && strings.TrimSpace(configured) != "" {
			databaseURL = configured
		}
	}
	if databaseURL == "" {
		databaseURL = "postgresql://localhost:5432/affine"
	}
	cfg := Config{
		NodeEnv:                     nodeEnv,
		Namespace:                   namespace,
		Deployment:                  deployment,
		Flavor:                      flavor,
		Version:                     valueOr(lookup, "NOTES_SERVER_VERSION", "0.27.5"),
		ListenAddr:                  valueOr(lookup, "LISTEN_ADDR", "0.0.0.0"),
		Port:                        port,
		Path:                        path,
		Host:                        valueOr(lookup, "AFFINE_SERVER_HOST", "localhost"),
		Hosts:                       hosts,
		HTTPS:                       https,
		ExternalURL:                 externalURL,
		DatabaseURL:                 databaseURL,
		RedisURL:                    redisURL,
		RuntimeConfigPath:           configPath,
		WebsocketMaxHttpBufferSize:  DefaultWebsocketMaxHttpBufferSize,
		TrustedProxyCIDRs:           trustedProxyCIDRs,
		TrustedCloudflareCIDRs:      trustedCloudflareCIDRs,
		TrustedXRealIPCIDRs:         trustedXRealIPCIDRs,
		FydeOSOAuth:                 fydeOSOAuth,
		FydeOSManagedCopilot:        fydeOSManagedCopilot,
		FydeOSManagedCopilotAPIKey:  strings.TrimSpace(valueOr(lookup, "COPILOT_OPENAI_API_KEY", "")),
		FydeOSManagedCopilotBaseURL: strings.TrimRight(strings.TrimSpace(valueOr(lookup, "COPILOT_OPENAI_BASE_URL", "")), "/"),
		FydeOSManagedCopilotModel:   strings.TrimSpace(valueOr(lookup, "COPILOT_DEFAULT_CHAT_MODEL", "")),
	}
	if err := applyStartupServerConfig(&cfg, root); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func loadFydeOSManagedCopilot(lookup LookupEnv) (bool, error) {
	apiKey := strings.TrimSpace(valueOr(lookup, "COPILOT_OPENAI_API_KEY", ""))
	baseURL := strings.TrimSpace(valueOr(lookup, "COPILOT_OPENAI_BASE_URL", ""))
	model := strings.TrimSpace(valueOr(lookup, "COPILOT_DEFAULT_CHAT_MODEL", ""))
	dialect := strings.TrimSpace(valueOr(lookup, "COPILOT_OPENAI_API_DIALECT", ""))
	if apiKey == "" && baseURL == "" && model == "" && dialect == "" {
		return false, nil
	}
	if apiKey == "" || baseURL == "" || model == "" {
		return false, fmt.Errorf("managed Copilot requires COPILOT_OPENAI_API_KEY, COPILOT_OPENAI_BASE_URL, and COPILOT_DEFAULT_CHAT_MODEL together")
	}
	if len(model) > 512 || strings.IndexFunc(model, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return false, fmt.Errorf("COPILOT_DEFAULT_CHAT_MODEL is invalid")
	}
	parsed, err := url.ParseRequestURI(baseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false, fmt.Errorf("COPILOT_OPENAI_BASE_URL is invalid")
	}
	if dialect != "" && dialect != "chat_completions" && dialect != "responses" {
		return false, fmt.Errorf("COPILOT_OPENAI_API_DIALECT is invalid")
	}
	if (strings.Contains(strings.ToLower(parsed.Hostname()), "deepseek") || strings.Contains(strings.ToLower(parsed.Hostname()), "moonshot")) && dialect == "responses" {
		return false, fmt.Errorf("DeepSeek and Kimi managed endpoints require chat_completions")
	}
	return true, nil
}

func readStartupDocument(configPath string) (map[string]any, error) {
	if configPath == "" {
		return nil, nil
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("unable to read runtime config")
	}
	return decodeRuntimeDocument(data)
}

// Legacy Node server settings use environment-derived defaults, then file
// overrides. Native deployment, database and Redis have their own precedence.
func applyStartupServerConfig(cfg *Config, root map[string]any) error {
	value, present := root["server"]
	if !present {
		return nil
	}
	server, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("invalid runtime configuration server")
	}
	for key, target := range map[string]*string{
		"host": &cfg.Host, "listenAddr": &cfg.ListenAddr, "path": &cfg.Path, "externalUrl": &cfg.ExternalURL,
	} {
		value, present := server[key]
		if !present {
			continue
		}
		if key == "externalUrl" && value == nil {
			*target = ""
			continue
		}
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("invalid runtime configuration server.%s", key)
		}
		*target = text
	}
	if value, present := server["https"]; present {
		flag, ok := value.(bool)
		if !ok {
			return fmt.Errorf("invalid runtime configuration server.https")
		}
		cfg.HTTPS = flag
	}
	if value, present := server["port"]; present {
		number, ok := value.(json.Number)
		if !ok {
			return fmt.Errorf("invalid runtime configuration server.port")
		}
		port, err := strconv.Atoi(number.String())
		if err != nil || port < 1 || port > 65535 {
			return fmt.Errorf("invalid runtime configuration server.port")
		}
		cfg.Port = port
	}
	if value, present := server["hosts"]; present {
		list, ok := value.([]any)
		if !ok {
			return fmt.Errorf("invalid runtime configuration server.hosts")
		}
		cfg.Hosts = make([]string, 0, len(list))
		for _, value := range list {
			host, ok := value.(string)
			if !ok {
				return fmt.Errorf("invalid runtime configuration server.hosts")
			}
			cfg.Hosts = append(cfg.Hosts, host)
		}
	}
	cfg.Path = strings.Trim(cfg.Path, "/")
	if strings.ContainsAny(cfg.Path, "?#") || strings.Contains(cfg.Path, "..") {
		return fmt.Errorf("invalid runtime configuration server.path")
	}
	if cfg.ExternalURL != "" {
		parsed, err := url.ParseRequestURI(cfg.ExternalURL)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
			return fmt.Errorf("invalid runtime configuration server.externalUrl")
		}
	}
	return nil
}

type redisConfigFile struct {
	Host     string `json:"host"`
	Port     uint16 `json:"port"`
	DB       uint8  `json:"db"`
	Username string `json:"username"`
	Password string `json:"password"`
	IORedis  struct {
		TLS json.RawMessage `json:"tls"`
	} `json:"ioredis"`
}

func startupConfigPath() (string, error) {
	if path := strings.TrimSpace(os.Getenv("AFFINE_BACKEND_RUNTIME_CONFIG_PATH")); path != "" {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			return "", fmt.Errorf("unable to inspect runtime config")
		}
		return path, nil
	}
	if _, err := os.Stat("config.json"); err == nil {
		return "config.json", nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("unable to inspect runtime config")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("unable to locate runtime config")
	}
	path := filepath.Join(home, ".affine", "config", "config.json")
	if _, err := os.Stat(path); err == nil {
		return path, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("unable to inspect runtime config")
	}
	return "", nil
}

func loadRedisURL(lookup LookupEnv, configPath string) (string, error) {
	root, err := readStartupDocument(configPath)
	if err != nil {
		return "", err
	}
	return redisURLFromDocument(lookup, root)
}

func redisURLFromDocument(lookup LookupEnv, root map[string]any) (string, error) {
	file := redisConfigFile{Host: "localhost", Port: 6379}
	if value, present := root["redis"]; present {
		module, ok := value.(map[string]any)
		if !ok {
			return "", fmt.Errorf("invalid runtime configuration redis")
		}
		for _, key := range []string{"host", "port", "db", "username", "password", "ioredis"} {
			if value, present := module[key]; present && value == nil {
				return "", fmt.Errorf("invalid runtime configuration redis.%s", key)
			}
		}
		raw, err := json.Marshal(module)
		if err != nil || json.Unmarshal(raw, &file) != nil || file.Port == 0 || file.DB > 10 {
			return "", fmt.Errorf("invalid runtime configuration redis")
		}
	}

	if raw, ok := lookup("REDIS_SERVER_URL"); ok && strings.TrimSpace(raw) != "" {
		parsed, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || (parsed.Scheme != "redis" && parsed.Scheme != "rediss") || parsed.Hostname() == "" {
			return "", fmt.Errorf("invalid Redis URL")
		}
		return parsed.String(), nil
	}

	host := nonBlankValueOr(lookup, "REDIS_SERVER_HOST", file.Host)
	if strings.TrimSpace(host) == "" {
		return "redis://localhost:6379/0", nil
	}
	port := int(file.Port)
	if raw, ok := lookup("REDIS_SERVER_PORT"); ok && raw != "" {
		if parsed, err := strconv.ParseUint(strings.TrimPrefix(raw, "+"), 10, 16); err == nil {
			port = int(parsed)
		}
	}
	database := int(file.DB)
	if raw, ok := lookup("REDIS_SERVER_DATABASE"); ok && raw != "" {
		if parsed, err := strconv.ParseUint(strings.TrimPrefix(raw, "+"), 10, 8); err == nil {
			database = int(parsed)
		}
	}
	username := nonBlankValueOr(lookup, "REDIS_SERVER_USERNAME", nonBlankValueOr(lookup, "REDIS_SERVER_USER", file.Username))
	password := nonBlankValueOr(lookup, "REDIS_SERVER_PASSWORD", file.Password)
	scheme := "redis"
	if len(file.IORedis.TLS) > 0 && string(file.IORedis.TLS) != "null" {
		scheme = "rediss"
	}
	parsed := &url.URL{Scheme: scheme, Host: net.JoinHostPort(host, strconv.Itoa(port)), Path: "/" + strconv.Itoa(database)}
	if username != "" {
		parsed.User = url.UserPassword(username, password)
	} else if password != "" {
		parsed.User = url.UserPassword("", password)
	}
	return parsed.String(), nil
}

func loadFydeOSOAuthEnvironment(lookup LookupEnv) (FydeOSOAuthEnvironment, error) {
	accountServer, hasAccountServer := lookup("OAUTH_FYDEOS_ACCOUNT_SERVER")
	clientID, hasClientID := lookup("OAUTH_FYDEOS_CLIENT_ID")
	clientSecret, hasClientSecret := lookup("OAUTH_FYDEOS_CLIENT_SECRET")
	allowPrivateNetworkRaw, hasAllowPrivateNetwork := lookup("OAUTH_FYDEOS_ALLOW_PRIVATE_NETWORK")
	configured := hasAccountServer || hasClientID || hasClientSecret || hasAllowPrivateNetwork
	if !configured {
		return FydeOSOAuthEnvironment{}, nil
	}
	if !hasAccountServer || !hasClientID || !hasClientSecret || strings.TrimSpace(accountServer) == "" || strings.TrimSpace(clientID) == "" || strings.TrimSpace(clientSecret) == "" {
		return FydeOSOAuthEnvironment{}, fmt.Errorf("FydeOS OAuth requires OAUTH_FYDEOS_ACCOUNT_SERVER, OAUTH_FYDEOS_CLIENT_ID, and OAUTH_FYDEOS_CLIENT_SECRET together")
	}
	allowPrivateNetwork := false
	if hasAllowPrivateNetwork {
		var err error
		allowPrivateNetwork, err = strconv.ParseBool(allowPrivateNetworkRaw)
		if err != nil || (allowPrivateNetworkRaw != "true" && allowPrivateNetworkRaw != "false") {
			return FydeOSOAuthEnvironment{}, fmt.Errorf("OAUTH_FYDEOS_ALLOW_PRIVATE_NETWORK must be true or false")
		}
	}
	return FydeOSOAuthEnvironment{
		Configured:          true,
		AccountServer:       accountServer,
		ClientID:            clientID,
		ClientSecret:        clientSecret,
		AllowPrivateNetwork: allowPrivateNetwork,
	}, nil
}

func loadCIDRs(lookup LookupEnv, key string) ([]netip.Prefix, error) {
	raw, ok := lookup(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return nil, nil
	}

	parts := strings.Split(raw, ",")
	prefixes := make([]netip.Prefix, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		prefix, err := netip.ParsePrefix(part)
		if part == "" || err != nil {
			return nil, fmt.Errorf("%s must contain only valid IP prefixes", key)
		}
		if prefix.Addr().Is4In6() {
			if prefix.Bits() < 96 {
				return nil, fmt.Errorf("%s must contain only valid IP prefixes", key)
			}
			prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96)
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	return prefixes, nil
}

func valueOr(lookup LookupEnv, key, fallback string) string {
	value, ok := lookup(key)
	if !ok {
		return fallback
	}
	return value
}

func nonBlankValueOr(lookup LookupEnv, key, fallback string) string {
	value, ok := lookup(key)
	if !ok || strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func present(lookup LookupEnv, key string) bool {
	_, ok := lookup(key)
	return ok
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func splitList(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			result = append(result, part)
		}
	}
	return result
}
