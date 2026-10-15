package registry

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"hubp/internal/upstream"
)

type tokenEntry struct {
	value   string
	expires time.Time
}
type tokenStore struct {
	mu      sync.Mutex
	entries map[string]tokenEntry
}

var parameter = regexp.MustCompile(`([A-Za-z_]+)=(?:"((?:[^"\\]|\\.)*)"|([^,\s]+))`)

func challenge(headers http.Header) (map[string]string, error) {
	for _, v := range headers.Values("WWW-Authenticate") {
		if len(v) < 7 || !strings.EqualFold(v[:7], "Bearer ") {
			continue
		}
		p := map[string]string{}
		for _, m := range parameter.FindAllStringSubmatch(v[7:], -1) {
			value := m[2]
			if value == "" {
				value = m[3]
			}
			p[strings.ToLower(m[1])] = strings.ReplaceAll(value, `\"`, `"`)
		}
		if p["realm"] != "" {
			return p, nil
		}
	}
	return nil, errors.New("upstream did not supply a Bearer challenge")
}

func (h *Handler) token(ctx context.Context, p map[string]string, repo string) (string, string, error) {
	realm, err := url.Parse(p["realm"])
	if err != nil || !upstream.Allowed(realm, h.hosts) {
		return "", "", upstream.ErrTarget
	}
	scope := "repository:" + repo + ":pull"
	key := realm.String() + "|" + p["service"] + "|" + scope
	h.tokens.mu.Lock()
	t, ok := h.tokens.entries[key]
	h.tokens.mu.Unlock()
	if ok && time.Now().Before(t.expires) {
		return t.value, key, nil
	}
	query := realm.Query()
	query.Set("service", p["service"])
	query.Set("scope", scope)
	realm.RawQuery = query.Encode()
	q, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return "", "", err
	}
	q.Header.Set("Accept-Encoding", "identity")
	resp, err := h.Client.Do(q, upstream.Options{Hosts: h.hosts})
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", errors.New("anonymous upstream token denied")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return "", "", errors.New("invalid token response")
	}
	var reply struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		Expires     int    `json:"expires_in"`
	}
	if err = json.Unmarshal(raw, &reply); err != nil {
		return "", "", errors.New("invalid token JSON")
	}
	if reply.Token == "" {
		reply.Token = reply.AccessToken
	}
	if reply.Token == "" {
		return "", "", errors.New("missing anonymous token")
	}
	if reply.Expires <= 0 {
		reply.Expires = 60
	}
	ttl := time.Duration(min(reply.Expires, 3600)) * time.Second
	if ttl > 10*time.Second {
		ttl -= 10 * time.Second
	}
	h.tokens.mu.Lock()
	if len(h.tokens.entries) >= 4096 {
		clear(h.tokens.entries)
	}
	h.tokens.entries[key] = tokenEntry{reply.Token, time.Now().Add(ttl)}
	h.tokens.mu.Unlock()
	return reply.Token, key, nil
}

func (h *Handler) fetch(r *http.Request, target *url.URL, repo string, headers http.Header) (*http.Response, error) {
	q := upstream.Request(r, target)
	for k, v := range headers {
		q.Header[k] = v
	}
	resp, err := h.Client.Do(q, upstream.Options{Hosts: h.hosts})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}
	p, e := challenge(resp.Header)
	resp.Body.Close()
	if e != nil {
		return nil, e
	}
	for attempt := 0; attempt < 2; attempt++ {
		token, key, e := h.token(r.Context(), p, repo)
		if e != nil {
			return nil, e
		}
		q = upstream.Request(r, target)
		for k, v := range headers {
			q.Header[k] = v
		}
		q.Header.Set("Authorization", "Bearer "+token)
		resp, err = h.Client.Do(q, upstream.Options{Hosts: h.hosts})
		if err != nil || resp.StatusCode != http.StatusUnauthorized {
			return resp, err
		}
		h.tokens.mu.Lock()
		delete(h.tokens.entries, key)
		h.tokens.mu.Unlock()
		if attempt == 1 {
			return resp, nil
		}
		if next, e := challenge(resp.Header); e == nil {
			p = next
		}
		resp.Body.Close()
	}
	return nil, errors.New("anonymous token refresh failed")
}
