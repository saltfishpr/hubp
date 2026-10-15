package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"hubp/internal/config"
	"hubp/internal/transfer"
	"hubp/internal/upstream"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRouterUsesKindAndUpstreamInsteadOfDomainNames(t *testing.T) {
	cfg := config.Default()
	cfg.Routes = []config.Route{
		{Domain: "alpha.local", Kind: config.KindGitHub, Upstream: "https://github.com"},
		{Domain: "alias.local", Kind: config.KindGitHub, Upstream: "https://github.com/"},
		{Domain: "beta.local", Kind: config.KindRegistry, Upstream: "https://ghcr.io"},
		{Domain: "gamma.local", Kind: config.KindHuggingFace, Upstream: "https://huggingface.co"},
	}
	var target string
	client := upstream.NewWithTransport(roundTripFunc(func(q *http.Request) (*http.Response, error) {
		target = q.URL.Host
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("payload")), ContentLength: 7}, nil
	}), 4, time.Second)
	handler, err := NewRouter(cfg, client, &transfer.Service{})
	if err != nil {
		t.Fatal(err)
	}
	router := handler.(*Router)
	if router.hosts["alpha.local"] != router.hosts["alias.local"] {
		t.Fatal("identical normalized upstreams did not reuse adapter")
	}
	tests := []struct{ host, path, want string }{
		{"alpha.local", "/https://github.com/u/r/archive/main.zip", "github.com"},
		{"BETA.local:8443", "/v2/org/repo/manifests/latest", "ghcr.io"},
		{"gamma.local", "/api/models/org/repo", "huggingface.co"},
	}
	for _, tc := range tests {
		q := httptest.NewRequest("GET", tc.path, nil)
		q.Host = tc.host
		w := httptest.NewRecorder()
		target = ""
		handler.ServeHTTP(w, q)
		if w.Code != 200 || w.Body.String() != "payload" || target != tc.want {
			t.Fatal(tc.host, w.Code, w.Body.String(), target)
		}
	}
	cfg.Routes = []config.Route{}
	handler, err = NewRouter(cfg, client, &transfer.Service{})
	if err != nil {
		t.Fatal(err)
	}
	q := httptest.NewRequest("GET", "/", nil)
	q.Host = "alpha.local"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, q)
	if w.Code != 421 {
		t.Fatal("empty routes did not disable dispatch", w.Code)
	}
}
