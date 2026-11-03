// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package maildelivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/config"
)

type SMTPConfig struct {
	Name      string
	Host      string
	Port      int
	Username  string
	Password  string
	Secure    bool
	IgnoreTLS bool
	Sender    string
}

type Settings struct {
	SMTP            SMTPConfig
	FallbackSMTP    SMTPConfig
	FallbackDomains []string
	BatchSize       int
	Lease           time.Duration
	Retention       time.Duration
}

type EnvironmentLookup func(string) (string, bool)

func LoadSettings(ctx context.Context, reader config.RuntimeConfigReader, cfg config.Config, lookup EnvironmentLookup) (Settings, error) {
	if lookup == nil {
		return Settings{}, errors.New("mail delivery environment lookup is required")
	}
	read := func(key, envName, fallback string) (string, error) {
		if envName != "" {
			if value, ok := lookup(envName); ok {
				return value, nil
			}
		}
		raw, found, err := config.ReadRuntimeValue(ctx, reader, cfg.RuntimeConfigPath, "mailer."+key)
		if err != nil || !found {
			if err != nil {
				return "", err
			}
			return fallback, nil
		}
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			return "", fmt.Errorf("decode mailer configuration %s", key)
		}
		if value == nil {
			return "", nil
		}
		return fmt.Sprint(value), nil
	}
	readBool := func(key, envName string, fallback bool) (bool, error) {
		if envName != "" {
			if raw, ok := lookup(envName); ok {
				value, err := strconv.ParseBool(raw)
				if err != nil {
					return false, fmt.Errorf("invalid %s", envName)
				}
				return value, nil
			}
		}
		raw, found, err := config.ReadRuntimeValue(ctx, reader, cfg.RuntimeConfigPath, "mailer."+key)
		if err != nil || !found {
			return fallback, err
		}
		var value *bool
		if err := json.Unmarshal(raw, &value); err != nil || value == nil {
			return false, fmt.Errorf("invalid mailer configuration %s", key)
		}
		return *value, nil
	}
	readInt := func(key, envName string, fallback int) (int, error) {
		value, err := read(key, envName, strconv.Itoa(fallback))
		if err != nil {
			return 0, err
		}
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return 0, fmt.Errorf("invalid mailer configuration %s", key)
		}
		return parsed, nil
	}
	readSMTP := func(prefix string, envPrefix string, defaults SMTPConfig) (SMTPConfig, error) {
		var result SMTPConfig
		var err error
		if result.Name, err = read(prefix+".name", envPrefix+"SERVERNAME", defaults.Name); err != nil {
			return SMTPConfig{}, err
		}
		if result.Host, err = read(prefix+".host", envPrefix+"HOST", defaults.Host); err != nil {
			return SMTPConfig{}, err
		}
		if result.Port, err = readInt(prefix+".port", envPrefix+"PORT", defaults.Port); err != nil {
			return SMTPConfig{}, err
		}
		if result.Username, err = read(prefix+".username", envPrefix+"USER", defaults.Username); err != nil {
			return SMTPConfig{}, err
		}
		if result.Password, err = read(prefix+".password", envPrefix+"PASSWORD", defaults.Password); err != nil {
			return SMTPConfig{}, err
		}
		secureEnv, ignoreTLSEnv := envPrefix+"SECURE", envPrefix+"IGNORE_TLS"
		if result.Secure, err = readBool(prefix+".secure", secureEnv, defaults.Secure); err != nil {
			return SMTPConfig{}, err
		}
		if result.IgnoreTLS, err = readBool(prefix+".ignoreTLS", ignoreTLSEnv, defaults.IgnoreTLS); err != nil {
			return SMTPConfig{}, err
		}
		if result.Sender, err = read(prefix+".sender", envPrefix+"SENDER", defaults.Sender); err != nil {
			return SMTPConfig{}, err
		}
		if result.Port < 1 || result.Port > 65535 {
			return SMTPConfig{}, fmt.Errorf("invalid mailer configuration %s.port", prefix)
		}
		return result, nil
	}
	primary, err := readSMTP("SMTP", "MAILER_", SMTPConfig{Port: 465, Sender: "FydeOS Notes <noreply@example.com>"})
	if err != nil {
		return Settings{}, err
	}
	fallback, err := readSMTP("fallbackSMTP", "", SMTPConfig{Port: 465})
	if err != nil {
		return Settings{}, err
	}
	batchSize, err := readInt("deliveryWorker.batchSize", "", 50)
	if err != nil || batchSize < 1 || batchSize > 1000 {
		return Settings{}, errors.New("invalid mailer delivery worker batch size")
	}
	leaseMS, err := readInt("deliveryWorker.leaseMs", "", 2*60*1000)
	if err != nil || leaseMS < 1000 {
		return Settings{}, errors.New("invalid mailer delivery worker lease")
	}
	retentionDays, err := readInt("deliveryWorker.retentionDays", "", 30)
	if err != nil || retentionDays < 1 {
		return Settings{}, errors.New("invalid mailer delivery retention")
	}
	fallbackRaw, found, err := config.ReadRuntimeValue(ctx, reader, cfg.RuntimeConfigPath, "mailer.fallbackDomains")
	if err != nil {
		return Settings{}, err
	}
	domains := []string{}
	if found {
		if err := json.Unmarshal(fallbackRaw, &domains); err != nil {
			return Settings{}, errors.New("invalid mailer fallback domains")
		}
	}
	return Settings{SMTP: primary, FallbackSMTP: fallback, FallbackDomains: domains, BatchSize: batchSize,
		Lease: time.Duration(leaseMS) * time.Millisecond, Retention: time.Duration(retentionDays) * 24 * time.Hour}, nil
}
