package proxy

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/saltfishpr/hubp/internal/config"
)

func TestBearerRewritePathMappingAndCache(t *testing.T) {
	var registryRequests atomic.Int32
	var tokenRequests atomic.Int32

	tokenServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenRequests.Add(1)
		if got := r.URL.Query().Get("scope"); got != "repository:oopsunix/hubp:pull" {
			t.Errorf("scope = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"token":"test-token"}`)
	}))
	defer tokenServer.Close()
	tokenURL, _ := url.Parse(tokenServer.URL)

	registryServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		registryRequests.Add(1)
		if r.URL.Path != "/v2/oopsunix/hubp/manifests/latest" {
			t.Errorf("upstream path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="ghcr.io",scope="repository:oopsunix/hubp:pull"`, tokenServer.URL))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", "sha256:abc")
		_, _ = io.WriteString(w, `{"schemaVersion":2}`)
	}))
	defer registryServer.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{Listen: ":0"},
		Cache: config.CacheConfig{
			Enabled:     true,
			Dir:         t.TempDir(),
			ManifestTTL: config.Duration(time.Minute),
		},
		Registries: []config.RegistryConfig{
			{
				Name:                  "dockerhub",
				PathPrefix:            "",
				Upstream:              registryServer.URL,
				AllowedAuthHosts:      []string{tokenURL.Host},
				TLSInsecureSkipVerify: true,
			},
			{
				Name:                  "ghcr",
				PathPrefix:            "ghcr.io",
				Upstream:              registryServer.URL,
				AllowedAuthHosts:      []string{tokenURL.Host},
				TLSInsecureSkipVerify: true,
			},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()

	imageURL := proxyServer.URL + "/v2/ghcr.io/oopsunix/hubp/manifests/latest"
	first, err := http.Get(imageURL)
	if err != nil {
		t.Fatal(err)
	}
	_ = first.Body.Close()
	if first.StatusCode != http.StatusUnauthorized {
		t.Fatalf("first status = %d", first.StatusCode)
	}
	challenge := first.Header.Get("WWW-Authenticate")
	if !strings.Contains(challenge, proxyServer.URL+"/token/ghcr/") {
		t.Fatalf("challenge was not rewritten: %q", challenge)
	}

	realm := bearerRealm(t, challenge)
	tokenResponse, err := http.Get(realm + "?service=ghcr.io&scope=repository:oopsunix/hubp:pull")
	if err != nil {
		t.Fatal(err)
	}
	tokenBody, _ := io.ReadAll(tokenResponse.Body)
	_ = tokenResponse.Body.Close()
	if tokenResponse.StatusCode != 200 || !strings.Contains(string(tokenBody), "test-token") {
		t.Fatalf("token status=%d body=%q", tokenResponse.StatusCode, tokenBody)
	}

	request, _ := http.NewRequest(http.MethodGet, imageURL, nil)
	request.Header.Set("Authorization", "Bearer test-token")
	request.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json")
	second, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(second.Body)
	_ = second.Body.Close()
	if second.StatusCode != 200 || string(body) != `{"schemaVersion":2}` {
		t.Fatalf("second status=%d body=%q", second.StatusCode, body)
	}
	if second.Header.Get("X-Hubp-Cache") != "MISS" {
		t.Fatalf("second cache header = %q", second.Header.Get("X-Hubp-Cache"))
	}

	thirdRequest, _ := http.NewRequest(http.MethodGet, imageURL, nil)
	thirdRequest.Header.Set("Authorization", "Bearer test-token")
	thirdRequest.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json")
	third, err := http.DefaultClient.Do(thirdRequest)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, third.Body)
	_ = third.Body.Close()
	if third.Header.Get("X-Hubp-Cache") != "HIT" {
		t.Fatalf("third cache header = %q", third.Header.Get("X-Hubp-Cache"))
	}
	if got := registryRequests.Load(); got != 2 {
		t.Fatalf("registry requests = %d, want 2 (401 + authenticated fetch)", got)
	}
	if got := tokenRequests.Load(); got != 1 {
		t.Fatalf("token requests = %d", got)
	}
}

func TestDockerHubUsesConfiguredHTTPSProxy(t *testing.T) {
	var connectRequests atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/library/alpine/manifests/latest" {
			t.Errorf("upstream path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		_, _ = io.WriteString(w, `{"schemaVersion":2}`)
	}))
	defer upstream.Close()

	forwardProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
			return
		}
		connectRequests.Add(1)
		upstreamConn, err := net.DialTimeout("tcp", r.Host, 2*time.Second)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			_ = upstreamConn.Close()
			http.Error(w, "hijacking unsupported", http.StatusInternalServerError)
			return
		}
		clientConn, buffered, err := hijacker.Hijack()
		if err != nil {
			_ = upstreamConn.Close()
			return
		}
		_, _ = buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = buffered.Flush()
		go func() {
			defer clientConn.Close()
			defer upstreamConn.Close()
			_, _ = io.Copy(upstreamConn, clientConn)
		}()
		_, _ = io.Copy(clientConn, upstreamConn)
	}))
	defer forwardProxy.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{Listen: ":0"},
		Cache:  config.CacheConfig{Enabled: false},
		Registries: []config.RegistryConfig{{
			Name:                  "dockerhub",
			PathPrefix:            "",
			Upstream:              upstream.URL,
			HTTPSProxy:            forwardProxy.URL,
			TLSInsecureSkipVerify: true,
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	response, err := http.Get(server.URL + "/v2/library/alpine/manifests/latest")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || string(body) != `{"schemaVersion":2}` {
		t.Fatalf("status=%d body=%q", response.StatusCode, body)
	}
	if connectRequests.Load() == 0 {
		t.Fatal("configured HTTPS proxy did not receive a CONNECT request")
	}
}

func bearerRealm(t *testing.T, challenge string) string {
	t.Helper()
	match := realmPattern.FindStringSubmatch(challenge)
	if len(match) != 2 {
		t.Fatalf("no realm in challenge %q", challenge)
	}
	return match[1]
}
