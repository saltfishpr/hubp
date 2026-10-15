package config

import "testing"

func TestRouteKindsAndOrigins(t *testing.T) {
	valid := []Route{
		{Domain: "code.example.com", Kind: KindGitHub, Upstream: "https://github.com/"},
		{Domain: "files.example.com", Kind: KindHuggingFace, Upstream: "https://huggingface.co"},
		{Domain: "images.example.com", Kind: KindRegistry, Upstream: "https://ghcr.io/"},
	}
	for _, r := range valid {
		c := Default()
		c.Routes = []Route{r}
		if err := c.Validate(); err != nil {
			t.Fatal(r, err)
		}
		u, err := r.Origin()
		if err != nil || u.Path != "" || u.RawPath != "" {
			t.Fatal("origin not normalized", u, err)
		}
	}
	invalid := []Route{
		{Domain: "", Kind: KindGitHub, Upstream: "https://github.com"},
		{Domain: "Code.example.com", Kind: KindGitHub, Upstream: "https://github.com"},
		{Domain: "code.example.com:8080", Kind: KindGitHub, Upstream: "https://github.com"},
		{Domain: "code.example.com", Kind: "other", Upstream: "https://github.com"},
		{Domain: "code.example.com", Kind: KindGitHub},
		{Domain: "code.example.com", Kind: KindGitHub, Upstream: "https://huggingface.co"},
		{Domain: "code.example.com", Kind: KindGitHub, Upstream: "http://github.com"},
		{Domain: "code.example.com", Kind: KindGitHub, Upstream: "https://github.com/path"},
		{Domain: "code.example.com", Kind: KindGitHub, Upstream: "https://github.com?"},
		{Domain: "code.example.com", Kind: KindGitHub, Upstream: "https://user:secret@github.com"},
		{Domain: "files.example.com", Kind: KindHuggingFace, Upstream: "https://github.com"},
		{Domain: "images.example.com", Kind: KindRegistry, Upstream: "https://127.0.0.1"},
	}
	for _, r := range invalid {
		c := Default()
		c.Routes = []Route{r}
		if err := c.Validate(); err == nil {
			t.Fatal("invalid route accepted", r)
		}
	}
}

func TestRoutesOmittedKeepDefaults(t *testing.T) {
	c, err := Load("")
	if err != nil || len(c.Routes) != 9 {
		t.Fatal("missing default routes", err)
	}
}
