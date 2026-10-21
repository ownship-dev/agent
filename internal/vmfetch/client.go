package vmfetch

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// Policy says which hosts a request, and every redirect it follows, may reach.
type Policy int

const (
	// Strict is for checksum files and signatures: allowlisted hosts only, redirects included.
	Strict Policy = iota
	// Mirror is for a catalogue image: it starts on the allowlist and may be redirected to any
	// public mirror, since its bytes are checked against the checksum.
	Mirror
	// Open is for a user's link: any public host, checked against the user's own hash.
	Open
)

const (
	maxRedirects = 5
	// A stalled transfer fails rather than holding a deploy for the whole fetch timeout.
	idleTimeout = 2 * time.Minute
)

// ErrNotPublic is returned for a connection to loopback, private, link-local or metadata space.
var ErrNotPublic = errors.New("refusing a non-public address")

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// publicAddr is the dial-time guard; tests replace it to reach httptest servers.
var publicAddr = func(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsGlobalUnicast() && !a.IsPrivate() && !a.IsLoopback() && !a.IsLinkLocalUnicast() &&
		!cgnat.Contains(a) && !(a.Is4() && a.As4()[0] == 0)
}

// allowedHost is AllowedHost unless a test widens it.
var allowedHost = AllowedHost

// transportHook lets tests trust their own TLS server.
var transportHook func(*http.Transport)

// CheckURL is the shape every fetched URL must have: https, no user info, default port.
func CheckURL(raw string, p Policy) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Opaque != "" {
		return nil, fmt.Errorf("not an https URL")
	}
	if port := u.Port(); port != "" && port != "443" {
		return nil, fmt.Errorf("only the default https port is allowed")
	}
	if p != Open && !allowedHost(u.Hostname()) {
		return nil, fmt.Errorf("host %s is not on the agent's image allowlist", u.Hostname())
	}
	return u, nil
}

func newClient(p Policy) *http.Client {
	dialer := &net.Dialer{
		Timeout: 30 * time.Second,
		// Runs on the address actually dialled, so DNS rebinding cannot slip past it.
		Control: func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil || !publicAddr(ap.Addr()) {
				return ErrNotPublic
			}
			return nil
		},
	}
	tr := &http.Transport{
		// No proxy: the guard must see the real destination.
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: time.Minute,
		ForceAttemptHTTP2:     true,
	}
	if transportHook != nil {
		transportHook(tr)
	}
	return &http.Client{
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("too many redirects")
			}
			next := p
			if p == Mirror {
				next = Open
			}
			_, err := CheckURL(req.URL.String(), next)
			return err
		},
	}
}

func get(ctx context.Context, raw string, p Policy) (*http.Response, error) {
	if _, err := CheckURL(raw, p); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	resp, err := newClient(p).Do(req)
	if err != nil {
		return nil, scrub(err, raw)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("%s answered %s", hostOf(raw), resp.Status)
	}
	return resp, nil
}

// Small fetches a checksum file or signature, at most limit bytes.
func Small(ctx context.Context, raw string, limit int64) ([]byte, error) {
	resp, err := get(ctx, raw, Strict)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, scrub(err, raw)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", hostOf(raw), limit)
	}
	return data, nil
}

// Sums are the digests of a download, computed while it is written.
type Sums struct {
	SHA256 string
	SHA512 string
	Bytes  int64
}

// Download streams raw into w, refusing more than maxBytes. progress, if set, gets the bytes
// written and the announced total (0 when unknown).
func Download(ctx context.Context, raw string, p Policy, w io.Writer, maxBytes int64, progress func(done, total int64)) (Sums, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	resp, err := get(ctx, raw, p)
	if err != nil {
		return Sums{}, err
	}
	defer resp.Body.Close()
	total := resp.ContentLength
	if total > maxBytes {
		return Sums{}, fmt.Errorf("the image is %d bytes, over the %d limit", total, maxBytes)
	}
	if total < 0 {
		total = 0
	}
	idle := time.AfterFunc(idleTimeout, func() { cancel(fmt.Errorf("no data for %s", idleTimeout)) })
	defer idle.Stop()
	h256, h512 := sha256.New(), sha512.New()
	out := io.MultiWriter(w, h256, h512)
	buf := make([]byte, 1<<20)
	var done int64
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			idle.Reset(idleTimeout)
			done += int64(n)
			if done > maxBytes {
				return Sums{}, fmt.Errorf("the image is over the %d byte limit", maxBytes)
			}
			if _, err := out.Write(buf[:n]); err != nil {
				return Sums{}, err
			}
			if progress != nil {
				progress(done, total)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			if c := context.Cause(ctx); c != nil {
				return Sums{}, c
			}
			return Sums{}, scrub(rerr, raw)
		}
	}
	if total > 0 && done != total {
		return Sums{}, fmt.Errorf("the download ended at %d of %d bytes", done, total)
	}
	return Sums{SHA256: hex.EncodeToString(h256.Sum(nil)), SHA512: hex.EncodeToString(h512.Sum(nil)), Bytes: done}, nil
}

func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Hostname()
	}
	return "the server"
}

// scrub drops the URL from an error: a link's query string can carry a token.
func scrub(err error, raw string) error {
	msg := err.Error()
	if u, perr := url.Parse(raw); perr == nil {
		msg = strings.ReplaceAll(msg, raw, u.Hostname())
		msg = strings.ReplaceAll(msg, `"`+u.Hostname()+`"`, u.Hostname())
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		msg = ue.Op + " " + hostOf(raw) + ": " + scrubInner(ue.Err)
	}
	return errors.New(msg)
}

func scrubInner(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Op + " " + hostOf(ue.URL) + ": " + scrubInner(ue.Err)
	}
	return err.Error()
}
