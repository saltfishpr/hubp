package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadReplacesRoutesAndRejectsInvalidConfig(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hubp.json")
	if err := os.WriteFile(p, []byte(`{"routes":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, e := Load(p)
	if e != nil {
		t.Fatal(e)
	}
	if len(c.Routes) != 0 {
		t.Fatal("default routes retained after explicit empty list")
	}
	for _, raw := range []string{`{"unknown_option":true}`, `{} {}`, `null`, `{"routes":[{"domain":"localhost","kind":"github","upstream":"https://github.com"},{"domain":"localhost","kind":"huggingface","upstream":"https://huggingface.co"}]}`, `{"routes":[{"domain":"private.local","kind":"registry","upstream":"https://127.0.0.1"}]}`, `{"github_hosts":["localhost"]}`, `{"hf_hosts":[]}`, `{"registries":{}}`, `{"upstream_proxy":"socks5://localhost:1234"}`} {
		if e = os.WriteFile(p, []byte(raw), 0o600); e != nil {
			t.Fatal(e)
		}
		if _, e = Load(p); e == nil {
			t.Fatal("invalid config accepted", raw)
		}
	}
}

func TestDefaultsAndProxy(t *testing.T) {
	if _, e := Load(""); e != nil {
		t.Fatal(e)
	}
	if u, e := ProxyURL("http://user:secret@127.0.0.1:3128"); e != nil || u.Host != "127.0.0.1:3128" {
		t.Fatal("trusted proxy config failed")
	}
}
