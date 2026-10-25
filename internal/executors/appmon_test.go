package executors

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

type appmonHarness struct {
	m      *AppMonitor
	status atomic.Int32
	events []*agentv1.AppHealth
	now    time.Time
}

func newAppmonHarness(t *testing.T) *appmonHarness {
	t.Helper()
	h := &appmonHarness{now: time.Unix(1_700_000_000, 0)}
	h.status.Store(200)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/up" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(int(h.status.Load()))
	}))
	t.Cleanup(srv.Close)
	h.m = NewAppMonitor(filepath.Join(t.TempDir(), "appmon.json"))
	h.m.SetEmit(func(e *agentv1.Event) error {
		h.events = append(h.events, e.GetAppHealth())
		return nil
	})
	h.m.Register("p1", srv.URL, &agentv1.HealthcheckConfig{Enabled: true, Path: "up"}, nil)
	return h
}

func (h *appmonHarness) tick(d time.Duration) {
	h.now = h.now.Add(d)
	h.m.probeOnce(context.Background(), h.now)
}

func TestAppMonitorReportsTheFirstSuccessOnce(t *testing.T) {
	h := newAppmonHarness(t)
	h.tick(0)
	h.tick(30 * time.Second)
	if len(h.events) != 1 || !h.events[0].Healthy || h.events[0].ProjectId != "p1" {
		t.Fatalf("events = %+v", h.events)
	}
}

func TestAppMonitorNeedsThreeFailuresAndRecoversOnOne(t *testing.T) {
	h := newAppmonHarness(t)
	h.tick(0)
	h.status.Store(503)
	h.tick(30 * time.Second)
	h.tick(30 * time.Second)
	if len(h.events) != 1 {
		t.Fatalf("two misses must stay quiet, got %d events", len(h.events))
	}
	h.tick(30 * time.Second)
	last := h.events[len(h.events)-1]
	if len(h.events) != 2 || last.Healthy || last.Reason != "status Service Unavailable" || last.StatusCode != 503 {
		t.Fatalf("third miss: %+v", h.events)
	}
	h.status.Store(200)
	h.tick(30 * time.Second)
	if last = h.events[len(h.events)-1]; len(h.events) != 3 || !last.Healthy {
		t.Fatalf("recovery: %+v", h.events)
	}
}

func TestAppMonitorRepeatsItsStateAsAKeepalive(t *testing.T) {
	h := newAppmonHarness(t)
	h.tick(0)
	h.tick(appKeepalive - time.Second)
	if len(h.events) != 1 {
		t.Fatalf("no keepalive before the interval, got %d", len(h.events))
	}
	h.tick(2 * time.Second)
	if len(h.events) != 2 || !h.events[1].Healthy {
		t.Fatalf("keepalive: %+v", h.events)
	}
}

func TestAppMonitorSleepIsNotAnOutage(t *testing.T) {
	h := newAppmonHarness(t)
	h.tick(0)
	h.m.Pause("p1")
	h.status.Store(500)
	for range 5 {
		h.tick(30 * time.Second)
	}
	if len(h.events) != 1 {
		t.Fatalf("a paused workload must not report, got %+v", h.events)
	}
	h.m.Resume("p1")
	h.status.Store(200)
	h.tick(30 * time.Second)
	if len(h.events) != 2 || !h.events[1].Healthy {
		t.Fatalf("resume reports in again: %+v", h.events)
	}
}

func TestAppMonitorOptOutAndRemoval(t *testing.T) {
	h := newAppmonHarness(t)
	h.m.Register("p1", "http://127.0.0.1:1", &agentv1.HealthcheckConfig{Enabled: false}, nil)
	h.tick(0)
	if len(h.events) != 0 || len(h.m.entries) != 0 {
		t.Fatalf("a disabled health check is not watched: %+v", h.events)
	}
}

func TestAppMonitorRegistrationsSurviveARestart(t *testing.T) {
	h := newAppmonHarness(t)
	again := NewAppMonitor(h.m.path)
	if e, ok := again.entries["p1"]; !ok || e.Path != "/up" {
		t.Fatalf("entries = %+v", again.entries)
	}
	h.m.Remove("p1")
	if len(NewAppMonitor(h.m.path).entries) != 0 {
		t.Fatal("removal must persist")
	}
}

func TestAppMonitorUnreachableAppIsRefused(t *testing.T) {
	m := NewAppMonitor(filepath.Join(t.TempDir(), "appmon.json"))
	var got []*agentv1.AppHealth
	m.SetEmit(func(e *agentv1.Event) error { got = append(got, e.GetAppHealth()); return nil })
	m.Register("p2", "http://127.0.0.1:1", &agentv1.HealthcheckConfig{Enabled: true}, nil)
	now := time.Unix(1_700_000_000, 0)
	for i := range 3 {
		m.probeOnce(context.Background(), now.Add(time.Duration(i)*30*time.Second))
	}
	if len(got) != 1 || got[0].Healthy || got[0].Reason != "connection refused" {
		t.Fatalf("got %+v", got)
	}
}

func TestAppMonitorAGuestStoppedInProxmoxIsReportedAtOnce(t *testing.T) {
	h := newAppmonHarness(t)
	var stopped atomic.Bool
	h.m.SetGuestStopped(func(context.Context, string) bool { return stopped.Load() })
	h.tick(0)

	h.status.Store(503)
	stopped.Store(true)
	h.tick(30 * time.Second)
	if len(h.events) != 2 || h.events[1].Healthy || !h.events[1].GuestStopped {
		t.Fatalf("a stopped guest is reported on the first miss: %+v", h.events)
	}

	// Started again but still booting: it stays "stopped" until its app has had its misses.
	stopped.Store(false)
	h.tick(30 * time.Second)
	h.tick(30 * time.Second)
	if len(h.events) != 2 {
		t.Fatalf("no report while the app boots: %+v", h.events)
	}
	h.tick(30 * time.Second)
	if len(h.events) != 3 || h.events[2].Healthy || h.events[2].GuestStopped {
		t.Fatalf("a running guest whose app never answers is down: %+v", h.events)
	}

	h.status.Store(200)
	h.tick(30 * time.Second)
	if len(h.events) != 4 || !h.events[3].Healthy || h.events[3].GuestStopped {
		t.Fatalf("events = %+v", h.events)
	}
}

func TestAppMonitorAStoppedGuestThatComesBackHealthyRecovers(t *testing.T) {
	h := newAppmonHarness(t)
	h.m.SetGuestStopped(func(context.Context, string) bool { return true })
	h.status.Store(503)
	h.tick(0)
	h.status.Store(200)
	h.tick(30 * time.Second)
	if len(h.events) != 2 || !h.events[0].GuestStopped || !h.events[1].Healthy {
		t.Fatalf("events = %+v", h.events)
	}
}
