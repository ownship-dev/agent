package executors

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func noProbe(context.Context, *Engine, string) bool { return false }

func TestEngineStatePersistsAndReloads(t *testing.T) {
	p := filepath.Join(t.TempDir(), "engines.json")
	engines := []*Engine{NewDockerEngine(), NewPodmanEngine()}
	s := loadEngineState(p, engines, noProbe)
	if err := s.Set("w1", KindPodman); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAsleep("w1", true); err != nil {
		t.Fatal(err)
	}
	r := loadEngineState(p, engines, nil)
	if k, ok := r.Recorded("w1"); !ok || k != KindPodman {
		t.Fatalf("got %v %v", k, ok)
	}
	if !slices.Equal(r.Asleep(), []string{"w1"}) {
		t.Fatalf("asleep %v", r.Asleep())
	}
}

func TestEngineStateProbesUnrecordedWorkloadAndWritesBack(t *testing.T) {
	p := filepath.Join(t.TempDir(), "engines.json")
	engines := []*Engine{NewDockerEngine(), NewPodmanEngine()}
	probed := 0
	s := loadEngineState(p, engines, func(_ context.Context, e *Engine, id string) bool {
		probed++
		return e.Kind == KindPodman && id == "old"
	})
	if got := s.For(context.Background(), "old"); got.Kind != KindPodman {
		t.Fatalf("got %v", got.Kind)
	}
	s.For(context.Background(), "old")
	if probed != 2 {
		t.Fatalf("second lookup must hit the written-back entry, probed=%d", probed)
	}
	if k, ok := loadEngineState(p, engines, nil).Recorded("old"); !ok || k != KindPodman {
		t.Fatal("the probe result must be persisted")
	}
}

func TestEngineStateFallsBackToDockerWithoutRecording(t *testing.T) {
	s := loadEngineState(filepath.Join(t.TempDir(), "e.json"), []*Engine{NewDockerEngine()}, noProbe)
	if s.For(context.Background(), "nothing").Kind != KindDocker {
		t.Fatal("unknown workload must default to docker")
	}
	if _, ok := s.Recorded("nothing"); ok {
		t.Fatal("a guess must not be persisted")
	}
}

func TestEngineStateRecordedButEngineGone(t *testing.T) {
	p := filepath.Join(t.TempDir(), "e.json")
	s := loadEngineState(p, []*Engine{NewDockerEngine(), NewPodmanEngine()}, nil)
	_ = s.Set("w", KindPodman)
	r := loadEngineState(p, []*Engine{NewDockerEngine()}, nil)
	if e := r.For(context.Background(), "w"); e.Kind != KindPodman || e.Bin != "podman" {
		t.Fatalf("must keep the recorded kind even when undetected now, got %v", e.Kind)
	}
	if _, ok := r.Engine(KindPodman); ok {
		t.Fatal("Engine reports only detected engines")
	}
}

func TestEngineStateRemoveAndUnspecified(t *testing.T) {
	s := loadEngineState(filepath.Join(t.TempDir(), "e.json"), []*Engine{NewDockerEngine()}, noProbe)
	_ = s.Set("w", 0)
	if k, _ := s.Recorded("w"); k != KindDocker {
		t.Fatalf("unspecified must be stored as docker, got %v", k)
	}
	_ = s.Remove("w")
	if _, ok := s.Recorded("w"); ok {
		t.Fatal("removed")
	}
}

func TestEngineStatePendingOld(t *testing.T) {
	p := filepath.Join(t.TempDir(), "e.json")
	s := loadEngineState(p, []*Engine{NewDockerEngine(), NewPodmanEngine()}, nil)
	_ = s.Set("w", KindPodman)
	_ = s.SetPendingOld("w", KindDocker)
	r := loadEngineState(p, nil, nil)
	if k, ok := r.PendingOld("w"); !ok || k != KindDocker {
		t.Fatalf("pending old %v %v", k, ok)
	}
	_ = r.ClearPendingOld("w")
	if _, ok := r.PendingOld("w"); ok {
		t.Fatal("cleared")
	}
}

// Without its user a rootless engine must fail, never fall back to podman as root, which is
// the rootful store: a pending rootless cleanup would delete a live rootful workload.
func TestUnresolvableRootlessEngineFailsClosed(t *testing.T) {
	s := loadEngineState(filepath.Join(t.TempDir(), "e.json"), []*Engine{NewPodmanEngine()}, nil)
	e := s.unresolvableRootless("no such user")
	cmd := e.Cmd(context.Background(), "rm", "-f", "ownship-w")
	if cmd.Args[0] == "podman" {
		t.Fatalf("would run podman as root: %v", cmd.Args)
	}
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), RootlessUser) {
		t.Fatalf("must fail naming the user: err=%v out=%q", err, out)
	}
	if !slices.Equal(e.ExecArgv("exec"), e.Cmd(context.Background(), "exec").Args) {
		t.Fatal("terminal argv must fail the same way")
	}
}
