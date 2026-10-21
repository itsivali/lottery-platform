package httpx_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/itsivali/lottery-platform/internal/platform/httpx"
)

type body struct {
	Name string `json:"name"`
	N    int    `json:"n"`
}

func decode(t *testing.T, raw string) (body, int) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/x", strings.NewReader(raw))
	var b body
	if !httpx.DecodeJSON(rec, req, &b) {
		return b, rec.Code
	}
	return b, 0
}

func TestDecodeJSON_AcceptsAValidBody(t *testing.T) {
	b, code := decode(t, `{"name":"a","n":3}`)
	if code != 0 || b.Name != "a" || b.N != 3 {
		t.Fatalf("%+v %d", b, code)
	}
}

func TestDecodeJSON_RejectsUnknownFieldsTrailingDataAndBadTypes(t *testing.T) {
	for name, raw := range map[string]string{
		"unknown field (e.g. user_id injection)": `{"name":"a","user_id":"someone-else"}`,
		"trailing json":                          `{"name":"a"} {"name":"b"}`,
		"trailing garbage":                       `{"name":"a"}x`,
		"wrong type":                             `{"name":1}`,
		"not json":                               `hello`,
		"empty":                                  ``,
		"array":                                  `[1]`,
		"null":                                   `null`,
	} {
		if _, code := decode(t, raw); code != http.StatusBadRequest {
			t.Errorf("%s: code = %d, want 400", name, code)
		}
	}
}

func TestDecodeJSON_RejectsOversizedBodies(t *testing.T) {
	if _, code := decode(t, `{"name":"`+strings.Repeat("a", 2<<20)+`"}`); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code = %d, want 413", code)
	}
}

func TestClientIP(t *testing.T) {
	mk := func(remote string, h map[string]string) *http.Request {
		r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
		r.RemoteAddr = remote
		for k, v := range h {
			r.Header.Set(k, v)
		}
		return r
	}
	cases := []struct {
		name   string
		r      *http.Request
		header string
		want   string
	}{
		{"no trusted header: remote address, spoofed XFF ignored", mk("198.51.100.7:4000", map[string]string{"X-Forwarded-For": "1.2.3.4"}), "", "198.51.100.7"},
		{"single-value header", mk("10.0.0.1:1", map[string]string{"True-Client-IP": "203.0.113.5"}), "True-Client-IP", "203.0.113.5"},
		{"XFF uses the rightmost entry appended by the trusted proxy", mk("10.0.0.1:1", map[string]string{"X-Forwarded-For": "6.6.6.6, 203.0.113.9"}), "X-Forwarded-For", "203.0.113.9"},
		{"header missing falls back to remote", mk("198.51.100.7:4000", nil), "True-Client-IP", "198.51.100.7"},
		{"garbage header falls back to remote", mk("198.51.100.7:4000", map[string]string{"True-Client-IP": "not-an-ip"}), "True-Client-IP", "198.51.100.7"},
		{"ipv6", mk("[2001:db8::1]:9", nil), "", "2001:db8::1"},
	}
	for _, c := range cases {
		if got := httpx.ClientIP(c.r, c.header); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestClientIPHops_PicksTheNthEntryFromTheRight(t *testing.T) {
	mk := func(xff string) *http.Request {
		r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
		r.RemoteAddr = "10.0.0.1:1"
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return r
	}
	cases := []struct {
		name string
		xff  string
		hops int
		want string
	}{
		{"one trusted proxy: the entry it appended", "6.6.6.6, 203.0.113.9", 1, "203.0.113.9"},
		{"two trusted proxies (CDN + load balancer)", "6.6.6.6, 203.0.113.9, 10.1.1.1", 2, "203.0.113.9"},
		{"client-supplied spoof entries are never reached", "1.1.1.1, 2.2.2.2, 203.0.113.9, 10.1.1.1", 2, "203.0.113.9"},
		{"fewer entries than hops: do not guess, use the socket", "203.0.113.9", 2, "10.0.0.1"},
		{"hops below 1 behaves as 1", "6.6.6.6, 203.0.113.9", 0, "203.0.113.9"},
		{"unparseable entry falls back to the socket", "6.6.6.6, garbage", 1, "10.0.0.1"},
		{"header absent", "", 1, "10.0.0.1"},
	}
	for _, c := range cases {
		if got := httpx.ClientIPHops(mk(c.xff), "X-Forwarded-For", c.hops); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
