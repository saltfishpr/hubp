package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"
	"unicode"
)

// Config 定义服务监听、路由、上游访问和缓存配置，可通过 Default 或 Load 获取。
type Config struct {
	Listen               string  `json:"listen"`                 // 服务监听地址，默认 127.0.0.1:8080。
	PublicScheme         string  `json:"public_scheme"`          // 生成对外链接使用的协议，默认 http；外层终止 TLS 时设为 https。
	UpstreamProxy        string  `json:"upstream_proxy"`         // 可信 HTTP(S) 出口代理地址，空值表示直连。
	Routes               []Route `json:"routes"`                 // 按请求 Host 匹配的路由，每个域名只能配置一次。
	HeaderTimeoutSeconds int     `json:"header_timeout_seconds"` // 请求发送完成后等待上游响应头的超时，单位秒；默认 30，不限制响应体下载时长。
	StallTimeoutSeconds  int     `json:"stall_timeout_seconds"`  // 上游读取或客户端写入的停滞超时，单位秒；默认 120，读到数据后重置上游计时器。
	MaxUpstreamRequests  int     `json:"max_upstream_requests"`  // 全局回源并发上限，默认 128；响应体关闭才释放名额，满额立即返回 503。
	Cache                Cache   `json:"cache"`
}

// Cache 定义磁盘缓存的开关、目录、容量和有效期，通过 Config.Cache 配置。
type Cache struct {
	Enabled        bool   `json:"enabled"`          // 是否启用磁盘缓存，默认关闭。
	Dir            string `json:"dir"`              // 缓存目录，须由单个实例独占；默认 ./data。
	MaxBytes       int64  `json:"max_bytes"`        // 缓存对象与临时写入的总容量上限，单位字节；默认 10 GiB。
	MaxObjectBytes int64  `json:"max_object_bytes"` // 单个对象的缓存上限，单位字节；默认 5 GiB，超出后停止缓存但继续下载。
	MinFreeBytes   uint64 `json:"min_free_bytes"`   // 缓存写入时需保留的磁盘可用空间，单位字节；默认 64 MiB。
	TTLSeconds     int    `json:"ttl_seconds"`      // 可变资源的缓存有效期上限，单位秒；默认 300，上游更严格的约束优先。
}

// Default 返回包含默认路由、超时和缓存参数的配置，缓存默认不启用。
func Default() Config {
	return Config{
		Listen:       "127.0.0.1:8080",
		PublicScheme: "http",

		HeaderTimeoutSeconds: 30,
		StallTimeoutSeconds:  120,
		MaxUpstreamRequests:  128,

		Cache: Cache{
			Dir:            "./data",
			MaxBytes:       10 << 30,
			MaxObjectBytes: 5 << 30,
			MinFreeBytes:   64 << 20,
			TTLSeconds:     300,
		},
	}
}

// Load 在默认配置上加载 JSON 文件并校验，拒绝未知字段和超过 1 MiB 的文件。
// path 为空时仅返回经过校验的默认配置。
func Load(path string) (Config, error) {
	c := Default()
	if path != "" {
		f, err := os.Open(path)
		if err != nil {
			return c, err
		}
		defer f.Close()
		raw, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
		if err != nil {
			return c, err
		}
		if len(raw) > 1<<20 {
			return c, errors.New("config exceeds 1 MiB")
		}
		var fields map[string]json.RawMessage
		if err = json.Unmarshal(raw, &fields); err != nil || fields == nil {
			return c, errors.New("config must contain one JSON object")
		}
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if err = d.Decode(&c); err != nil {
			return c, fmt.Errorf("decode config: %w", err)
		}
		var extra any
		if err = d.Decode(&extra); err != io.EOF {
			return c, errors.New("config must contain one JSON object")
		}
	}
	return c, c.Validate()
}

// Host 移除主机地址中的端口和末尾的点，并将主机名转为小写。
func Host(s string) string {
	if h, _, err := net.SplitHostPort(s); err == nil {
		s = h
	}
	return strings.ToLower(strings.TrimSuffix(s, "."))
}

// Validate 校验监听地址、代理、超时、路由和缓存配置，并拒绝重复的路由域名。
func (c Config) Validate() error {
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	if c.PublicScheme != "http" && c.PublicScheme != "https" {
		return errors.New("public_scheme must be http or https")
	}
	if _, err := ProxyURL(c.UpstreamProxy); err != nil {
		return err
	}
	if c.HeaderTimeoutSeconds < 1 || c.StallTimeoutSeconds < 1 || c.MaxUpstreamRequests < 1 {
		return errors.New("timeouts and upstream request capacity must be positive")
	}
	seen := map[string]bool{}
	check := func(host string) error {
		if host == "" || strings.ContainsAny(host, "/:@?# ") || strings.IndexFunc(host, unicode.IsSpace) >= 0 || Host(host) != host {
			return fmt.Errorf("invalid canonical host %q", host)
		}
		if seen[host] {
			return fmt.Errorf("host %q is assigned more than once", host)
		}
		seen[host] = true
		return nil
	}
	for i, route := range c.Routes {
		if err := check(route.Domain); err != nil {
			return fmt.Errorf("routes[%d]: %w", i, err)
		}
		if _, err := route.Origin(); err != nil {
			return fmt.Errorf("routes[%d] (%s): %w", i, route.Domain, err)
		}
	}
	if c.Cache.Enabled && (c.Cache.Dir == "" || c.Cache.MaxBytes < 1 || c.Cache.MaxObjectBytes < 1 || c.Cache.MaxObjectBytes > c.Cache.MaxBytes || c.Cache.TTLSeconds < 1) {
		return errors.New("invalid cache directory, capacity or TTL")
	}
	return nil
}

// HeaderTimeout 将响应头超时配置从秒转换为 time.Duration。
func (c Config) HeaderTimeout() time.Duration {
	return time.Duration(c.HeaderTimeoutSeconds) * time.Second
}

// StallTimeout 将读取停滞超时配置从秒转换为 time.Duration。
func (c Config) StallTimeout() time.Duration {
	return time.Duration(c.StallTimeoutSeconds) * time.Second
}
