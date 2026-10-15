package huggingface

import (
	"context"
	"errors"
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
	Origin       *url.URL
	Client       *upstream.Client
	Transfer     *transfer.Service
	TTL          time.Duration
	PublicScheme string
}

var (
	hosts        = []string{"huggingface.co", "cdn-lfs.huggingface.co", ".hf.co", ".huggingface.co"}
	resolve      = regexp.MustCompile(`^/((?:(?:datasets|spaces)/)?[^/]+(?:/[^/]+)?)/resolve/([^/]+)/(.+)$`)
	commit       = regexp.MustCompile(`^[a-f0-9]{40}$`)
	modelAPI     = regexp.MustCompile(`^/api/(models|datasets|spaces)(/[^/]+(?:/[^/]+)?(/revision/[^/]+|/tree/[^/]+(/.*)?)?)?/?$`)
	resolveCache = regexp.MustCompile(`^/api/resolve-cache/(models|datasets|spaces)/.+$`)
	absoluteLink = regexp.MustCompile(`<([^>]+)>`)
)

// ServeHTTP 处理 Hugging Face 公开仓库的元数据查询和文件下载，并将分页链接改写为代理地址。
// 仅支持 GET 和 HEAD，拒绝带认证信息的请求；HTTP 下载模式要求客户端禁用 Xet。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "read-only proxy", http.StatusMethodNotAllowed)
		return
	}
	if r.Header.Get("Authorization") != "" {
		http.Error(w, "MVP supports public repositories only", http.StatusForbidden)
		return
	}
	match := resolve.FindStringSubmatch(r.URL.EscapedPath())
	isFile := match != nil || resolveCache.MatchString(r.URL.EscapedPath())
	if !isFile && !modelAPI.MatchString(r.URL.EscapedPath()) {
		http.Error(w, "unsupported Hugging Face operation; HTTP mode requires HF_HUB_DISABLE_XET=1", http.StatusNotFound)
		return
	}
	target := *h.Origin
	target.Path = r.URL.Path
	target.RawPath = r.URL.RawPath
	target.RawQuery = r.URL.RawQuery
	endpoint := h.PublicScheme + "://" + r.Host
	fetch := func(ctx context.Context, headers http.Header) (*http.Response, error) {
		q := upstream.Request(r.WithContext(ctx), &target)
		for k, v := range headers {
			q.Header[k] = v
		}
		meta := http.Header{}
		resp, err := h.Client.Do(q, upstream.Options{Hosts: hosts, OnResponse: func(resp *http.Response) error {
			for _, key := range []string{"X-Repo-Commit", "X-Linked-ETag", "X-Linked-Size"} {
				if v := resp.Header.Get(key); v != "" && meta.Get(key) == "" {
					meta.Set(key, v)
				}
			}
			return nil
		}})
		if err != nil {
			return nil, err
		}
		for k, v := range meta {
			resp.Header[k] = v
		}
		// HTTP mode never advertises an external authentication/CAS path.
		for _, key := range []string{"X-Xet-Hash", "X-Xet-Refresh-Route", "X-Xet-Cas-Url", "X-Xet-Access-Token", "X-Xet-Token-Expiration"} {
			resp.Header.Del(key)
		}
		links := resp.Header.Values("Link")
		resp.Header.Del("Link")
		for _, link := range links {
			for _, item := range splitLinks(link) {
				if strings.Contains(item, `rel="xet-`) {
					continue
				}
				rewritten, err := rewriteLink(item, endpoint, h.Origin.Host)
				if err != nil {
					resp.Body.Close()
					return nil, err
				}
				resp.Header.Add("Link", rewritten)
			}
		}
		resp.Header.Del("Location")
		// Pin a branch HEAD to the resolved commit without exposing the CDN URL.
		if r.Method == http.MethodHead && match != nil && resp.StatusCode == http.StatusOK {
			if sha := meta.Get("X-Repo-Commit"); commit.MatchString(sha) && match[2] != sha {
				pinned, err := url.Parse("/" + match[1] + "/resolve/" + sha + "/" + match[3])
				if err != nil {
					resp.Body.Close()
					return nil, err
				}
				pinned.RawQuery = r.URL.RawQuery
				resp.Header.Set("Location", endpoint+pinned.String())
			}
		}
		// Public Hub errors and file responses do not set browser cookies on the proxy.
		return resp, nil
	}
	if !isFile || r.Method == http.MethodHead {
		h.Transfer.Pipe(w, r, fetch)
		return
	}
	ttl := h.TTL
	if match != nil && commit.MatchString(match[2]) {
		ttl = 365 * 24 * time.Hour
	}
	key := cache.Key("huggingface", target.EscapedPath(), target.RawQuery, r.Header.Get("Accept"), "public")
	h.Transfer.Object(w, r, transfer.Resource{Key: key, TTL: ttl}, fetch)
}

func splitLinks(s string) []string {
	var out []string
	start := 0
	inAngle := false
	inQuote := false
	for i, c := range s {
		switch c {
		case '<':
			if !inQuote {
				inAngle = true
			}
		case '>':
			if !inQuote {
				inAngle = false
			}
		case '"':
			inQuote = !inQuote
		case ',':
			if !inAngle && !inQuote {
				out = append(out, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		}
	}
	if strings.TrimSpace(s[start:]) != "" {
		out = append(out, strings.TrimSpace(s[start:]))
	}
	return out
}

func rewriteLink(link, endpoint, originHost string) (string, error) {
	var failed bool
	out := absoluteLink.ReplaceAllStringFunc(link, func(s string) string {
		u, err := url.Parse(s[1 : len(s)-1])
		if err != nil {
			failed = true
			return s
		}
		if !u.IsAbs() {
			return "<" + endpoint + u.String() + ">"
		}
		if u.Scheme != "https" || u.Host != originHost || u.User != nil {
			failed = true
			return s
		}
		return "<" + endpoint + u.RequestURI() + ">"
	})
	if failed {
		return "", errors.New("unsupported Hub pagination target")
	}
	return out, nil
}
