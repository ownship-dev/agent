package hostproxy

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestProxyKeepsHostAndChoosesForwardedHeaders(t *testing.T) {
	var got *http.Request
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r }))
	defer app.Close()
	u, _ := url.Parse(app.URL)

	for _, tc := range []struct {
		keep  bool
		proto string
	}{{false, "http"}, {true, "https"}} {
		req := httptest.NewRequest(http.MethodGet, "http://app.lan/x", nil)
		req.Header.Set("X-Forwarded-Proto", "https")
		Fixed(u, Options{KeepForwarded: tc.keep}).ServeHTTP(httptest.NewRecorder(), req)
		if got.Host != "app.lan" {
			t.Fatalf("keep=%v: host %q, want the client's", tc.keep, got.Host)
		}
		if p := got.Header.Get("X-Forwarded-Proto"); p != tc.proto {
			t.Fatalf("keep=%v: X-Forwarded-Proto %q, want %q", tc.keep, p, tc.proto)
		}
	}
}

func TestUnreachableAppIsBadGateway(t *testing.T) {
	u, _ := url.Parse("http://127.0.0.1:1")
	rec := httptest.NewRecorder()
	Fixed(u, Options{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://app.lan/", nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", rec.Code)
	}
}

func TestNormalizeHost(t *testing.T) {
	for in, want := range map[string]string{"App.Lan:8443": "app.lan", "app.lan.": "app.lan", "[::1]:80": "::1", " x.io ": "x.io"} {
		if got := NormalizeHost(in); got != want {
			t.Fatalf("NormalizeHost(%q) = %q, want %q", in, got, want)
		}
	}
}
