// Package lanproxy serves workloads on the LAN over HTTPS, routing by Host with certificates
// from a local CA the owner trusts.
package lanproxy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ownship/agent/internal/hostproxy"
)

const (
	DefaultPort = 443
	leafTTL     = 90 * 24 * time.Hour
	// Re-mint a cached leaf this long before it expires.
	leafRenewBefore = 7 * 24 * time.Hour
)

type route struct {
	Workload string `json:"workload"`
	Target   string `json:"target"`
}

type persisted struct {
	Port   int              `json:"port"`
	Routes map[string]route `json:"routes"`
}

type Proxy struct {
	dir string

	mu sync.Mutex
	// Created with the first LAN domain and name-constrained to it.
	caCert   *x509.Certificate
	caKey    *ecdsa.PrivateKey
	caPEM    []byte
	caDomain string

	port    int
	routes  map[string]route
	proxies map[string]*httputil.ReverseProxy
	certs   map[string]*tls.Certificate
	srv     *http.Server
	// Swappable so tests can bind an ephemeral port.
	listen func(port int) (net.Listener, error)
}

func New(dir string) (*Proxy, error) {
	p := &Proxy{
		dir:     dir,
		port:    DefaultPort,
		routes:  map[string]route{},
		proxies: map[string]*httputil.ReverseProxy{},
		certs:   map[string]*tls.Certificate{},
		listen: func(port int) (net.Listener, error) {
			return net.Listen("tcp", fmt.Sprintf(":%d", port))
		},
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := p.loadCA(); err != nil {
		return nil, err
	}
	if data, err := os.ReadFile(p.statePath()); err == nil {
		var st persisted
		if json.Unmarshal(data, &st) == nil {
			if st.Port > 0 {
				p.port = st.Port
			}
			for h, r := range st.Routes {
				p.routes[h] = r
			}
		}
	}
	return p, nil
}

func (p *Proxy) Resume() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.routes) == 0 {
		return nil
	}
	return p.ensureListeningLocked(p.port)
}

// CAPEM is the CA the owner installs on their devices, "" before the first LAN route.
func (p *Proxy) CAPEM() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return string(p.caPEM)
}

// Set replaces the hosts routed to workload. It returns the CA PEM when it created a new CA.
func (p *Proxy) Set(workload string, hosts []string, target string, port int, caDomain string) (string, error) {
	return p.apply(workload, hosts, target, port, caDomain, true)
}

func (p *Proxy) Add(workload string, hosts []string, target string, port int, caDomain string) (string, error) {
	return p.apply(workload, hosts, target, port, caDomain, false)
}

func (p *Proxy) apply(workload string, hosts []string, target string, port int, caDomain string, replace bool) (string, error) {
	if _, err := url.Parse(target); err != nil {
		return "", fmt.Errorf("lan target: %w", err)
	}
	caDomain = hostproxy.NormalizeHost(caDomain)
	if !validHost(caDomain) {
		return "", fmt.Errorf("lan domain %q is not valid", caDomain)
	}
	clean := make([]string, 0, len(hosts))
	for _, h := range hosts {
		h = hostproxy.NormalizeHost(h)
		if !validHost(h) {
			return "", fmt.Errorf("lan hostname %q is not valid", h)
		}
		if !underDomain(h, caDomain) {
			return "", fmt.Errorf("lan hostname %s is outside the LAN domain %s", h, caDomain)
		}
		clean = append(clean, h)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	newCA := ""
	if p.caCert == nil || p.caDomain != caDomain {
		if err := p.createCALocked(caDomain); err != nil {
			return "", err
		}
		newCA = string(p.caPEM)
		// Cached leaves were signed by the old CA, and old routes may be outside the new domain.
		p.certs = map[string]*tls.Certificate{}
		for h := range p.routes {
			if !underDomain(h, caDomain) {
				delete(p.routes, h)
			}
		}
	}
	for h, r := range p.routes {
		if r.Workload != workload {
			continue
		}
		if replace {
			delete(p.routes, h)
			delete(p.certs, h)
		} else {
			p.routes[h] = route{Workload: workload, Target: target}
		}
	}
	for _, h := range clean {
		if r, taken := p.routes[h]; taken && r.Workload != workload {
			return "", fmt.Errorf("lan hostname %s is already routed to another app", h)
		}
		p.routes[h] = route{Workload: workload, Target: target}
	}
	if port <= 0 {
		port = DefaultPort
	}
	if err := p.ensureListeningLocked(port); err != nil {
		return "", err
	}
	return newCA, p.saveLocked()
}

func (p *Proxy) Remove(hosts []string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, h := range hosts {
		h = hostproxy.NormalizeHost(h)
		delete(p.routes, h)
		delete(p.certs, h)
	}
	return p.afterRemovalLocked()
}

// Drop unroutes every host of a workload.
func (p *Proxy) Drop(workload string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for h, r := range p.routes {
		if r.Workload == workload {
			delete(p.routes, h)
			delete(p.certs, h)
		}
	}
	return p.afterRemovalLocked()
}

func (p *Proxy) Hosts(workload string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for h, r := range p.routes {
		if r.Workload == workload {
			out = append(out, h)
		}
	}
	return out
}

func (p *Proxy) afterRemovalLocked() error {
	if len(p.routes) == 0 && p.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = p.srv.Shutdown(ctx)
		p.srv = nil
	}
	return p.saveLocked()
}

func (p *Proxy) ensureListeningLocked(port int) error {
	if p.srv != nil && p.port == port {
		return nil
	}
	if p.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = p.srv.Shutdown(ctx)
		cancel()
		p.srv = nil
	}
	ln, err := p.listen(port)
	if err != nil {
		return fmt.Errorf("lan proxy can't listen on port %d: %w", port, err)
	}
	p.port = port
	srv := &http.Server{
		Handler:           http.HandlerFunc(p.serveHTTP),
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: p.certificate},
	}
	p.srv = srv
	go func() { _ = srv.Serve(tls.NewListener(lanOnly{ln}, srv.TLSConfig)) }()
	return nil
}

func (p *Proxy) serveHTTP(w http.ResponseWriter, r *http.Request) {
	host := hostproxy.NormalizeHost(r.Host)
	p.mu.Lock()
	rt, ok := p.routes[host]
	var rp *httputil.ReverseProxy
	if ok {
		rp = p.proxyForLocked(rt.Target)
	}
	p.mu.Unlock()
	if !ok || rp == nil {
		http.Error(w, "no app is routed at "+host, http.StatusNotFound)
		return
	}
	rp.ServeHTTP(w, r)
}

func (p *Proxy) proxyForLocked(target string) *httputil.ReverseProxy {
	if rp, ok := p.proxies[target]; ok {
		return rp
	}
	u, err := url.Parse(target)
	if err != nil {
		return nil
	}
	rp := hostproxy.Fixed(u, hostproxy.Options{})
	p.proxies[target] = rp
	return rp
}

// certificate signs only routed hosts, so a stray SNI can't get arbitrary names signed.
func (p *Proxy) certificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	host := hostproxy.NormalizeHost(hello.ServerName)
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.routes[host]; !ok || p.caCert == nil {
		return nil, fmt.Errorf("no lan route for %q", host)
	}
	if c, ok := p.certs[host]; ok && time.Until(c.Leaf.NotAfter) > leafRenewBefore {
		return c, nil
	}
	c, err := p.mintLocked(host)
	if err != nil {
		return nil, err
	}
	p.certs[host] = c
	return c, nil
}

func (p *Proxy) mintLocked(host string) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(leafTTL),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.caCert, &key.PublicKey, p.caKey)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{Certificate: [][]byte{der, p.caCert.Raw}, PrivateKey: key, Leaf: leaf}, nil
}

func (p *Proxy) caPaths() (string, string) {
	return filepath.Join(p.dir, "ca.pem"), filepath.Join(p.dir, "ca-key.pem")
}

// loadCA discards a CA without a name constraint, since it could vouch for any site.
func (p *Proxy) loadCA() error {
	certPath, keyPath := p.caPaths()
	certPEM, certErr := os.ReadFile(certPath)
	keyPEM, keyErr := os.ReadFile(keyPath)
	if certErr != nil || keyErr != nil {
		return nil
	}
	cb, _ := pem.Decode(certPEM)
	kb, _ := pem.Decode(keyPEM)
	if cb == nil || kb == nil {
		return errors.New("lan CA files are corrupt")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return err
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return err
	}
	if !cert.PermittedDNSDomainsCritical || len(cert.PermittedDNSDomains) != 1 {
		_ = os.Remove(keyPath)
		_ = os.Remove(certPath)
		return nil
	}
	p.caCert, p.caKey, p.caPEM, p.caDomain = cert, key, certPEM, cert.PermittedDNSDomains[0]
	return nil
}

func (p *Proxy) createCALocked(domain string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Ownship LAN CA (" + domain + ")", Organization: []string{"Ownship"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
		// Valid only for the LAN domain, whatever else a stolen key signs.
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         []string{domain},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	certPath, keyPath := p.caPaths()
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return err
	}
	p.caCert, p.caKey, p.caPEM, p.caDomain = cert, key, certPEM, domain
	return nil
}

func underDomain(host, domain string) bool {
	return host == domain || strings.HasSuffix(host, "."+domain)
}

// lanOnly drops connections from public addresses.
type lanOnly struct{ net.Listener }

var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

func (l lanOnly) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if addr, ok := c.RemoteAddr().(*net.TCPAddr); ok && privateSource(addr.IP) {
			return c, nil
		}
		_ = c.Close()
	}
}

func privateSource(ip net.IP) bool {
	return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || cgnat.Contains(ip)
}

func (p *Proxy) statePath() string { return filepath.Join(p.dir, "routes.json") }

func (p *Proxy) saveLocked() error {
	data, err := json.Marshal(persisted{Port: p.port, Routes: p.routes})
	if err != nil {
		return err
	}
	tmp := p.statePath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p.statePath())
}

func validHost(h string) bool {
	if h == "" || len(h) > 253 || !strings.Contains(h, ".") {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}
