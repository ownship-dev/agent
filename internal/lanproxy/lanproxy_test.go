package lanproxy

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// newTestProxy binds an ephemeral port instead of 443.
func newTestProxy(t *testing.T, dir string) (*Proxy, *string) {
	t.Helper()
	p, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	addr := new(string)
	p.listen = func(int) (net.Listener, error) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err == nil {
			*addr = ln.Addr().String()
		}
		return ln, err
	}
	return p, addr
}

func client(p *Proxy, addr string) *http.Client {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM([]byte(p.CAPEM()))
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}}
}

func TestRoutesByHostWithATrustedCertificate(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "host="+r.Host+" proto="+r.Header.Get("X-Forwarded-Proto"))
	}))
	defer app.Close()

	p, addr := newTestProxy(t, t.TempDir())
	if err := set1(p, "wl1", []string{"App.Lab.Home.Arpa"}, app.URL, 443, "lab.home.arpa"); err != nil {
		t.Fatal(err)
	}
	c := client(p, *addr)

	res, err := c.Get("https://app.lab.home.arpa/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	if string(body) != "host=app.lab.home.arpa proto=https" {
		t.Fatalf("unexpected body %q", body)
	}

	// An unrouted name gets no certificate at all, so the handshake fails.
	if _, err := c.Get("https://other.lab.home.arpa/"); err == nil {
		t.Fatal("expected a TLS failure for an unrouted host")
	}
}

func TestSetReplacesAndDropRemoves(t *testing.T) {
	p, _ := newTestProxy(t, t.TempDir())
	if err := set1(p, "wl1", []string{"a.lab.lan", "b.lab.lan"}, "http://127.0.0.1:1", 443, "lab.lan"); err != nil {
		t.Fatal(err)
	}
	if err := set1(p, "wl1", []string{"a.lab.lan"}, "http://127.0.0.1:2", 443, "lab.lan"); err != nil {
		t.Fatal(err)
	}
	if got := p.Hosts("wl1"); len(got) != 1 || got[0] != "a.lab.lan" {
		t.Fatalf("hosts = %v", got)
	}
	if err := set1(p, "wl2", []string{"a.lab.lan"}, "http://127.0.0.1:3", 443, "lab.lan"); err == nil || !strings.Contains(err.Error(), "another app") {
		t.Fatalf("expected a conflict, got %v", err)
	}
	if err := p.Drop("wl1"); err != nil {
		t.Fatal(err)
	}
	if got := p.Hosts("wl1"); len(got) != 0 {
		t.Fatalf("hosts after drop = %v", got)
	}
}

func TestStatePersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	p, _ := newTestProxy(t, dir)
	if err := set1(p, "wl1", []string{"a.lab.lan"}, "http://127.0.0.1:1", 8443, "lab.lan"); err != nil {
		t.Fatal(err)
	}
	ca := p.CAPEM()

	again, _ := newTestProxy(t, dir)
	if again.CAPEM() != ca {
		t.Fatal("CA was regenerated on restart; devices would stop trusting it")
	}
	if got := again.Hosts("wl1"); len(got) != 1 || again.port != 8443 {
		t.Fatalf("restored hosts=%v port=%d", got, again.port)
	}
}

func TestRejectsInvalidHostnames(t *testing.T) {
	p, _ := newTestProxy(t, t.TempDir())
	for _, h := range []string{"nodot", "bad_char.lab.lan", "-x.lab.lan", "a..lab.lan", "bank.example.com"} {
		if err := set1(p, "wl", []string{h}, "http://127.0.0.1:1", 443, "lab.lan"); err == nil {
			t.Fatalf("%q accepted", h)
		}
	}
}

func set1(p *Proxy, wl string, hosts []string, target string, port int, domain string) error {
	_, err := p.Set(wl, hosts, target, port, domain)
	return err
}

func TestCAIsNameConstrainedToTheLANDomain(t *testing.T) {
	p, _ := newTestProxy(t, t.TempDir())
	ca, err := p.Set("wl1", []string{"app.lab.lan"}, "http://127.0.0.1:1", 443, "lab.lan")
	if err != nil || ca == "" {
		t.Fatalf("first route should create and return the CA: ca=%q err=%v", ca, err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM([]byte(ca))

	// A leaf for a name outside the domain, signed with the real CA key, must not verify.
	p.mu.Lock()
	evil, err := p.mintLocked("bank.example.com")
	p.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := evil.Leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "bank.example.com"}); err == nil {
		t.Fatal("the CA vouched for a name outside the LAN domain")
	}
	p.mu.Lock()
	good, _ := p.mintLocked("app.lab.lan")
	p.mu.Unlock()
	if _, err := good.Leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "app.lab.lan"}); err != nil {
		t.Fatalf("in-domain leaf rejected: %v", err)
	}
}

func TestDomainChangeMintsANewCAAndDropsStaleRoutes(t *testing.T) {
	p, _ := newTestProxy(t, t.TempDir())
	first, _ := p.Set("wl1", []string{"a.lab.lan"}, "http://127.0.0.1:1", 443, "lab.lan")
	again, err := p.Set("wl2", []string{"b.lab.lan"}, "http://127.0.0.1:2", 443, "lab.lan")
	if err != nil || again != "" {
		t.Fatalf("same domain must reuse the CA: %q %v", again, err)
	}
	second, err := p.Set("wl2", []string{"b.home.arpa"}, "http://127.0.0.1:2", 443, "home.arpa")
	if err != nil || second == "" || second == first {
		t.Fatalf("a new domain must mint a new CA: %v", err)
	}
	if len(p.Hosts("wl1")) != 0 {
		t.Fatal("a route outside the new CA's domain was kept")
	}
}

func TestUnconstrainedCAFromAnOlderAgentIsDiscarded(t *testing.T) {
	dir := t.TempDir()
	p, _ := newTestProxy(t, dir)
	p.mu.Lock()
	if err := p.createCALocked("lab.lan"); err != nil {
		t.Fatal(err)
	}
	// Rewrite it without the constraint, as an older agent would have left it.
	p.caCert.PermittedDNSDomainsCritical = false
	tmpl := *p.caCert
	tmpl.PermittedDNSDomains = nil
	der, _ := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &p.caKey.PublicKey, p.caKey)
	p.mu.Unlock()
	certPath, _ := p.caPaths()
	_ = os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)

	if again, _ := newTestProxy(t, dir); again.CAPEM() != "" {
		t.Fatal("an unconstrained CA was loaded")
	}
}

func TestOnlyPrivateSourcesAreServed(t *testing.T) {
	for ip, want := range map[string]bool{
		"192.168.1.20": true, "10.0.0.5": true, "127.0.0.1": true, "100.101.1.2": true,
		"fd00::1": true, "fe80::1": true, "8.8.8.8": false, "2001:4860::1": false,
	} {
		if got := privateSource(net.ParseIP(ip)); got != want {
			t.Errorf("%s: got %v want %v", ip, got, want)
		}
	}
}
