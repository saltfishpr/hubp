package proxy

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/saltfishpr/hubp/internal/cache"
	"github.com/saltfishpr/hubp/internal/config"
)

type Handler struct {
	cfg          *config.Config
	cache        *cache.Cache
	logger       *slog.Logger
	registries   []*registry
	registryByID map[string]*registry
	defaultReg   *registry
	locks        *keyedLocker
}

type registry struct {
	cfg              config.RegistryConfig
	upstream         *url.URL
	client           *http.Client
	allowedAuthHosts map[string]struct{}
}

type resourceKind int

const (
	resourceOther resourceKind = iota
	resourceManifest
	resourceBlob
)

var realmPattern = regexp.MustCompile(`(?i)realm="([^"]+)"`)

func NewHandler(cfg *config.Config, logger *slog.Logger) (*Handler, error) {
	if logger == nil {
		logger = slog.Default()
	}
	h := &Handler{
		cfg:          cfg,
		logger:       logger,
		registryByID: make(map[string]*registry),
		locks:        newKeyedLocker(),
	}
	if cfg.Cache.Enabled {
		c, err := cache.New(cfg.Cache.Dir)
		if err != nil {
			return nil, err
		}
		h.cache = c
	}

	for _, rc := range cfg.SortedRegistries() {
		u, err := url.Parse(rc.Upstream)
		if err != nil {
			return nil, fmt.Errorf("parse upstream for %s: %w", rc.Name, err)
		}
		transport := &http.Transport{
			Proxy:                 nil,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          256,
			MaxIdleConnsPerHost:   64,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			DisableCompression:    true,
			TLSClientConfig: &tls.Config{
				MinVersion:         tls.VersionTLS12,
				InsecureSkipVerify: rc.TLSInsecureSkipVerify, //nolint:gosec -- explicit per-registry option
			},
		}
		if rc.HTTPSProxy != "" {
			proxyURL, err := url.Parse(rc.HTTPSProxy)
			if err != nil {
				return nil, fmt.Errorf("parse HTTPS proxy for %s: %w", rc.Name, err)
			}
			transport.Proxy = http.ProxyURL(proxyURL)
		}
		allowed := make(map[string]struct{}, len(rc.AllowedAuthHosts))
		for _, host := range rc.AllowedAuthHosts {
			allowed[strings.ToLower(host)] = struct{}{}
		}
		reg := &registry{
			cfg:              rc,
			upstream:         u,
			client:           &http.Client{Transport: transport},
			allowedAuthHosts: allowed,
		}
		h.registries = append(h.registries, reg)
		h.registryByID[rc.Name] = reg
		if rc.PathPrefix == "" {
			h.defaultReg = reg
		}
	}
	if h.defaultReg == nil {
		return nil, errors.New("default registry is not configured")
	}
	return h, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	switch {
	case r.URL.Path == "/-/healthz":
		h.handleHealth(w, r)
	case strings.HasPrefix(r.URL.Path, "/token/"):
		h.handleToken(w, r)
	case r.URL.Path == "/v2" || r.URL.Path == "/v2/":
		h.handlePing(w, r)
	case strings.HasPrefix(r.URL.Path, "/v2/"):
		h.handleRegistry(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	}
}

func (h *Handler) handlePing(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeRegistryError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "the operation is unsupported", nil)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) handleRegistry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeRegistryError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "this proxy is pull-only", nil)
		return
	}

	repo, operation, kind, reference, ok := splitRegistryPath(strings.TrimPrefix(r.URL.Path, "/v2/"))
	if !ok {
		writeRegistryError(w, http.StatusNotFound, "NAME_UNKNOWN", "unsupported registry path", nil)
		return
	}
	reg, upstreamRepo, ok := h.selectRegistry(repo)
	if !ok {
		writeRegistryError(w, http.StatusNotFound, "NAME_UNKNOWN", "repository name is invalid", nil)
		return
	}
	upstreamPath := "/v2/" + upstreamRepo + operation

	if h.cache == nil || kind == resourceOther {
		h.proxyDirect(w, r, reg, upstreamPath)
		return
	}

	key := cacheKey(reg.cfg.Name, upstreamPath, r.URL.RawQuery, kind, r.Header.Values("Accept"))
	ttl := time.Duration(0)
	if kind == resourceManifest && !strings.Contains(reference, ":") {
		ttl = h.cfg.Cache.ManifestTTL.Value()
	}
	if entry, file, hit, err := h.cache.Open(key, ttl); err == nil && hit {
		defer file.Close()
		cache.Serve(w, r, entry, file)
		return
	} else if err != nil {
		h.logger.Warn("cache lookup failed", "key", key, "error", err)
	}

	if r.Method == http.MethodHead || r.Header.Get("Range") != "" {
		h.proxyDirect(w, r, reg, upstreamPath)
		return
	}

	unlock := h.locks.Lock(key)
	defer unlock()
	if entry, file, hit, err := h.cache.Open(key, ttl); err == nil && hit {
		defer file.Close()
		cache.Serve(w, r, entry, file)
		return
	}
	h.fetchAndCache(w, r, reg, upstreamPath, key)
}

func (h *Handler) proxyDirect(w http.ResponseWriter, r *http.Request, reg *registry, upstreamPath string) {
	resp, err := h.doUpstream(r.Context(), r, reg, upstreamPath)
	if err != nil {
		h.logger.Error("upstream request failed", "registry", reg.cfg.Name, "path", upstreamPath, "error", err)
		writeRegistryError(w, http.StatusBadGateway, "UNKNOWN", "upstream request failed", nil)
		return
	}
	defer resp.Body.Close()
	copyResponseHeaders(w.Header(), resp.Header)
	if resp.StatusCode == http.StatusUnauthorized {
		h.rewriteAuthChallenge(w.Header(), r, reg)
	}
	w.Header().Set("X-Hubp-Cache", "BYPASS")
	w.WriteHeader(resp.StatusCode)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, resp.Body)
	}
}

func (h *Handler) fetchAndCache(w http.ResponseWriter, r *http.Request, reg *registry, upstreamPath, key string) {
	resp, err := h.doUpstream(r.Context(), r, reg, upstreamPath)
	if err != nil {
		h.logger.Error("upstream request failed", "registry", reg.cfg.Name, "path", upstreamPath, "error", err)
		writeRegistryError(w, http.StatusBadGateway, "UNKNOWN", "upstream request failed", nil)
		return
	}
	defer resp.Body.Close()

	copyResponseHeaders(w.Header(), resp.Header)
	if resp.StatusCode == http.StatusUnauthorized {
		h.rewriteAuthChallenge(w.Header(), r, reg)
	}
	if resp.StatusCode != http.StatusOK {
		w.Header().Set("X-Hubp-Cache", "BYPASS")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}

	temp, err := h.cache.NewTemp(key)
	if err != nil {
		h.logger.Warn("create cache temp file failed", "key", key, "error", err)
		w.Header().Set("X-Hubp-Cache", "MISS-NOT-STORED")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}
	tempName := temp.Name()
	committed := false
	defer func() {
		_ = temp.Close()
		if !committed {
			_ = removeFile(tempName)
		}
	}()

	w.Header().Set("X-Hubp-Cache", "MISS")
	w.WriteHeader(resp.StatusCode)
	written, copyErr := io.Copy(io.MultiWriter(w, temp), resp.Body)
	if copyErr != nil {
		h.logger.Warn("streaming upstream response failed", "key", key, "error", copyErr)
		return
	}
	if resp.ContentLength >= 0 && written != resp.ContentLength {
		h.logger.Warn("upstream response length mismatch", "key", key, "expected", resp.ContentLength, "actual", written)
		return
	}
	if err := temp.Sync(); err != nil {
		h.logger.Warn("sync cache temp file failed", "key", key, "error", err)
		return
	}
	if err := temp.Close(); err != nil {
		h.logger.Warn("close cache temp file failed", "key", key, "error", err)
		return
	}
	entry := cache.Entry{
		Status:    resp.StatusCode,
		Headers:   cacheableHeaders(resp.Header, written),
		CreatedAt: time.Now().UTC(),
	}
	if err := h.cache.Commit(key, tempName, entry); err != nil {
		h.logger.Warn("commit cache entry failed", "key", key, "error", err)
		return
	}
	committed = true
}

func (h *Handler) doUpstream(ctx context.Context, incoming *http.Request, reg *registry, upstreamPath string) (*http.Response, error) {
	u := *reg.upstream
	u.Path = upstreamPath
	u.RawPath = ""
	u.RawQuery = incoming.URL.RawQuery
	req, err := http.NewRequestWithContext(ctx, incoming.Method, u.String(), nil)
	if err != nil {
		return nil, err
	}
	copyRequestHeaders(req.Header, incoming.Header)
	if authorization := incoming.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(authorization), "basic ") && !reg.cfg.ForwardClientAuthorization {
		req.Header.Del("Authorization")
	}
	req.Host = reg.upstream.Host
	return reg.client.Do(req)
}

func (h *Handler) rewriteAuthChallenge(header http.Header, incoming *http.Request, reg *registry) {
	values := header.Values("Www-Authenticate")
	if len(values) == 0 {
		values = header.Values("WWW-Authenticate")
	}
	if len(values) == 0 {
		return
	}
	header.Del("Www-Authenticate")
	header.Del("WWW-Authenticate")
	for _, challenge := range values {
		match := realmPattern.FindStringSubmatch(challenge)
		if len(match) != 2 {
			header.Add("WWW-Authenticate", challenge)
			continue
		}
		realmURL, err := url.Parse(match[1])
		if err != nil || !reg.authHostAllowed(realmURL) {
			h.logger.Warn("refusing to rewrite untrusted auth realm", "registry", reg.cfg.Name, "realm", match[1])
			header.Add("WWW-Authenticate", challenge)
			continue
		}
		encoded := base64.RawURLEncoding.EncodeToString([]byte(realmURL.String()))
		localRealm := h.publicBaseURL(incoming) + "/token/" + url.PathEscape(reg.cfg.Name) + "/" + encoded
		rewritten := realmPattern.ReplaceAllString(challenge, `realm="`+localRealm+`"`)
		header.Add("WWW-Authenticate", rewritten)
	}
}

func (h *Handler) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/token/"), "/", 2)
	if len(parts) != 2 {
		http.Error(w, "invalid token route", http.StatusBadRequest)
		return
	}
	reg := h.registryByID[parts[0]]
	if reg == nil {
		http.Error(w, "unknown registry", http.StatusNotFound)
		return
	}
	realmBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		http.Error(w, "invalid token realm", http.StatusBadRequest)
		return
	}
	realm, err := url.Parse(string(realmBytes))
	if err != nil || !reg.authHostAllowed(realm) {
		http.Error(w, "token realm is not allowed", http.StatusForbidden)
		return
	}
	query := realm.Query()
	for key, values := range r.URL.Query() {
		query.Del(key)
		for _, value := range values {
			query.Add(key, value)
		}
	}
	realm.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(r.Context(), r.Method, realm.String(), nil)
	if err != nil {
		http.Error(w, "invalid token request", http.StatusBadRequest)
		return
	}
	if accept := r.Header.Get("Accept"); accept != "" {
		req.Header.Set("Accept", accept)
	}
	if userAgent := r.Header.Get("User-Agent"); userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}
	if reg.cfg.Username != "" {
		req.SetBasicAuth(reg.cfg.Username, reg.cfg.Password)
	} else if reg.cfg.ForwardClientAuthorization {
		if authorization := r.Header.Get("Authorization"); authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
	}
	resp, err := reg.client.Do(req)
	if err != nil {
		h.logger.Error("token request failed", "registry", reg.cfg.Name, "error", err)
		http.Error(w, "upstream token request failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	copyResponseHeaders(w.Header(), resp.Header)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, resp.Body)
	}
}

func (r *registry) authHostAllowed(u *url.URL) bool {
	if u == nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	hostPort := strings.ToLower(u.Host)
	if _, ok := r.allowedAuthHosts[hostPort]; ok {
		return true
	}
	if u.Port() == "" || u.Port() == "443" {
		if _, ok := r.allowedAuthHosts[host]; ok {
			return true
		}
	}
	for allowed := range r.allowedAuthHosts {
		if strings.HasPrefix(allowed, "*.") && strings.HasSuffix(host, strings.TrimPrefix(allowed, "*")) {
			return true
		}
	}
	return false
}

func (h *Handler) publicBaseURL(r *http.Request) string {
	if h.cfg.Server.PublicURL != "" {
		return h.cfg.Server.PublicURL
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if forwarded := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]); forwarded == "http" || forwarded == "https" {
		scheme = forwarded
	}
	return scheme + "://" + r.Host
}

func (h *Handler) selectRegistry(repo string) (*registry, string, bool) {
	for _, reg := range h.registries {
		prefix := reg.cfg.PathPrefix
		if prefix == "" {
			continue
		}
		if strings.HasPrefix(repo, prefix+"/") {
			upstreamRepo := strings.TrimPrefix(repo, prefix+"/")
			return reg, upstreamRepo, upstreamRepo != ""
		}
	}
	return h.defaultReg, repo, repo != ""
}

func splitRegistryPath(rest string) (repo, operation string, kind resourceKind, reference string, ok bool) {
	type marker struct {
		value string
		kind  resourceKind
	}
	markers := []marker{
		{value: "/manifests/", kind: resourceManifest},
		{value: "/blobs/", kind: resourceBlob},
		{value: "/referrers/", kind: resourceOther},
		{value: "/tags/list", kind: resourceOther},
	}
	best := -1
	var selected marker
	for _, candidate := range markers {
		if idx := strings.LastIndex(rest, candidate.value); idx > best {
			best = idx
			selected = candidate
		}
	}
	if best <= 0 {
		return "", "", resourceOther, "", false
	}
	repo = rest[:best]
	operation = rest[best:]
	if strings.Contains(repo, "..") || strings.HasPrefix(repo, "/") || strings.HasSuffix(repo, "/") {
		return "", "", resourceOther, "", false
	}
	kind = selected.kind
	if kind == resourceManifest || kind == resourceBlob {
		reference = strings.TrimPrefix(operation, selected.value)
		if reference == "" || strings.Contains(reference, "/") {
			return "", "", resourceOther, "", false
		}
	}
	return repo, operation, kind, reference, true
}

func cacheKey(registryName, path, rawQuery string, kind resourceKind, accepts []string) string {
	hash := sha256.New()
	_, _ = io.WriteString(hash, registryName)
	_, _ = io.WriteString(hash, "\n")
	_, _ = io.WriteString(hash, path)
	_, _ = io.WriteString(hash, "\n")
	_, _ = io.WriteString(hash, rawQuery)
	if kind == resourceManifest {
		_, _ = io.WriteString(hash, "\n")
		_, _ = io.WriteString(hash, strings.Join(accepts, ","))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func cacheableHeaders(src http.Header, size int64) map[string][]string {
	allowed := map[string]struct{}{
		"Content-Type":          {},
		"Content-Encoding":      {},
		"Docker-Content-Digest": {},
		"Etag":                  {},
		"Last-Modified":         {},
		"Cache-Control":         {},
	}
	out := make(map[string][]string)
	for key, values := range src {
		canonical := http.CanonicalHeaderKey(key)
		if _, ok := allowed[canonical]; ok {
			out[canonical] = append([]string(nil), values...)
		}
	}
	out["Content-Length"] = []string{strconv.FormatInt(size, 10)}
	return out
}

var hopHeaders = map[string]struct{}{
	"Connection":          {},
	"Proxy-Connection":    {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Te":                  {},
	"Trailer":             {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
}

func copyRequestHeaders(dst, src http.Header) {
	connectionTokens := connectionHeaderTokens(src)
	for key, values := range src {
		canonical := http.CanonicalHeaderKey(key)
		if _, blocked := hopHeaders[canonical]; blocked {
			continue
		}
		if _, blocked := connectionTokens[canonical]; blocked {
			continue
		}
		for _, value := range values {
			dst.Add(canonical, value)
		}
	}
}

func copyResponseHeaders(dst, src http.Header) {
	connectionTokens := connectionHeaderTokens(src)
	for key, values := range src {
		canonical := http.CanonicalHeaderKey(key)
		if _, blocked := hopHeaders[canonical]; blocked {
			continue
		}
		if _, blocked := connectionTokens[canonical]; blocked {
			continue
		}
		for _, value := range values {
			dst.Add(canonical, value)
		}
	}
}

func connectionHeaderTokens(header http.Header) map[string]struct{} {
	out := make(map[string]struct{})
	for _, value := range header.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			if token = strings.TrimSpace(token); token != "" {
				out[http.CanonicalHeaderKey(token)] = struct{}{}
			}
		}
	}
	return out
}

type registryErrorEnvelope struct {
	Errors []registryError `json:"errors"`
}

type registryError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Detail  any    `json:"detail,omitempty"`
}

func writeRegistryError(w http.ResponseWriter, status int, code, message string, detail any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(registryErrorEnvelope{Errors: []registryError{{Code: code, Message: message, Detail: detail}}})
}

func removeFile(path string) error {
	if path == "" {
		return nil
	}
	return os.Remove(path)
}

type keyedLocker struct {
	mu    sync.Mutex
	locks map[string]*lockRef
}

type lockRef struct {
	mu   sync.Mutex
	refs int
}

func newKeyedLocker() *keyedLocker {
	return &keyedLocker{locks: make(map[string]*lockRef)}
}

func (k *keyedLocker) Lock(key string) func() {
	k.mu.Lock()
	ref := k.locks[key]
	if ref == nil {
		ref = &lockRef{}
		k.locks[key] = ref
	}
	ref.refs++
	k.mu.Unlock()

	ref.mu.Lock()
	return func() {
		ref.mu.Unlock()
		k.mu.Lock()
		ref.refs--
		if ref.refs == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
}
