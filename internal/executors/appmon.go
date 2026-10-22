package executors

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

const (
	appProbeInterval = 30 * time.Second
	appProbeTimeout  = 5 * time.Second
	// Three misses in a row (about 90s) before an app counts as down, so a slow request or a
	// restart does not page anyone.
	appFailuresToFail = 3
	// Re-sent while the state holds, so the control plane can tell silence from "still fine".
	appKeepalive   = 2 * time.Minute
	appProbeFanout = 8
)

// AppMonitor probes each deployed app on its health path from the machine, so private, LAN and
// Tailscale apps are watched too. Registrations persist, since containers outlive the agent.
type AppMonitor struct {
	path   string
	client *http.Client
	emit   atomic.Value

	mu      sync.Mutex
	entries map[string]appEntry
	state   map[string]*appState
}

type appEntry struct {
	URL  string `json:"url"`
	Path string `json:"path"`
	User string `json:"user,omitempty"`
	Pass string `json:"pass,omitempty"`
}

type appState struct {
	fails    int
	known    bool
	healthy  bool
	paused   bool
	lastEmit time.Time
}

func NewAppMonitor(path string) *AppMonitor {
	m := &AppMonitor{
		path:    path,
		entries: map[string]appEntry{},
		state:   map[string]*appState{},
		// A redirect to a login page is the app answering, so only the first response counts.
		client: &http.Client{
			Timeout:       appProbeTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	m.emit.Store(Emit(func(*agentv1.Event) error { return nil }))
	if data, err := os.ReadFile(path); err == nil {
		var saved struct {
			Entries map[string]appEntry `json:"entries"`
		}
		if json.Unmarshal(data, &saved) == nil && saved.Entries != nil {
			m.entries = saved.Entries
		}
	}
	return m
}

func (m *AppMonitor) SetEmit(emit Emit) {
	if m != nil {
		m.emit.Store(emit)
	}
}

// Register starts watching a workload after a deploy passed its gate. A disabled health check
// means the owner opted out, so it drops any earlier registration instead.
func (m *AppMonitor) Register(workload, baseURL string, hc *agentv1.HealthcheckConfig, auth *agentv1.BasicAuth) {
	if m == nil {
		return
	}
	if !hc.GetEnabled() {
		m.Remove(workload)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries[workload] = appEntry{
		URL:  strings.TrimSuffix(baseURL, "/"),
		Path: normalizeHealthPath(hc.GetPath()),
		User: auth.GetUser(),
		Pass: auth.GetPassword(),
	}
	m.state[workload] = &appState{}
	_ = m.flush()
}

func (m *AppMonitor) Remove(workload string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.entries[workload]; !ok {
		return
	}
	delete(m.entries, workload)
	delete(m.state, workload)
	_ = m.flush()
}

// Pause is a sleep: a stopped container is not an outage.
func (m *AppMonitor) Pause(workload string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stateFor(workload).paused = true
}

// Resume starts from a clean slate, so the first answer after a wake reports in.
func (m *AppMonitor) Resume(workload string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state[workload] = &appState{}
}

func (m *AppMonitor) stateFor(workload string) *appState {
	st := m.state[workload]
	if st == nil {
		st = &appState{}
		m.state[workload] = st
	}
	return st
}

func (m *AppMonitor) flush() error {
	if err := os.MkdirAll(filepath.Dir(m.path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(struct {
		Entries map[string]appEntry `json:"entries"`
	}{m.entries})
	if err != nil {
		return err
	}
	return writeFileAtomic(m.path, data, 0o600)
}

// Run probes until ctx ends.
func (m *AppMonitor) Run(ctx context.Context) {
	if m == nil {
		return
	}
	ticker := time.NewTicker(appProbeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.probeOnce(ctx, time.Now())
		}
	}
}

type appProbe struct {
	workload string
	entry    appEntry
}

type appResult struct {
	ok     bool
	status int
	reason string
}

// probeOnce checks every unpaused workload and reports the ones whose state changed or is due a keepalive.
func (m *AppMonitor) probeOnce(ctx context.Context, now time.Time) {
	m.mu.Lock()
	var due []appProbe
	for wl, e := range m.entries {
		if st := m.state[wl]; st != nil && st.paused {
			continue
		}
		due = append(due, appProbe{wl, e})
	}
	m.mu.Unlock()

	results := make([]appResult, len(due))
	sem := make(chan struct{}, appProbeFanout)
	var wg sync.WaitGroup
	for i, p := range due {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = m.probe(ctx, p.entry)
		}()
	}
	wg.Wait()

	emit := m.emit.Load().(Emit)
	for i, p := range due {
		if ev := m.apply(p.workload, results[i], now); ev != nil {
			_ = emit(ev)
		}
	}
}

func (m *AppMonitor) probe(ctx context.Context, e appEntry) appResult {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.URL+e.Path, nil)
	if err != nil {
		return appResult{reason: "invalid health check URL"}
	}
	if e.User != "" || e.Pass != "" {
		req.SetBasicAuth(e.User, e.Pass)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return appResult{reason: probeFailure(err)}
	}
	resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		return appResult{ok: true, status: resp.StatusCode}
	}
	return appResult{status: resp.StatusCode, reason: "status " + http.StatusText(resp.StatusCode)}
}

// probeFailure names a failure without echoing the transport error, which can carry addresses.
func probeFailure(err error) string {
	var ne net.Error
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.As(err, &ne) && ne.Timeout():
		return "timed out"
	default:
		return "no response"
	}
}

// apply folds one result into the workload's state and returns the event to send, if any.
func (m *AppMonitor) apply(workload string, r appResult, now time.Time) *agentv1.Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.entries[workload]; !ok {
		return nil
	}
	st := m.stateFor(workload)
	if st.paused {
		return nil
	}
	changed := false
	if r.ok {
		st.fails = 0
		changed = !st.known || !st.healthy
		st.known, st.healthy = true, true
	} else {
		st.fails++
		if st.fails >= appFailuresToFail {
			changed = !st.known || st.healthy
			st.known, st.healthy = true, false
		}
	}
	if !changed && !(st.known && now.Sub(st.lastEmit) >= appKeepalive) {
		return nil
	}
	st.lastEmit = now
	reason := ""
	if !st.healthy {
		reason = r.reason
	}
	return &agentv1.Event{Payload: &agentv1.Event_AppHealth{AppHealth: &agentv1.AppHealth{
		ProjectId:  workload,
		Healthy:    st.healthy,
		Reason:     reason,
		StatusCode: uint32(r.status),
		Ts:         now.UnixMilli(),
	}}}
}
