// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package renderer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"html"
	"log/slog"
	"mime"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/clientip"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

type PreviewExposure = workspaces.PreviewExposure

const (
	ExposureDenied          = workspaces.PreviewDenied
	ExposureMetadataNoIndex = workspaces.PreviewMetadataNoIndex
	ExposurePublicIndexable = workspaces.PreviewPublicIndexable
)

type PreviewResult = workspaces.PreviewResult

type PreviewReader interface {
	ReadDocumentMarkdown(context.Context, string, string) (string, bool, error)
	ReadDocumentPreview(context.Context, string, string) (PreviewResult, error)
	ReadWorkspacePreview(context.Context, string) (PreviewResult, error)
}

type ViewRecorder interface {
	RecordDocView(context.Context, string, string, string, bool, string, time.Time) error
}

type Assets struct {
	CSS         []string `json:"css"`
	JS          []string `json:"js"`
	PublicPath  string   `json:"publicPath"`
	GitHash     string   `json:"gitHash"`
	Description string   `json:"description"`
}

type Config struct {
	ServerPath             string
	Deployment             string
	Namespace              string
	PublicBaseURL          string
	WebAssets              Assets
	MobileAssets           Assets
	TrustedProxyCIDRs      []netip.Prefix
	TrustedCloudflareCIDRs []netip.Prefix
	TrustedXRealIPCIDRs    []netip.Prefix
}

type Handler struct {
	reader    PreviewReader
	analytics ViewRecorder
	config    Config
	viewSlots chan struct{}
}

var staticPaths = map[string]struct{}{
	"all": {}, "home": {}, "search": {}, "collection": {}, "tag": {}, "trash": {},
}

var markdownTypes = map[string]struct{}{
	"text/markdown": {}, "application/markdown": {}, "text/x-markdown": {},
}

func NewHandler(reader PreviewReader, analytics ViewRecorder, cfg Config) *Handler {
	return &Handler{reader: reader, analytics: analytics, config: cfg, viewSlots: make(chan struct{}, 32)}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	workspaceID, sub, isWorkspace := h.workspaceRoute(r.URL.Path)
	isDocPath := isWorkspace && workspaceID != sub
	if isDocPath {
		if _, ok := staticPaths[sub]; ok {
			isDocPath = false
			isWorkspace = false
		}
	}
	if isDocPath && acceptsMarkdown(strings.Join(r.Header.Values("Accept"), ",")) {
		if h.reader == nil {
			http.NotFound(w, r)
			return
		}
		markdown, present, err := h.reader.ReadDocumentMarkdown(r.Context(), workspaceID, sub)
		if err != nil {
			slog.WarnContext(r.Context(), "failed to render markdown document", "workspace_id", workspaceID, "doc_id", sub)
		}
		if err != nil || !present {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(markdown))
		return
	}

	assets := h.config.WebAssets
	if h.config.Namespace == "canary" && isMobile(r.UserAgent()) && h.config.MobileAssets.PublicPath != "" {
		assets = h.config.MobileAssets
	}
	result := PreviewResult{Exposure: ExposureDenied}
	if isWorkspace && h.reader != nil {
		var err error
		if workspaceID == sub {
			result, err = h.reader.ReadWorkspacePreview(r.Context(), workspaceID)
		} else {
			result, err = h.reader.ReadDocumentPreview(r.Context(), workspaceID, sub)
		}
		if err != nil {
			slog.WarnContext(r.Context(), "failed to load renderer preview metadata", "workspace_id", workspaceID, "doc_id", sub)
			result = PreviewResult{Exposure: ExposureDenied}
		}
		if result.Present && isDocPath {
			h.recordDocView(r, workspaceID, sub)
		}
		if result.Present && result.Avatar == "" && result.AvatarKey != "" && workspaceID == sub {
			result.Avatar = workspaceAvatarURL(h.config.PublicBaseURL, h.config.ServerPath, workspaceID, result.AvatarKey)
		}
	}

	indexable := result.Exposure == ExposurePublicIndexable
	if !indexable {
		w.Header().Set("X-Robots-Tag", "noindex")
	}
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(renderHTML(result, assets, h.config, indexable)))
}

func (h *Handler) workspaceRoute(requestPath string) (string, string, bool) {
	path := requestPath
	serverPath := "/" + strings.Trim(h.config.ServerPath, "/")
	if serverPath != "/" {
		if !strings.HasPrefix(path, serverPath+"/") {
			return "", "", false
		}
		path = strings.TrimPrefix(path, serverPath)
	}
	segments := strings.Split(strings.Trim(path, "/"), "/")
	if len(segments) != 3 || segments[0] != "workspace" || segments[1] == "" || segments[2] == "" {
		return "", "", false
	}
	return segments[1], segments[2], true
}

func acceptsMarkdown(value string) bool {
	for _, item := range strings.Split(value, ",") {
		mediaType, params, err := mime.ParseMediaType(strings.TrimSpace(item))
		if err != nil {
			continue
		}
		if _, ok := markdownTypes[strings.ToLower(mediaType)]; ok && acceptsQuality(params["q"]) {
			return true
		}
	}
	return false
}

func acceptsQuality(value string) bool {
	if value == "" {
		return true
	}
	quality, err := strconv.ParseFloat(value, 64)
	return err == nil && quality > 0 && quality <= 1
}

func isMobile(userAgent string) bool {
	value := strings.ToLower(userAgent)
	for _, marker := range []string{"mobile", "android", "iphone", "ipad", "ipod", "blackberry", "iemobile", "opera mini"} {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}

func (h *Handler) recordDocView(r *http.Request, workspace, doc string) {
	if h.analytics == nil {
		return
	}
	job := rendererViewJob{
		workspace: workspace,
		doc:       doc,
		visitor:   rendererVisitorID(r, workspace, doc, h.config),
		viewedAt:  time.Now().UTC(),
	}
	select {
	case h.viewSlots <- struct{}{}:
		go func() {
			defer func() { <-h.viewSlots }()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := h.analytics.RecordDocView(ctx, job.workspace, job.doc, job.visitor, true, "", job.viewedAt); err != nil {
				slog.WarnContext(ctx, "failed to record shared document view", "workspace_id", job.workspace, "doc_id", job.doc)
			}
		}()
	default:
		slog.Warn("dropping renderer view analytics because recorder is saturated", "workspace_id", workspace, "doc_id", doc)
	}
}

type rendererViewJob struct {
	workspace string
	doc       string
	visitor   string
	viewedAt  time.Time
}

func rendererVisitorID(r *http.Request, workspace, doc string, cfg Config) string {
	tracker := ""
	if cookie, err := r.Cookie("affine_session"); err == nil {
		tracker = cookie.Value
	}
	if tracker == "" {
		for _, value := range []string{r.Header.Get("CF-Connecting-IP"), firstForwardedIP(r.Header.Get("X-Forwarded-For")), r.Header.Get("X-Real-IP"), r.Header.Get("CF-Ray")} {
			if value = strings.TrimSpace(value); value != "" {
				tracker = value
				break
			}
		}
	}
	if tracker == "" {
		tracker = clientip.RequestClientIP(r, cfg.TrustedProxyCIDRs, cfg.TrustedCloudflareCIDRs, cfg.TrustedXRealIPCIDRs)
	}
	digest := sha256.Sum256([]byte(workspace + ":" + doc + ":" + tracker))
	return hex.EncodeToString(digest[:])
}

func firstForwardedIP(value string) string {
	first, _, _ := strings.Cut(value, ",")
	return strings.TrimSpace(first)
}

func workspaceAvatarURL(publicBaseURL, serverPath, workspace, key string) string {
	base := strings.TrimRight(publicBaseURL, "/")
	if base == "" {
		base = "/" + strings.Trim(serverPath, "/")
		base = strings.TrimRight(base, "/")
	}
	values := url.Values{"sourceType": {"currentDoc"}, "docId": {workspace}}
	return base + "/api/workspaces/" + url.PathEscape(workspace) + "/blobs/v1/" + url.PathEscape(key) + "?" + values.Encode()
}

// productName is shown in page titles and link previews.
const productName = "FydeOS Notes"

func renderHTML(result PreviewResult, assets Assets, cfg Config, indexable bool) string {
	publicPath := assets.PublicPath
	if publicPath == "" {
		publicPath = "/"
	}
	title := productName
	summary := assets.Description
	image := strings.TrimRight(cfg.PublicBaseURL, "/") + "/apple-touch-icon.png"
	if result.Present {
		if result.Title != "" {
			title = html.EscapeString(result.Title + " | " + productName)
		}
		summary = result.Summary
		if result.Avatar != "" {
			image = result.Avatar
		}
	}
	summary = html.EscapeString(summary)
	image = html.EscapeString(image)
	robots := ""
	if !indexable {
		robots = "<meta name=\"robots\" content=\"noindex, nofollow\" />"
	}
	envMeta := `<meta name="env:publicPath" content="` + html.EscapeString(publicPath) + `" />
<meta name="env:subPath" content="` + html.EscapeString(cfg.ServerPath) + `" />
<meta name="env:renderer" content="ssr" />`
	if cfg.Deployment == "selfhosted" {
		envMeta += "\n<meta name=\"env:isSelfHosted\" content=\"true\" />"
	}
	preconnect := ""
	if !strings.HasPrefix(publicPath, "/") {
		preconnect = `<link rel="preconnect" href="` + html.EscapeString(publicPath) + `" />`
	}
	css := renderAssetTags(assets.CSS, "stylesheet")
	js := renderAssetTags(assets.JS, "script")
	return `<!DOCTYPE html>
<html>
  <head>
    <meta charset="utf-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1, maximum-scale=1" />
    <meta name="mobile-web-app-capable" content="yes" />
    <meta name="apple-mobile-web-app-capable" content="yes" />
    <meta name="apple-mobile-web-app-status-bar-style" content="black-translucent" />
    <title>` + title + `</title>
    <meta name="theme-color" content="#fafafa" />
    ` + preconnect + `
    <link rel="manifest" href="/manifest.json" />
    <link rel="apple-touch-icon" sizes="180x180" href="/apple-touch-icon.png" />
    <link rel="icon" sizes="192x192" href="/favicon-192.png" />
    <link rel="shortcut icon" href="/favicon.ico?v=2" />
    <meta name="emotion-insertion-point" content="" />
    ` + robots + `
    <meta name="twitter:title" content="` + title + `" />
    <meta name="twitter:description" content="` + summary + `" />
    <meta name="twitter:image" content="` + image + `" />
    <meta property="og:title" content="` + title + `" />
    <meta property="og:description" content="` + summary + `" />
    <meta property="og:image" content="` + image + `" />
    ` + envMeta + `
    ` + css + `
  </head>
  <body>
    <div id="app" data-version="` + html.EscapeString(assets.GitHash) + `"></div>
    ` + js + `
  </body>
</html>
`
}

func renderAssetTags(paths []string, kind string) string {
	var b strings.Builder
	for _, path := range paths {
		path = html.EscapeString(path)
		if kind == "stylesheet" {
			b.WriteString(`<link rel="stylesheet" href="` + path + `" crossorigin />`)
		} else {
			b.WriteString(`<script src="` + path + `" crossorigin></script>`)
		}
		b.WriteByte('\n')
	}
	return b.String()
}
