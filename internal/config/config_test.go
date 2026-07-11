package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoad(t *testing.T) {
	t.Setenv("PROXY_URL", "http://127.0.0.1:7890")
	t.Setenv("GHCR_USERNAME", `user"name`)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	data := `{
  "server": {
    "listen": ":9090",
    "public_url": "https://mirror.example.com"
  },
  "cache": {
    "enabled": true,
    "dir": "` + filepath.ToSlash(filepath.Join(dir, "cache")) + `",
    "manifest_ttl": "7m"
  },
  "registries": [
    {
      "name": "dockerhub",
      "path_prefix": "",
      "upstream": "https://registry-1.docker.io",
      "https_proxy": "${PROXY_URL}",
      "allowed_auth_hosts": ["auth.docker.io"]
    },
    {
      "name": "ghcr",
      "path_prefix": "ghcr.io",
      "upstream": "https://ghcr.io",
      "allowed_auth_hosts": ["ghcr.io"],
      "username": "${GHCR_USERNAME}",
      "password": "token"
    }
  ]
}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Server.Listen != ":9090" {
		t.Fatalf("listen = %q", cfg.Server.Listen)
	}
	if got := cfg.Server.ReadHeaderTimeout.Value(); got != 10*time.Second {
		t.Fatalf("default read header timeout = %s", got)
	}
	if got := cfg.Cache.ManifestTTL.Value(); got != 7*time.Minute {
		t.Fatalf("manifest TTL = %s", got)
	}
	if got := cfg.Registries[0].HTTPSProxy; got != "http://127.0.0.1:7890" {
		t.Fatalf("proxy = %q", got)
	}
	if got := cfg.Registries[1].AllowedAuthHosts; len(got) != 1 || got[0] != "ghcr.io" {
		t.Fatalf("allowed auth hosts = %#v", got)
	}
	if got := cfg.Registries[1].Username; got != `user"name` {
		t.Fatalf("username = %q", got)
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := `{
  "server": {"listen": ":8080", "typo": true},
  "registries": [
    {
      "name": "dockerhub",
      "path_prefix": "",
      "upstream": "https://registry-1.docker.io"
    }
  ]
}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() unexpectedly accepted an unknown field")
	}
	if !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadRejectsInvalidDuration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := `{
  "cache": {"manifest_ttl": "soon"},
  "registries": [
    {
      "name": "dockerhub",
      "path_prefix": "",
      "upstream": "https://registry-1.docker.io"
    }
  ]
}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load() unexpectedly accepted an invalid duration")
	}
}

func TestLoadRejectsTrailingJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := `{"registries":[{"name":"dockerhub","path_prefix":"","upstream":"https://registry-1.docker.io"}]} {}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load() unexpectedly accepted trailing JSON")
	}
}
