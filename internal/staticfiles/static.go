// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package staticfiles

import (
	"context"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

type InitializedCheck func(context.Context) (bool, error)

type HandlerConfig struct {
	Root        string
	ServerPath  string
	Namespace   string
	Deployment  string
	Initialized InitializedCheck
}

type handler struct {
	root        string
	serverPath  string
	namespace   string
	deployment  string
	initialized InitializedCheck
}

var strictAssetPrefixes = []string{"/_plugin/", "/assets/", "/imgs/", "/js/", "/plugins/", "/static/"}

func NewHandler(config HandlerConfig) http.Handler {
	root, err := filepath.Abs(config.Root)
	if err != nil {
		root = config.Root
	}
	return &handler{
		root:        root,
		serverPath:  normalizeBasePath(config.ServerPath),
		namespace:   config.Namespace,
		deployment:  config.Deployment,
		initialized: config.Initialized,
	}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.NotFound(w, r)
		return
	}
	requestPath, ok := h.relativeRequestPath(r.URL.Path)
	if !ok || hasUnsafePathSegment(requestPath) {
		http.NotFound(w, r)
		return
	}

	if h.deployment == "selfhosted" {
		if requestPath == "/admin/index.html" {
			http.Redirect(w, r, h.routePath("/admin"), http.StatusFound)
			return
		}
		if requestPath == "/index.html" {
			http.Redirect(w, r, h.routePath("/"), http.StatusFound)
			return
		}
	}

	if strings.HasPrefix(requestPath, "/admin/") || requestPath == "/admin" {
		h.serveAdmin(w, r, requestPath)
		return
	}

	root := h.selectedRoot(r)
	if isStrictAssetPath(requestPath) {
		if h.serveFile(w, r, root, requestPath) {
			return
		}
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Cache-Control", "private, no-cache, no-store, max-age=0, must-revalidate")
	if h.serveFile(w, r, root, requestPath) {
		return
	}
	if h.redirectIfSetupRequired(w, r, requestPath) {
		return
	}
	if h.serveFile(w, r, root, "/"+h.indexName()) {
		return
	}
	http.NotFound(w, r)
}

func (h *handler) serveAdmin(w http.ResponseWriter, r *http.Request, requestPath string) {
	adminRoot := filepath.Join(h.root, "admin")
	if h.serveFile(w, r, adminRoot, strings.TrimPrefix(requestPath, "/admin")) {
		return
	}
	if h.redirectIfSetupRequired(w, r, requestPath) {
		return
	}
	if h.serveFile(w, r, adminRoot, "/"+h.indexName()) {
		return
	}
	http.NotFound(w, r)
}

func (h *handler) redirectIfSetupRequired(w http.ResponseWriter, r *http.Request, requestPath string) bool {
	if h.deployment != "selfhosted" || h.initialized == nil {
		return false
	}
	initialized, err := h.initialized(r.Context())
	if err != nil {
		return false
	}
	if !initialized && requestPath != "/admin/setup" {
		w.Header().Del("Cache-Control")
		legacyRedirect(w, r, h.routePath("/admin/setup"))
		return true
	}
	if initialized && requestPath == "/admin/setup" {
		legacyRedirect(w, r, h.routePath("/admin"))
		return true
	}
	return false
}

func legacyRedirect(w http.ResponseWriter, r *http.Request, target string) {
	w.Header().Set("Location", target)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Add("Vary", "Accept")
	w.WriteHeader(http.StatusFound)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte("Found. Redirecting to " + target))
	}
}

func (h *handler) selectedRoot(r *http.Request) string {
	if h.namespace == "dev" && isMobileRequest(r) {
		return filepath.Join(h.root, "mobile")
	}
	return h.root
}

func (h *handler) indexName() string {
	if h.deployment == "selfhosted" {
		return "selfhost.html"
	}
	return "index.html"
}

func (h *handler) serveFile(w http.ResponseWriter, r *http.Request, root, requestPath string) bool {
	filename := h.rootedFilePath(root, requestPath)
	if filename == "" {
		return false
	}
	file, err := os.Open(filename)
	if err != nil {
		return false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	if contentType := mime.TypeByExtension(filepath.Ext(filename)); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	http.ServeContent(w, r, filepath.Base(filename), info.ModTime(), file)
	return true
}

func (h *handler) rootedFilePath(root, requestPath string) string {
	if hasUnsafePathSegment(requestPath) || strings.Contains(requestPath, "\\") {
		return ""
	}
	relative := strings.TrimPrefix(requestPath, "/")
	if relative == "" {
		return ""
	}
	cleanRelative := path.Clean("/" + relative)
	filename := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(cleanRelative, "/")))
	rootPath, err := filepath.EvalSymlinks(root)
	if err != nil {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(filename)
	if err != nil {
		return ""
	}
	rel, err := filepath.Rel(rootPath, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	return resolved
}

func (h *handler) relativeRequestPath(requestPath string) (string, bool) {
	if h.serverPath == "" {
		if requestPath == "" {
			return "/", true
		}
		return requestPath, strings.HasPrefix(requestPath, "/")
	}
	if requestPath == h.serverPath {
		return "/", true
	}
	if strings.HasPrefix(requestPath, h.serverPath+"/") {
		return strings.TrimPrefix(requestPath, h.serverPath), true
	}
	return "", false
}

func (h *handler) routePath(requestPath string) string {
	if h.serverPath == "" {
		return requestPath
	}
	if requestPath == "/" {
		return h.serverPath
	}
	return h.serverPath + requestPath
}

func normalizeBasePath(value string) string {
	value = strings.TrimSpace(value)
	value = "/" + strings.Trim(value, "/")
	if value == "/" {
		return ""
	}
	return value
}

func hasUnsafePathSegment(requestPath string) bool {
	if strings.ContainsRune(requestPath, '\x00') || strings.Contains(requestPath, "\\") {
		return true
	}
	for _, segment := range strings.Split(requestPath, "/") {
		if segment == ".." || strings.HasPrefix(segment, ".") {
			return true
		}
	}
	return false
}

func isStrictAssetPath(requestPath string) bool {
	for _, prefix := range strictAssetPrefixes {
		if strings.HasPrefix(requestPath, prefix) {
			return true
		}
	}
	return false
}

func isMobileRequest(r *http.Request) bool {
	mobileHint := strings.Trim(strings.ToLower(r.Header.Get("Sec-CH-UA-Mobile")), `" `)
	if mobileHint != "" {
		return mobileHint == "?1" || mobileHint == "1" || mobileHint == "true"
	}
	platform := strings.Trim(strings.ToLower(r.Header.Get("Sec-CH-UA-Platform")), `" `)
	if platform == "android" || platform == "ios" || platform == "ipados" {
		return true
	}
	userAgent := strings.ToLower(r.UserAgent())
	if len(userAgent) > 512 {
		userAgent = userAgent[:512]
	}
	for _, token := range []string{"android", "iphone", "ipad", "ipod", "mobile", "tablet"} {
		if strings.Contains(userAgent, token) {
			return true
		}
	}
	return false
}
