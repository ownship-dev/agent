// Package hostproxy is the reverse proxy shared by the agent's ingresses: the waker, the LAN
// proxy and the tailnet nodes all route by Host to an app on this machine.
package hostproxy

import (
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// Options tune a proxy for where it sits in the chain.
type Options struct {
	// Keep the X-Forwarded-* headers of an ingress in front, such as the LAN proxy ahead of the
	// waker, instead of replacing them with this hop's view.
	KeepForwarded bool
}

// To proxies to whatever target returns at request time, keeping the client's Host, which
// the waker and most apps route on.
func To(target func() *url.URL, o Options) *httputil.ReverseProxy {
	rp := &httputil.ReverseProxy{ErrorHandler: unreachable}
	if o.KeepForwarded {
		rp.Director = func(r *http.Request) {
			u := target()
			r.URL.Scheme, r.URL.Host = u.Scheme, u.Host
		}
		return rp
	}
	rp.Rewrite = func(pr *httputil.ProxyRequest) {
		pr.SetURL(target())
		pr.Out.Host = pr.In.Host
		pr.SetXForwarded()
	}
	return rp
}

// Fixed returns a proxy to one parsed target.
func Fixed(u *url.URL, o Options) *httputil.ReverseProxy {
	return To(func() *url.URL { return u }, o)
}

func unreachable(w http.ResponseWriter, _ *http.Request, _ error) {
	http.Error(w, "app unreachable", http.StatusBadGateway)
}

// NormalizeHost lowercases a Host header or SNI name and drops its port and trailing dot.
func NormalizeHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	return strings.TrimSuffix(h, ".")
}

// Serve runs h on ln in the background; a listener that fails is closed.
func Serve(ln net.Listener, h http.Handler) *http.Server {
	s := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := s.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			_ = ln.Close()
		}
	}()
	return s
}
