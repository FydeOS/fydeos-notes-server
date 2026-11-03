// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package upgrades

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	semver "github.com/Masterminds/semver/v3"
)

const maxResponseBytes = 1 << 20

var canaryDateVersion = regexp.MustCompile(`(?i)^v?(\d{4})\.(\d{1,2})\.(\d{1,2})-canary\.(\d+)(?:\+.*)?$`)

type Release struct {
	Version     string
	URL         string
	PublishedAt time.Time
	Changelog   string
}

type Checker interface {
	Available(context.Context, string, string) (*Release, error)
}

type HTTPChecker struct {
	client   *http.Client
	endpoint string
}

type releaseResponse struct {
	Name        string  `json:"name"`
	URL         string  `json:"url"`
	Body        *string `json:"body"`
	PublishedAt string  `json:"published_at"`
}

func NewChecker(client *http.Client, endpoint string) *HTTPChecker {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &HTTPChecker{client: client, endpoint: endpoint}
}

// Available reports a newer release. It reports none when no release endpoint
// is configured.
func (c *HTTPChecker) Available(ctx context.Context, namespace, currentVersion string) (*Release, error) {
	if c.endpoint == "" {
		return nil, nil
	}
	target, err := url.Parse(c.endpoint)
	if err != nil {
		return nil, fmt.Errorf("parse release endpoint: %w", err)
	}
	query := target.Query()
	query.Set("channel", releaseChannel(namespace))
	target.RawQuery = query.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create release request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Cache-Control", "no-cache")

	response, err := c.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("request releases: %w", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read release response: %w", err)
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("release response exceeds %d bytes", maxResponseBytes)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("release API returned %s: %s", response.Status, string(body))
	}

	var releases []releaseResponse
	if err := json.Unmarshal(body, &releases); err != nil {
		return nil, fmt.Errorf("decode release response: %w", err)
	}
	if len(releases) == 0 || !hasNewerVersion(currentVersion, releases[0].Name) {
		return nil, nil
	}

	publishedAt, err := time.Parse(time.RFC3339Nano, releases[0].PublishedAt)
	if err != nil {
		return nil, fmt.Errorf("parse release published_at: %w", err)
	}
	changelog := ""
	if releases[0].Body != nil {
		changelog = *releases[0].Body
	}
	return &Release{
		Version:     releases[0].Name,
		URL:         releases[0].URL,
		PublishedAt: publishedAt.UTC(),
		Changelog:   changelog,
	}, nil
}

func releaseChannel(namespace string) string {
	switch namespace {
	case "dev":
		return "canary"
	case "beta":
		return "beta"
	default:
		return "stable"
	}
}

func hasNewerVersion(currentVersion, nextVersion string) bool {
	currentComparable := normalizeCanaryDateVersion(currentVersion)
	nextComparable := normalizeCanaryDateVersion(nextVersion)
	current, currentErr := semver.NewVersion(currentComparable)
	next, nextErr := semver.NewVersion(nextComparable)
	if currentErr != nil || nextErr != nil {
		return strings.TrimSpace(currentVersion) != strings.TrimSpace(nextVersion)
	}
	return next.GreaterThan(current)
}

func normalizeCanaryDateVersion(version string) string {
	match := canaryDateVersion.FindStringSubmatch(strings.TrimSpace(version))
	if match == nil {
		return strings.TrimSpace(version)
	}
	year, _ := strconv.Atoi(match[1])
	month, _ := strconv.Atoi(match[2])
	day, _ := strconv.Atoi(match[3])
	build := match[4]
	date := time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
	if year < 0 || month < 1 || month > 12 || day < 1 || day > 31 || date.Year() != year || int(date.Month()) != month || date.Day() != day {
		return strings.TrimSpace(version)
	}
	build = strings.TrimLeft(build, "0")
	if build == "" {
		build = "0"
	}
	return fmt.Sprintf("%d.%d.%d-canary.%s", year, month, day, build)
}
