package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(code int, h http.Header, text string) *http.Response {
	return &http.Response{StatusCode: code, Header: h, Body: io.NopCloser(strings.NewReader(text)), ContentLength: int64(len(text))}
}

func TestRedirectDropsCredentialsAndPreservesSignedQuery(t *testing.T) {
	calls := 0
	client := NewWithTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return response(307, http.Header{"Location": {`https://cdn.example/object?sig=a%2Fb+z&x=1&x=2`}}, ""), nil
		}
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Fatal("credential crossed origin boundary")
		}
		if r.URL.RawQuery != `sig=a%2Fb+z&x=1&x=2` {
			t.Fatalf("query changed: %s", r.URL.RawQuery)
		}
		return response(200, http.Header{}, "ok"), nil
	}), 1, time.Second)
	q, _ := http.NewRequest("GET", "https://origin.example/start", nil)
	q.Header.Set("Authorization", "secret")
	q.Header.Set("Cookie", "secret")
	resp, e := client.Do(q, Options{Hosts: []string{"origin.example", "cdn.example"}})
	if e != nil {
		t.Fatal(e)
	}
	resp.Body.Close()
	if calls != 2 || len(client.slots) != 0 {
		t.Fatal("redirect leaked request slot")
	}
}
func TestRedirectRejectedBeforeDial(t *testing.T) {
	calls := 0
	c := NewWithTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return response(302, http.Header{"Location": {"https://127.0.0.1/secret"}}, ""), nil
	}), 1, time.Second)
	q, _ := http.NewRequest("GET", "https://origin.example/", nil)
	if _, err := c.Do(q, Options{Hosts: []string{"origin.example"}}); !errors.Is(err, ErrTarget) {
		t.Fatal(err)
	}
	if calls != 1 || len(c.slots) != 0 {
		t.Fatal("rejected redirect consumed resources")
	}
}
func TestBusyAndCancellationRelease(t *testing.T) {
	c := NewWithTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) { return response(200, http.Header{}, "ok"), nil }), 1, time.Second)
	q, _ := http.NewRequest("GET", "https://origin.example/", nil)
	a, e := c.Do(q, Options{Hosts: []string{"origin.example"}})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = c.Do(q, Options{Hosts: []string{"origin.example"}}); !errors.Is(e, ErrBusy) {
		t.Fatal(e)
	}
	a.Body.Close()
	a.Body.Close()
	b, e := c.Do(q, Options{Hosts: []string{"origin.example"}})
	if e != nil {
		t.Fatal(e)
	}
	b.Body.Close()
}

type blockedBody struct{ ctx context.Context }

func (b blockedBody) Read([]byte) (int, error) { <-b.ctx.Done(); return 0, b.ctx.Err() }
func (b blockedBody) Close() error             { return nil }
func TestStalledResponseCanceled(t *testing.T) {
	c := NewWithTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: blockedBody{r.Context()}}, nil
	}), 1, 20*time.Millisecond)
	q, _ := http.NewRequest("GET", "https://origin.example/", nil)
	r, e := c.Do(q, Options{Hosts: []string{"origin.example"}})
	if e != nil {
		t.Fatal(e)
	}
	_, e = r.Body.Read(make([]byte, 1))
	r.Body.Close()
	if !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
}
func TestPublicAddresses(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "100.100.100.200", "::1", "::ffff:192.168.1.1", "fc00::1", "192.0.2.3"} {
		if PublicIP(netip.MustParseAddr(s)) {
			t.Fatal("accepted", s)
		}
	}
	for _, s := range []string{"1.1.1.1", "2606:4700:4700::1111"} {
		if !PublicIP(netip.MustParseAddr(s)) {
			t.Fatal("rejected", s)
		}
	}
}
