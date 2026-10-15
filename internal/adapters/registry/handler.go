package registry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"hubp/internal/cache"
	"hubp/internal/transfer"
	"hubp/internal/upstream"
)

type Handler struct {
	Client   *upstream.Client
	Transfer *transfer.Service
	Origin   *url.URL
	TTL      time.Duration
	hosts    []string
	tokens   tokenStore
}

// New 创建 Registry 适配器，并按源站配置允许访问的主机和独立的令牌缓存。
func New(origin *url.URL, client *upstream.Client, delivery *transfer.Service, ttl time.Duration) *Handler {
	hosts := []string{origin.Hostname()}
	switch origin.Hostname() {
	case "registry-1.docker.io":
		hosts = append(hosts, "auth.docker.io", "production.cloudfront.docker.com", "production.cloudflare.docker.com", ".r2.cloudflarestorage.com")
	case "ghcr.io":
		hosts = append(hosts, "pkg-containers.githubusercontent.com")
	case "gcr.io":
		hosts = append(hosts, ".gcr.io", "storage.googleapis.com", ".googleusercontent.com")
	case "quay.io":
		hosts = append(hosts, "cdn01.quay.io", "cdn02.quay.io", "cdn03.quay.io", "cdn04.quay.io", "cdn05.quay.io", "cdn06.quay.io")
	case "registry.k8s.io":
		hosts = append(hosts, "cdn.registry.k8s.io", ".pkg.dev", ".gcr.io", ".amazonaws.com")
	}
	return &Handler{Client: client, Transfer: delivery, Origin: origin, TTL: ttl, hosts: hosts, tokens: tokenStore{entries: map[string]tokenEntry{}}}
}

var (
	route    = regexp.MustCompile(`^/v2/(.+)/(manifests|blobs)/([^/]+)$`)
	repoPart = regexp.MustCompile(`^[a-z0-9]+(?:[._-]+[a-z0-9]+)*$`)
	tag      = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	digest   = regexp.MustCompile(`^(?:sha256:[a-f0-9]{64}|sha512:[a-f0-9]{128})$`)
)

// ServeHTTP 处理 Registry V2 探测及 manifest、blob 的只读请求，并校验内容摘要、应用缓存策略。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		failure(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "read-only proxy")
		return
	}
	if r.URL.Path == "/v2/" || r.URL.Path == "/v2" {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodHead {
			_, _ = w.Write([]byte("{}"))
		}
		return
	}
	match := route.FindStringSubmatch(r.URL.Path)
	if match == nil {
		failure(w, http.StatusNotFound, "UNSUPPORTED", "unsupported Registry operation")
		return
	}
	repo, kind, ref := match[1], match[2], match[3]
	for _, part := range strings.Split(repo, "/") {
		if !repoPart.MatchString(part) {
			failure(w, 400, "NAME_INVALID", "invalid repository")
			return
		}
	}
	if !digest.MatchString(ref) && (kind == "blobs" || !tag.MatchString(ref)) {
		failure(w, 400, "DIGEST_INVALID", "invalid reference")
		return
	}
	target := *h.Origin
	target.Path = r.URL.Path
	target.RawPath = r.URL.RawPath
	target.RawQuery = r.URL.RawQuery
	expected := ""
	ttl := h.TTL
	if digest.MatchString(ref) {
		expected = ref
		ttl = 365 * 24 * time.Hour
	}
	fetch := func(ctx context.Context, headers http.Header) (*http.Response, error) {
		resp, err := h.fetch(r.WithContext(ctx), &target, repo, headers)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized {
			resp.Header.Del("WWW-Authenticate")
		}
		verify := expected
		if verify == "" && kind == "manifests" {
			verify = resp.Header.Get("Docker-Content-Digest")
		}
		if err = upstream.Verify(resp, verify); err != nil {
			resp.Body.Close()
			return nil, err
		}
		return resp, nil
	}
	key := cache.Key("registry", h.Origin.Host, repo, kind, ref, r.URL.RawQuery, r.Header.Get("Accept"), "public")
	h.Transfer.Object(w, r, transfer.Resource{Key: key, Digest: expected, TTL: ttl}, fetch)
}

func failure(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"code": code, "message": msg}}})
}
