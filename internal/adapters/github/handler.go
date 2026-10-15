package github

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"hubp/internal/cache"
	"hubp/internal/transfer"
	"hubp/internal/upstream"
)

type Handler struct {
	Origin   *url.URL
	Client   *upstream.Client
	Transfer *transfer.Service
	TTL      time.Duration
}

var targets = []string{"github.com", "raw.githubusercontent.com", "codeload.github.com", ".githubusercontent.com"}

// ServeHTTP 处理 GitHub 公开仓库的只读 Git 请求和文件下载，文件响应按缓存策略交付。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "" {
		http.Error(w, "MVP supports public repositories only", http.StatusForbidden)
		return
	}
	raw := strings.TrimPrefix(r.URL.EscapedPath(), "/")
	if r.URL.RawQuery != "" {
		raw += "?" + r.URL.RawQuery
	}
	u, err := url.Parse(raw)
	if err != nil || !upstream.Allowed(u, []string{h.Origin.Hostname(), "raw.githubusercontent.com", "codeload.github.com"}) {
		http.Error(w, "unsupported GitHub URL", http.StatusBadRequest)
		return
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for _, p := range parts {
		if p == "" || p == "." || p == ".." {
			http.Error(w, "invalid resource path", http.StatusBadRequest)
			return
		}
	}
	git := false
	valid := false
	switch u.Hostname() {
	case "raw.githubusercontent.com":
		valid = len(parts) >= 4
	case "codeload.github.com":
		valid = len(parts) >= 4 && (parts[2] == "zip" || parts[2] == "tar.gz" || parts[2] == "legacy.zip" || parts[2] == "legacy.tar.gz")
	case h.Origin.Hostname():
		if len(parts) == 4 && parts[2] == "info" && parts[3] == "refs" {
			git = true
			valid = r.Method == http.MethodGet && u.Query().Get("service") == "git-upload-pack" && len(u.Query()["service"]) == 1
		} else if len(parts) == 3 && parts[2] == "git-upload-pack" {
			git = true
			valid = r.Method == http.MethodPost
		} else if len(parts) >= 4 && parts[2] == "archive" {
			valid = true
		} else if len(parts) >= 6 && parts[2] == "releases" && (parts[3] == "download" || parts[3] == "latest" && parts[4] == "download") {
			valid = true
		}
	}
	if !valid {
		http.Error(w, "unsupported GitHub operation", http.StatusNotFound)
		return
	}
	if !git && r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "read-only proxy", http.StatusMethodNotAllowed)
		return
	}
	fetch := func(ctx context.Context, headers http.Header) (*http.Response, error) {
		q := upstream.Request(r.WithContext(ctx), u)
		for k, v := range headers {
			q.Header[k] = v
		}
		return h.Client.Do(q, upstream.Options{Hosts: targets})
	}
	if git {
		h.Transfer.Pipe(w, r, fetch)
		return
	}
	key := cache.Key("github", u.Hostname(), u.EscapedPath(), u.RawQuery, r.Header.Get("Accept"), "public")
	h.Transfer.Object(w, r, transfer.Resource{Key: key, TTL: h.TTL}, fetch)
}
