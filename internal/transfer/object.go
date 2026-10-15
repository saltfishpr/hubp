package transfer

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"hubp/internal/cache"
)

type CacheStore interface {
	// Lookup 查找缓存条目；调用方负责关闭返回的条目。
	Lookup(context.Context, string) (*cache.Entry, error)
	// Begin 创建缓存写入任务，调用方负责提交或放弃。
	Begin(string, string) (*cache.Fill, error)
	// Refresh 更新已有缓存条目的响应头和有效期。
	Refresh(context.Context, string, http.Header, time.Duration) error
	// Invalidate 删除指定键的缓存索引。
	Invalidate(context.Context, string) error
}

type Resource struct {
	Key, Digest string
	TTL         time.Duration
}

// Object 优先使用有效缓存，对过期条目尝试条件验证，并在转发符合条件的完整响应时写入缓存。
// 缓存不可用或请求不允许缓存时直接转发上游响应。
func (s *Service) Object(w http.ResponseWriter, r *http.Request, res Resource, fetch FetchFunc) {
	if s.Cache == nil || r.Header.Get("Authorization") != "" || strings.Contains(strings.ToLower(r.Header.Get("Cache-Control")), "no-store") {
		s.Pipe(w, r, fetch)
		return
	}
	entry, err := s.Cache.Lookup(r.Context(), res.Key)
	if err != nil {
		s.Pipe(w, r, fetch)
		return
	}
	if entry != nil {
		defer entry.Close()
	}
	force := strings.Contains(strings.ToLower(r.Header.Get("Cache-Control")), "no-cache") || strings.Contains(r.Header.Get("Cache-Control"), "max-age=0")
	if entry != nil && entry.Fresh() && !force && !strings.Contains(r.Header.Get("Range"), ",") {
		serveEntry(w, r, entry)
		return
	}
	overrides := http.Header{}
	conditional := r.Header.Get("If-None-Match") != "" || r.Header.Get("If-Modified-Since") != "" || r.Header.Get("If-Match") != "" || r.Header.Get("If-Unmodified-Since") != ""
	revalidate := entry != nil && r.Method == http.MethodGet && r.Header.Get("Range") == "" && !conditional
	if revalidate {
		if v := entry.Headers.Get("ETag"); v != "" {
			overrides.Set("If-None-Match", v)
		} else if v := entry.Headers.Get("Last-Modified"); v != "" {
			overrides.Set("If-Modified-Since", v)
		}
	}
	resp, err := fetch(r.Context(), overrides)
	if err != nil {
		Error(w, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified && revalidate {
		hdr := entry.Headers.Clone()
		for _, key := range []string{"ETag", "Last-Modified", "Cache-Control", "Expires", "Date", "Age", "Vary"} {
			if v := resp.Header.Values(key); len(v) > 0 {
				hdr[http.CanonicalHeaderKey(key)] = v
			}
		}
		if cacheableHeaders(hdr) {
			_ = s.Cache.Refresh(r.Context(), res.Key, hdr, freshness(hdr, res.TTL))
		} else {
			_ = s.Cache.Invalidate(r.Context(), res.Key)
		}
		entry.Headers = hdr
		entry.Stored = time.Now()
		serveEntry(w, r, entry)
		return
	}
	if r.Method != http.MethodGet || r.Header.Get("Range") != "" || conditional || resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Range") != "" || !cacheableHeaders(resp.Header) {
		s.write(w, r, resp, nil)
		return
	}
	fill, err := s.Cache.Begin(res.Key, res.Digest)
	if err != nil {
		s.write(w, r, resp, nil)
		return
	}
	defer fill.Abort()
	filling := true
	s.write(w, r, resp, func(p []byte) {
		if filling {
			if _, e := fill.Write(p); e != nil {
				fill.Abort()
				filling = false
			}
		}
	})
	if filling {
		if err = fill.Commit(r.Context(), storedHeaders(resp.Header), freshness(resp.Header, res.TTL), resp.ContentLength); err != nil {
			// Digest/length failures invalidate the stream, not merely the cache entry.
			if errors.Is(err, cache.ErrIntegrity) {
				panic(http.ErrAbortHandler)
			}
		}
	}
}

func serveEntry(w http.ResponseWriter, r *http.Request, e *cache.Entry) {
	CopyHeaders(w.Header(), e.Headers)
	age, _ := strconv.ParseInt(e.Headers.Get("Age"), 10, 64)
	age += max(int64(time.Since(e.Stored)/time.Second), 0)
	if date, err := http.ParseTime(e.Headers.Get("Date")); err == nil {
		age = max(age, int64(time.Since(date)/time.Second))
	}
	w.Header().Set("Age", strconv.FormatInt(max(age, 0), 10))
	mod, _ := http.ParseTime(e.Headers.Get("Last-Modified"))
	http.ServeContent(w, r, "", mod, e.File)
}

func storedHeaders(h http.Header) http.Header {
	out := http.Header{}
	for _, k := range []string{"Content-Type", "Content-Length", "Content-Disposition", "Content-Encoding", "ETag", "Last-Modified", "Cache-Control", "Expires", "Date", "Age", "Vary", "Docker-Content-Digest", "Docker-Distribution-Api-Version", "X-Repo-Commit", "X-Linked-ETag", "X-Linked-Size", "Accept-Ranges"} {
		if v := h.Values(k); len(v) > 0 {
			out[http.CanonicalHeaderKey(k)] = append([]string(nil), v...)
		}
	}
	return out
}

func cacheableHeaders(h http.Header) bool {
	cc := strings.ToLower(h.Get("Cache-Control"))
	if strings.Contains(cc, "no-store") || strings.Contains(cc, "private") || h.Get("Set-Cookie") != "" {
		return false
	}
	for _, v := range h.Values("Vary") {
		for _, key := range strings.Split(v, ",") {
			switch strings.ToLower(strings.TrimSpace(key)) {
			case "", "accept", "accept-encoding", "authorization":
			default:
				return false
			}
		}
	}
	return true
}

func freshness(h http.Header, fallback time.Duration) time.Duration {
	if strings.Contains(strings.ToLower(h.Get("Cache-Control")), "no-cache") {
		return 0
	}
	age := time.Duration(0)
	if n, e := strconv.ParseInt(h.Get("Age"), 10, 64); e == nil && n > 0 {
		if n > int64(365*24*time.Hour/time.Second) {
			return 0
		}
		age = time.Duration(n) * time.Second
	}
	if date, e := http.ParseTime(h.Get("Date")); e == nil {
		age = max(age, time.Since(date))
	}
	// Never extend upstream freshness; immutable identity only affects the fallback.
	for _, p := range strings.Split(strings.ToLower(h.Get("Cache-Control")), ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
		if ok && (k == "max-age" || k == "s-maxage") {
			d, e := time.ParseDuration(strings.Trim(v, "\"") + "s")
			if e != nil || d < 0 {
				return 0
			}
			d = max(d-age, 0)
			if d < fallback {
				fallback = d
			}
		}
	}
	if exp, e := http.ParseTime(h.Get("Expires")); e == nil {
		if d := time.Until(exp); d < fallback {
			fallback = max(d, 0)
		}
	}
	return fallback
}
