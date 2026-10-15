package executors

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

// Waker proxies wake-on-request projects and boots them on the first request while asleep.
// Registrations are in memory, so after a restart a manual wake is needed.

const (
	defaultWakerPort = 18719
	wakeHoldFast     = 5 * time.Second  // browsers get the interstitial past this
	wakeHoldMax      = 90 * time.Second // then 504
)

type wakerEntry struct {
	workload  string
	target    *url.URL
	proxy     *httputil.ReverseProxy
	hostnames []string
	health    *agentv1.HealthcheckConfig
	asleep    bool
	waking    chan struct{} // non-nil while a wake is in flight
	wakeErr   error
}

type Waker struct {
	orch Orchestrator
	rtl  *RuntimeLogs
	emit atomic.Value

	mu      sync.Mutex
	entries map[string]*wakerEntry
	byHost  map[string]string // hostname -> workload id
	started bool
	port    int
}

func NewWaker(orch Orchestrator, rtl *RuntimeLogs) *Waker {
	w := &Waker{orch: orch, rtl: rtl, entries: map[string]*wakerEntry{}, byHost: map[string]string{}, port: defaultWakerPort}
	w.emit.Store(Emit(func(*agentv1.Event) error { return nil }))
	return w
}

func (w *Waker) SetEmit(emit Emit) { w.emit.Store(emit) }

// URL is what the tunnel routes to; OWNSHIP_WAKER_URL overrides it.
func (w *Waker) URL() string {
	if env := os.Getenv("OWNSHIP_WAKER_URL"); env != "" {
		return env
	}
	return fmt.Sprintf("http://%s:%d", hostReachableIP(), w.port)
}

// Register binds a workload after a wake-on-request deploy, starting the listener on first use.
func (w *Waker) Register(workload string, hostnames []string, target string, health *agentv1.HealthcheckConfig) error {
	parsed, err := url.Parse(target)
	if err != nil {
		return fmt.Errorf("waker target: %w", err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	// Drop hostnames from a previous deploy.
	for host, wl := range w.byHost {
		if wl == workload {
			delete(w.byHost, host)
		}
	}
	entry := &wakerEntry{
		workload:  workload,
		target:    parsed,
		proxy:     httputil.NewSingleHostReverseProxy(parsed),
		hostnames: hostnames,
		health:    health,
	}
	entry.proxy.ErrorHandler = func(rw http.ResponseWriter, _ *http.Request, _ error) {
		rw.WriteHeader(http.StatusBadGateway)
		_, _ = rw.Write([]byte("app unreachable"))
	}
	w.entries[workload] = entry
	for _, h := range hostnames {
		w.byHost[strings.ToLower(h)] = workload
	}
	return w.ensureListener()
}

func (w *Waker) Unregister(workload string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.entries, workload)
	for host, wl := range w.byHost {
		if wl == workload {
			delete(w.byHost, host)
		}
	}
}

// MarkAsleep reports whether the workload was registered.
func (w *Waker) MarkAsleep(workload string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	entry := w.entries[workload]
	if entry == nil {
		return false
	}
	entry.asleep = true
	return true
}

// MarkAwake handles a manual wake from the control plane.
func (w *Waker) MarkAwake(workload string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if entry := w.entries[workload]; entry != nil {
		entry.asleep = false
	}
}

func (w *Waker) ensureListener() error {
	if w.started {
		return nil
	}
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", w.port))
	if err != nil {
		return fmt.Errorf("waker listen :%d: %w", w.port, err)
	}
	server := &http.Server{Handler: w, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = server.Serve(ln) }()
	w.started = true
	return nil
}

func (w *Waker) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	host := strings.ToLower(r.Host)
	if i := strings.LastIndex(host, ":"); i > 0 && !strings.Contains(host[i:], "]") {
		host = host[:i]
	}
	w.mu.Lock()
	entry := w.entries[w.byHost[host]]
	w.mu.Unlock()
	if entry == nil {
		http.Error(rw, "unknown app", http.StatusBadGateway)
		return
	}

	w.mu.Lock()
	asleep := entry.asleep
	if asleep && entry.waking == nil {
		// The first request starts the wake; the rest wait.
		entry.waking = make(chan struct{})
		go w.wake(entry)
	}
	waking := entry.waking
	w.mu.Unlock()

	if !asleep {
		entry.proxy.ServeHTTP(rw, r)
		return
	}

	// Past a short hold browsers get a refreshing interstitial; other clients keep waiting.
	fast := time.NewTimer(wakeHoldFast)
	defer fast.Stop()
	select {
	case <-waking:
	case <-fast.C:
		if strings.Contains(r.Header.Get("Accept"), "text/html") {
			rw.Header().Set("Retry-After", "3")
			rw.Header().Set("Content-Type", "text/html; charset=utf-8")
			rw.WriteHeader(http.StatusServiceUnavailable)
			_, _ = rw.Write([]byte(`<!doctype html><meta http-equiv="refresh" content="3"><title>Waking up…</title>
<body style="background:#0a0a0c;color:#ededf0;font-family:system-ui;display:grid;place-items:center;min-height:100vh;margin:0">
<div style="text-align:center"><p style="font-size:2rem;margin:0">⏳</p><p>This app was asleep — waking it up now.</p>
<p style="color:#8b8b93;font-size:.85rem">This page refreshes automatically.</p></div>`))
			return
		}
		slow := time.NewTimer(wakeHoldMax)
		defer slow.Stop()
		select {
		case <-waking:
		case <-slow.C:
			http.Error(rw, "wake timed out", http.StatusGatewayTimeout)
			return
		}
	}

	w.mu.Lock()
	err := entry.wakeErr
	w.mu.Unlock()
	if err != nil {
		http.Error(rw, "wake failed", http.StatusBadGateway)
		return
	}
	entry.proxy.ServeHTTP(rw, r)
}

// wake starts the workload, waits for health and reports upstream, like a Wake command.
// Start and the health gate get separate budgets so a slow start can't fail the check.
func (w *Waker) wake(entry *wakerEntry) {
	startCtx, startCancel := context.WithTimeout(context.Background(), wakeHoldMax)
	err := w.orch.Start(startCtx, entry.workload)
	startCancel()

	if err == nil && entry.health.GetEnabled() {
		healthTimeout := time.Duration(entry.health.GetTimeoutSeconds()) * time.Second
		if healthTimeout <= 0 {
			healthTimeout = 60 * time.Second
		}
		healthCtx, healthCancel := context.WithTimeout(context.Background(), healthTimeout)
		err = waitHealthy(healthCtx, entry.target.String(), entry.health, nil, func(agentv1.LogStream, string) {})
		healthCancel()
	}

	w.mu.Lock()
	entry.wakeErr = err
	if err == nil {
		entry.asleep = false
	}
	waking := entry.waking
	entry.waking = nil
	w.mu.Unlock()
	close(waking)

	if err == nil {
		w.rtl.Start(entry.workload)
		emit := w.emit.Load().(Emit)
		_ = emit(sleepState(entry.workload, false))
	}
}

// hostReachableIP finds the outbound interface's address; no packets are sent.
func hostReachableIP() string {
	conn, err := net.Dial("udp", "192.0.2.1:1") // TEST-NET, never routed
	if err != nil {
		return "127.0.0.1"
	}
	defer conn.Close()
	if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		return addr.IP.String()
	}
	return "127.0.0.1"
}
