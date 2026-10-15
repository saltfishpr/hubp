package server

import (
	"net/http"
	"time"

	"hubp/internal/adapters/github"
	"hubp/internal/adapters/huggingface"
	"hubp/internal/adapters/registry"
	"hubp/internal/config"
	"hubp/internal/transfer"
	"hubp/internal/upstream"
)

type Router struct{ hosts map[string]http.Handler }

// NewRouter 校验配置并创建按域名分发的路由器，相同协议类型和源站共用一个适配器。
func NewRouter(cfg config.Config, client *upstream.Client, delivery *transfer.Service) (http.Handler, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	routes := map[string]http.Handler{}
	ttl := time.Duration(cfg.Cache.TTLSeconds) * time.Second
	type adapterKey struct {
		kind   config.Kind
		origin string
	}
	adapters := map[adapterKey]http.Handler{}
	for _, route := range cfg.Routes {
		origin, err := route.Origin()
		if err != nil {
			return nil, err
		}
		key := adapterKey{route.Kind, origin.String()}
		handler := adapters[key]
		if handler == nil {
			switch route.Kind {
			case config.KindGitHub:
				handler = &github.Handler{Client: client, Transfer: delivery, TTL: ttl, Origin: origin}
			case config.KindHuggingFace:
				handler = &huggingface.Handler{Client: client, Transfer: delivery, TTL: ttl, PublicScheme: cfg.PublicScheme, Origin: origin}
			case config.KindRegistry:
				handler = registry.New(origin, client, delivery, ttl)
			}
			adapters[key] = handler
		}
		routes[route.Domain] = handler
	}
	return &Router{hosts: routes}, nil
}

// ServeHTTP 按请求的 Host 分发到已配置的适配器，拒绝正向代理请求和未知域名。
func (r *Router) ServeHTTP(w http.ResponseWriter, q *http.Request) {
	if q.URL.IsAbs() || q.Method == http.MethodConnect {
		http.Error(w, "absolute-form proxy requests are unsupported", http.StatusBadRequest)
		return
	}
	h := r.hosts[config.Host(q.Host)]
	if h == nil {
		http.Error(w, "unknown proxy host", http.StatusMisdirectedRequest)
		return
	}
	h.ServeHTTP(w, q)
}
