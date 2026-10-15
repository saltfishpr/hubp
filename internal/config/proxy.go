package config

import (
	"errors"
	"net/url"
)

// ProxyURL 解析并校验管理员指定的可信 HTTP(S) 出口代理地址；空字符串返回 nil。
func ProxyURL(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("upstream_proxy must be an HTTP(S) proxy origin")
	}
	return u, nil
}
