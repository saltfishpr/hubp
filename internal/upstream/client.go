package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"
)

var (
	ErrBusy   = errors.New("upstream capacity exhausted")
	ErrTarget = errors.New("unsupported upstream target")
)

type Options struct {
	Hosts      []string
	OnResponse func(*http.Response) error
}

type Client struct {
	transport http.RoundTripper
	slots     chan struct{}
	stall     time.Duration
}

// NewWithTransport 使用指定的传输层、并发上限和读取停滞超时创建客户端，便于测试时替换传输层。
func NewWithTransport(rt http.RoundTripper, max int, stall time.Duration) *Client {
	return &Client{transport: rt, slots: make(chan struct{}, max), stall: stall}
}

// New 创建直连上游的客户端，并设置响应头超时、读取停滞超时和并发上限。
func New(headerTimeout, stall time.Duration, max int) *Client {
	return NewWithProxy(headerTimeout, stall, max, nil)
}

// NewWithProxy 创建上游客户端；proxy 为 nil 时直连，否则使用指定的可信出口代理。
// 使用代理时，目标 IP 的安全校验由代理负责，客户端仍校验目标主机名。
func NewWithProxy(headerTimeout, stall time.Duration, max int, proxy *url.URL) *Client {
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy: nil, // DNS validation must apply to the actual destination, not a remote proxy resolver.
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if err != nil {
				return nil, err
			}
			if len(ips) == 0 {
				return nil, ErrTarget
			}
			for _, ip := range ips {
				if !PublicIP(ip) {
					return nil, ErrTarget
				}
			}
			var last error
			for _, ip := range ips {
				conn, e := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				if e == nil {
					return conn, nil
				}
				last = e
			}
			return nil, last
		},
		ForceAttemptHTTP2:     true,
		DisableCompression:    true,
		MaxIdleConns:          128,
		MaxIdleConnsPerHost:   16,
		MaxConnsPerHost:       32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: headerTimeout,
		ExpectContinueTimeout: time.Second,
	}
	if proxy != nil {
		transport.Proxy = http.ProxyURL(proxy)
		transport.DialContext = dialer.DialContext
	}
	return NewWithTransport(transport, max, stall)
}

// PublicIP 判断 IP 是否符合上游直连策略，排除私有、回环及指定的特殊用途地址段。
func PublicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return false
	}
	for _, s := range []string{"100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001:db8::/32"} {
		if netip.MustParsePrefix(s).Contains(ip) {
			return false
		}
	}
	return true
}

// Allowed 检查 URL 是否为允许访问的 HTTPS 目标；hosts 中以点开头的条目匹配子域名。
func Allowed(u *url.URL, hosts []string) bool {
	if u == nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") {
		return false
	}
	h := strings.ToLower(u.Hostname())
	for _, p := range hosts {
		if h == p || (strings.HasPrefix(p, ".") && strings.HasSuffix(h, p) && len(h) > len(p)) {
			return true
		}
	}
	return false
}

// CleanHeaders 返回请求头或响应头的副本，移除逐跳头和代理转发来源信息。
func CleanHeaders(src http.Header) http.Header {
	h := src.Clone()
	for _, v := range h.Values("Connection") {
		for _, name := range strings.Split(v, ",") {
			h.Del(strings.TrimSpace(name))
		}
	}
	for _, k := range []string{
		"Connection",
		"Proxy-Connection",
		"Keep-Alive",
		"Proxy-Authenticate",
		"Proxy-Authorization",
		"Te",
		"Trailer",
		"Transfer-Encoding",
		"Upgrade",
		"Forwarded",
		"X-Forwarded-For",
		"X-Forwarded-Host",
		"X-Forwarded-Proto",
	} {
		h.Del(k)
	}
	return h
}

// Request 将请求克隆为发往 target 的上游请求，移除认证和 Cookie，并禁用压缩协商。
func Request(r *http.Request, target *url.URL) *http.Request {
	q := r.Clone(r.Context())
	u := *target
	q.URL = &u
	q.Host = u.Host
	q.RequestURI = ""
	q.Header = CleanHeaders(r.Header)
	q.Header.Del("Authorization")
	q.Header.Del("Cookie")
	q.Header.Set("Accept-Encoding", "identity")
	q.Trailer = nil
	return q
}

// Do 在并发限制和目标白名单约束下发送请求，并处理允许的重定向及响应回调。
// 调用方必须关闭返回响应的 Body，以释放并发名额。
func (c *Client) Do(req *http.Request, opts Options) (*http.Response, error) {
	q := req.Clone(req.Context())
	q.Header = req.Header.Clone()
	for n := 0; n <= 10; n++ {
		if !Allowed(q.URL, opts.Hosts) {
			return nil, ErrTarget
		}
		resp, err := c.roundTrip(q)
		if err != nil {
			return nil, err
		}
		if opts.OnResponse != nil {
			if err = opts.OnResponse(resp); err != nil {
				resp.Body.Close()
				return nil, err
			}
		}
		switch resp.StatusCode {
		case 301, 302, 303, 307, 308:
			loc, err := resp.Location()
			resp.Body.Close()
			if err != nil {
				return nil, errors.New("invalid upstream redirect")
			}
			if q.Method != http.MethodGet && q.Method != http.MethodHead {
				return nil, errors.New("cannot replay streaming request through a redirect")
			}
			if !Allowed(loc, opts.Hosts) {
				return nil, ErrTarget
			}
			next := q.Clone(q.Context())
			next.Header = q.Header.Clone()
			next.URL = loc
			next.Host = loc.Host
			next.RequestURI = ""
			if !strings.EqualFold(q.URL.Host, loc.Host) {
				next.Header.Del("Authorization")
				next.Header.Del("Cookie")
			}
			q = next
		default:
			return resp, nil
		}
	}
	return nil, errors.New("too many upstream redirects")
}

func (c *Client) roundTrip(q *http.Request) (*http.Response, error) {
	select {
	case c.slots <- struct{}{}:
	default:
		return nil, ErrBusy
	}
	ctx, cancel := context.WithCancel(q.Context())
	req := q.Clone(ctx)
	resp, err := c.transport.RoundTrip(req)
	if err != nil {
		cancel()
		<-c.slots
		return nil, fmt.Errorf("upstream request failed: %w", err)
	}
	resp.Request = req
	b := &body{ReadCloser: resp.Body, release: func() { cancel(); <-c.slots }, timeout: c.stall, cancel: cancel}
	if c.stall > 0 {
		b.timer = time.AfterFunc(c.stall, cancel)
	}
	resp.Body = b
	return resp, nil
}

type body struct {
	io.ReadCloser
	once    sync.Once
	mu      sync.Mutex
	timer   *time.Timer
	timeout time.Duration
	cancel  context.CancelFunc
	release func()
	closed  bool
}

// Read 读取响应内容，并在读到数据后重置读取停滞计时器。
func (b *body) Read(p []byte) (int, error) {
	n, e := b.ReadCloser.Read(p)
	if n > 0 {
		b.mu.Lock()
		if !b.closed && b.timer != nil {
			b.timer.Reset(b.timeout)
		}
		b.mu.Unlock()
	}
	return n, e
}

// Close 关闭响应体、停止计时器并释放并发名额；重复调用不会重复释放资源。
func (b *body) Close() error {
	var err error
	b.once.Do(func() {
		b.mu.Lock()
		b.closed = true
		if b.timer != nil {
			b.timer.Stop()
		}
		b.mu.Unlock()
		err = b.ReadCloser.Close()
		b.release()
	})
	return err
}

// Close 关闭底层传输层的空闲连接，不中断正在进行的请求。
func (c *Client) Close() {
	if t, ok := c.transport.(interface{ CloseIdleConnections() }); ok {
		t.CloseIdleConnections()
	}
}
