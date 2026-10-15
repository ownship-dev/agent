// Package tsingress puts each workload on the owner's tailnet as its own node.
package tsingress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"tailscale.com/tsnet"
)

const upTimeout = 90 * time.Second

// node is the part of tsnet.Server used here, so tests can fake it.
type node interface {
	Up(ctx context.Context) error
	CertDomains() []string
	Listen(network, addr string) (net.Listener, error)
	ListenTLS(network, addr string) (net.Listener, error)
	Logout(ctx context.Context) error
	Close() error
}

type tsnetNode struct{ *tsnet.Server }

func (n tsnetNode) Up(ctx context.Context) error {
	_, err := n.Server.Up(ctx)
	return err
}

func (n tsnetNode) Logout(ctx context.Context) error {
	lc, err := n.Server.LocalClient()
	if err != nil {
		return err
	}
	return lc.Logout(ctx)
}

type record struct {
	Hostname string `json:"hostname"`
	Target   string `json:"target"`
}

type running struct {
	node     node
	hostname string
	target   atomic.Pointer[url.URL]
	url      string
	servers  []*http.Server
}

type Ingress struct {
	dir     string
	newNode func(hostname, dir, authKey string) node

	mu    sync.Mutex
	nodes map[string]*running
	saved map[string]record
}

func New(dir string) *Ingress {
	i := &Ingress{
		dir:   dir,
		nodes: map[string]*running{},
		saved: map[string]record{},
		newNode: func(hostname, dir, authKey string) node {
			return tsnetNode{&tsnet.Server{Hostname: hostname, Dir: dir, AuthKey: authKey, Logf: func(string, ...any) {}}}
		},
	}
	if data, err := os.ReadFile(i.statePath()); err == nil {
		_ = json.Unmarshal(data, &i.saved)
	}
	return i
}

// Resume restores nodes after a restart; their identity is on disk, so no auth key is needed.
func (i *Ingress) Resume(ctx context.Context, logf func(string)) {
	i.mu.Lock()
	saved := make(map[string]record, len(i.saved))
	for k, v := range i.saved {
		saved[k] = v
	}
	i.mu.Unlock()
	for wl, r := range saved {
		if _, err := i.Ensure(ctx, wl, r.Hostname, "", r.Target); err != nil {
			logf("tailscale node for " + wl + " did not come back: " + err.Error())
		}
	}
}

var hostnameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// Ensure starts or retargets the workload's node and returns its URL.
func (i *Ingress) Ensure(ctx context.Context, workload, hostname, authKey, target string) (string, error) {
	if !hostnameRE.MatchString(hostname) {
		return "", fmt.Errorf("tailscale hostname %q is not valid", hostname)
	}
	u, err := url.Parse(target)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("tailscale target %q is not valid", target)
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	if r, ok := i.nodes[workload]; ok {
		if r.hostname == hostname {
			r.target.Store(u)
			i.saved[workload] = record{Hostname: hostname, Target: target}
			return r.url, i.saveLocked()
		}
		i.stopLocked(r)
		delete(i.nodes, workload)
	}

	dir := i.nodeDir(workload)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	n := i.newNode(hostname, dir, authKey)
	upCtx, cancel := context.WithTimeout(ctx, upTimeout)
	defer cancel()
	if err := n.Up(upCtx); err != nil {
		_ = n.Close()
		if authKey == "" {
			return "", fmt.Errorf("tailscale login: %w (no auth key on this deploy; the node may have been removed from the tailnet)", err)
		}
		return "", fmt.Errorf("tailscale login: %w", err)
	}

	r := &running{node: n, hostname: hostname}
	r.target.Store(u)
	proxy := &httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) {
		pr.SetURL(r.target.Load())
		pr.Out.Host = pr.In.Host
		pr.SetXForwarded()
	}}

	plain, err := n.Listen("tcp", ":80")
	if err != nil {
		_ = n.Close()
		return "", fmt.Errorf("tailscale listen :80: %w", err)
	}
	r.servers = append(r.servers, serve(plain, proxy))
	r.url = "http://" + hostname
	// HTTPS needs MagicDNS and HTTPS certificates on the tailnet; otherwise plain HTTP is used
	// inside the already encrypted tailnet.
	if domains := n.CertDomains(); len(domains) > 0 {
		if tlsLn, err := n.ListenTLS("tcp", ":443"); err == nil {
			r.servers = append(r.servers, serve(tlsLn, proxy))
			r.url = "https://" + domains[0]
		}
	}

	i.nodes[workload] = r
	i.saved[workload] = record{Hostname: hostname, Target: target}
	return r.url, i.saveLocked()
}

// Drop removes the node from the tailnet and forgets its identity.
func (i *Ingress) Drop(ctx context.Context, workload string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if r, ok := i.nodes[workload]; ok {
		logoutCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		_ = r.node.Logout(logoutCtx)
		cancel()
		i.stopLocked(r)
		delete(i.nodes, workload)
	}
	_, had := i.saved[workload]
	delete(i.saved, workload)
	if err := os.RemoveAll(i.nodeDir(workload)); err != nil {
		return err
	}
	if !had {
		return nil
	}
	return i.saveLocked()
}

func (i *Ingress) Has(workload string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	_, ok := i.saved[workload]
	return ok
}

func (i *Ingress) stopLocked(r *running) {
	for _, s := range r.servers {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = s.Shutdown(ctx)
		cancel()
	}
	_ = r.node.Close()
}

func serve(ln net.Listener, h http.Handler) *http.Server {
	s := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := s.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			_ = ln.Close()
		}
	}()
	return s
}

// Base keeps a malformed id inside dir.
func (i *Ingress) nodeDir(workload string) string {
	return filepath.Join(i.dir, filepath.Base(workload))
}

func (i *Ingress) statePath() string { return filepath.Join(i.dir, "nodes.json") }

func (i *Ingress) saveLocked() error {
	if err := os.MkdirAll(i.dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(i.saved)
	if err != nil {
		return err
	}
	tmp := i.statePath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, i.statePath())
}
