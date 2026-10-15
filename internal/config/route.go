package config

import (
	"fmt"
	"net/url"
)

type Kind string

const (
	KindGitHub      Kind = "github"
	KindRegistry    Kind = "registry"
	KindHuggingFace Kind = "huggingface"
)

// Route maps one public domain to one protocol adapter and upstream origin.
type Route struct {
	Domain   string `json:"domain"`
	Kind     Kind   `json:"kind"`
	Upstream string `json:"upstream"`
}

// Origin 按路由类型校验并规范化受支持的 HTTPS 源站地址，不接受任意转发目标 URL。
func (r Route) Origin() (*url.URL, error) {
	var allowed map[string]bool
	switch r.Kind {
	case KindGitHub:
		allowed = map[string]bool{"github.com": true}
	case KindHuggingFace:
		allowed = map[string]bool{"huggingface.co": true}
	case KindRegistry:
		allowed = map[string]bool{"registry-1.docker.io": true, "ghcr.io": true, "gcr.io": true, "quay.io": true, "registry.k8s.io": true}
	default:
		return nil, fmt.Errorf("unsupported route kind %q", r.Kind)
	}
	u, err := url.Parse(r.Upstream)
	if err != nil || u.Scheme != "https" || u.User != nil || !allowed[u.Host] || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("%s upstream must be a supported HTTPS origin", r.Kind)
	}
	u.Path = ""
	u.RawPath = ""
	return u, nil
}
