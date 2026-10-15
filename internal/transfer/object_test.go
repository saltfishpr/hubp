package transfer

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"hubp/internal/cache"
)

func service(t *testing.T, max int64) *Service {
	t.Helper()
	s, e := cache.Open(cache.Config{Dir: t.TempDir(), MaxBytes: max, MaxObjectBytes: max})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return &Service{Cache: s, WriteTimeout: time.Second}
}
func resp(status int, h http.Header, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body))}
}
func call(s *Service, method string, h http.Header, res Resource, fetch FetchFunc) *httptest.ResponseRecorder {
	q := httptest.NewRequest(method, "http://proxy.example/file", nil)
	q.Header = h
	w := httptest.NewRecorder()
	s.Object(w, q, res, fetch)
	return w
}
func TestCachedHeadRangeAndConditional(t *testing.T) {
	s := service(t, 1024)
	calls := 0
	res := Resource{Key: "k", TTL: time.Hour}
	fetch := func(context.Context, http.Header) (*http.Response, error) {
		calls++
		return resp(200, http.Header{"Etag": {"\"v1\""}, "Content-Type": {"text/plain"}}, "0123456789"), nil
	}
	if w := call(s, "GET", http.Header{}, res, fetch); w.Code != 200 || w.Body.String() != "0123456789" {
		t.Fatal(w)
	}
	w := call(s, "GET", http.Header{"Range": {"bytes=2-4"}}, res, fetch)
	if w.Code != 206 || w.Body.String() != "234" || w.Header().Get("Content-Range") != "bytes 2-4/10" {
		t.Fatal(w)
	}
	w = call(s, "HEAD", http.Header{}, res, fetch)
	if w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("Content-Length") != "10" {
		t.Fatal(w)
	}
	w = call(s, "GET", http.Header{"If-None-Match": {"\"v1\""}}, res, fetch)
	if w.Code != 304 {
		t.Fatal(w.Code)
	}
	if calls != 1 {
		t.Fatal("unexpected origin calls", calls)
	}
}
func TestColdRangesAndPrivateResponsesNeverFill(t *testing.T) {
	for _, kind := range []string{"range", "private", "no-store", "head"} {
		t.Run(kind, func(t *testing.T) {
			s := service(t, 1024)
			res := Resource{Key: "k", TTL: time.Hour}
			calls := 0
			method := "GET"
			reqHeaders := http.Header{}
			fetch := func(context.Context, http.Header) (*http.Response, error) {
				calls++
				h := http.Header{}
				status := 200
				if kind == "range" {
					status = 206
					h.Set("Content-Range", "bytes 0-2/10")
				}
				if kind == "private" || kind == "no-store" {
					h.Set("Cache-Control", kind)
				}
				return resp(status, h, "abc"), nil
			}
			if kind == "range" {
				reqHeaders.Set("Range", "bytes=0-2")
			}
			if kind == "head" {
				method = "HEAD"
			}
			_ = call(s, method, reqHeaders, res, fetch)
			_ = call(s, method, reqHeaders, res, fetch)
			if calls != 2 {
				t.Fatal("unsafe response was cached")
			}
		})
	}
}
func TestRevalidationKeepsBytesAndUpdatesValidator(t *testing.T) {
	s := service(t, 1024)
	fill, e := s.Cache.Begin("k", "")
	if e != nil {
		t.Fatal(e)
	}
	_, _ = fill.Write([]byte("payload"))
	e = fill.Commit(context.Background(), http.Header{"Etag": {"\"old\""}}, -time.Second, 7)
	if e != nil {
		t.Fatal(e)
	}
	calls := 0
	fetch := func(_ context.Context, h http.Header) (*http.Response, error) {
		calls++
		if h.Get("If-None-Match") != "\"old\"" {
			t.Fatal(h)
		}
		return resp(304, http.Header{"Etag": {"\"new\""}}, ""), nil
	}
	w := call(s, "GET", http.Header{}, Resource{Key: "k", TTL: time.Hour}, fetch)
	if w.Code != 200 || w.Body.String() != "payload" || w.Header().Get("ETag") != "\"new\"" {
		t.Fatal(w)
	}
	_ = call(s, "GET", http.Header{}, Resource{Key: "k", TTL: time.Hour}, fetch)
	if calls != 1 {
		t.Fatal("304 did not refresh")
	}
}
func TestCacheCapacityFailureDoesNotBreakDownload(t *testing.T) {
	s := service(t, 3)
	fetch := func(context.Context, http.Header) (*http.Response, error) {
		return resp(200, http.Header{}, "large payload"), nil
	}
	w := call(s, "GET", http.Header{}, Resource{Key: "k", TTL: time.Hour}, fetch)
	if w.Code != 200 || w.Body.String() != "large payload" {
		t.Fatal(w)
	}
	entry, e := s.Cache.Lookup(context.Background(), "k")
	if e != nil || entry != nil {
		t.Fatal("partial cache published", e)
	}
}

func TestRevalidationNoStoreInvalidatesMapping(t *testing.T) {
	s := service(t, 1024)
	f, e := s.Cache.Begin("k", "")
	if e != nil {
		t.Fatal(e)
	}
	_, _ = f.Write([]byte("old"))
	if e = f.Commit(context.Background(), http.Header{"Etag": {"\"v1\""}}, -time.Second, 3); e != nil {
		t.Fatal(e)
	}
	fetch := func(context.Context, http.Header) (*http.Response, error) {
		return resp(304, http.Header{"Cache-Control": {"no-store"}}, ""), nil
	}
	_ = call(s, "GET", http.Header{}, Resource{Key: "k", TTL: time.Hour}, fetch)
	if entry, e := s.Cache.Lookup(context.Background(), "k"); e != nil || entry != nil {
		t.Fatal("no-store mapping retained", e)
	}
}

func TestFreshnessRespectsAgeAndExpires(t *testing.T) {
	h := http.Header{}
	h.Set("Cache-Control", "max-age=60")
	h.Set("Age", "59")
	if d := freshness(h, time.Hour); d != time.Second {
		t.Fatal("upstream age ignored", d)
	}
	h.Set("Expires", time.Now().Add(-time.Minute).UTC().Format(http.TimeFormat))
	if d := freshness(h, time.Hour); d != 0 {
		t.Fatal("expired resource fresh", d)
	}
}

func TestTruncationAndDigestFailureNeverPublish(t *testing.T) {
	for _, kind := range []string{"length", "digest"} {
		t.Run(kind, func(t *testing.T) {
			s := service(t, 1024)
			resource := Resource{Key: "k", TTL: time.Hour}
			if kind == "digest" {
				resource.Digest = "sha256:" + strings.Repeat("0", 64)
			}
			fetch := func(context.Context, http.Header) (*http.Response, error) {
				v := resp(200, http.Header{}, "payload")
				if kind == "length" {
					v.ContentLength = 100
				}
				return v, nil
			}
			func() {
				defer func() {
					if e := recover(); e != http.ErrAbortHandler {
						t.Fatalf("expected aborted stream, got %v", e)
					}
				}()
				_ = call(s, "GET", http.Header{}, resource, fetch)
			}()
			if entry, e := s.Cache.Lookup(context.Background(), "k"); e != nil || entry != nil {
				t.Fatal("invalid object published", e)
			}
		})
	}
}
