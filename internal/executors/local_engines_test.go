package executors

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

// fakeEngineCmds swaps runEngineCmd for the test and records every argv it sees.
func fakeEngineCmds(t *testing.T, answer func(args []string) ([]byte, error)) *[][]string {
	t.Helper()
	var mu sync.Mutex
	var calls [][]string
	restore := runEngineCmd
	t.Cleanup(func() { runEngineCmd = restore })
	runEngineCmd = func(cmd *exec.Cmd) ([]byte, error) {
		mu.Lock()
		calls = append(calls, cmd.Args)
		mu.Unlock()
		return answer(cmd.Args)
	}
	return &calls
}

func newTestLocal(t *testing.T, engines ...*Engine) *LocalDocker {
	t.Helper()
	o := NewLocalDocker(filepath.Join(t.TempDir(), "engines.json"), engines)
	o.state.detect = nil // never probe the real CLIs from a unit test
	return o
}

func TestLocalListUnionsEnginesAndFlagsPartialReads(t *testing.T) {
	o := newTestLocal(t, NewDockerEngine(), NewPodmanEngine())
	fakeEngineCmds(t, func(args []string) ([]byte, error) {
		if args[0] == "podman" {
			return nil, errors.New("podman socket down")
		}
		return []byte("ownship-a\nownship-b\n"), nil
	})
	inv, err := o.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if inv.Complete {
		t.Fatal("a failed engine must make the inventory incomplete")
	}
	if !slices.Equal(inv.IDs, []string{"a", "b"}) {
		t.Fatalf("ids %v", inv.IDs)
	}
}

func TestLocalListCompleteWhenAllEnginesAnswer(t *testing.T) {
	o := newTestLocal(t, NewDockerEngine(), NewPodmanEngine())
	fakeEngineCmds(t, func(args []string) ([]byte, error) {
		if args[0] == "podman" {
			return []byte("ownship-c\nownship-c-auth\n"), nil
		}
		return []byte("ownship-a\nosc-ownship-x-web-1\n"), nil
	})
	inv, _ := o.List(context.Background())
	if !inv.Complete || !slices.Equal(inv.IDs, []string{"a", "c"}) {
		t.Fatalf("got %+v", inv)
	}
}

func TestLocalRunRefusesUndetectedEngine(t *testing.T) {
	o := newTestLocal(t, NewDockerEngine())
	_, err := o.Run(context.Background(), RunSpec{WorkloadID: "w", Engine: KindPodman, LogLine: func(agentv1.LogStream, string) {}})
	if err == nil || !strings.Contains(err.Error(), "podman is not available on this host") {
		t.Fatalf("err %v", err)
	}
}

func TestLocalStopUsesTheWorkloadsEngine(t *testing.T) {
	o := newTestLocal(t, NewDockerEngine(), NewPodmanEngine())
	_ = o.state.Set("w", KindPodman)
	calls := fakeEngineCmds(t, func([]string) ([]byte, error) { return nil, nil })
	if err := o.Stop(context.Background(), "w"); err != nil {
		t.Fatal(err)
	}
	for _, c := range *calls {
		if c[0] != "podman" {
			t.Fatalf("a podman workload must never reach %v", c)
		}
	}
	if !slices.ContainsFunc(*calls, func(a []string) bool { return slices.Equal(a, []string{"podman", "stop", "ownship-w"}) }) {
		t.Fatalf("calls %v", *calls)
	}
	if !slices.Equal(o.state.Asleep(), []string{"w"}) {
		t.Fatal("stop must record the workload as asleep")
	}
}

func TestLocalExecArgsUseTheWorkloadsEngine(t *testing.T) {
	o := newTestLocal(t, NewDockerEngine(), NewPodmanEngine())
	_ = o.state.Set("w", KindPodman)
	args, _ := o.ExecArgs(context.Background(), "w", []string{"sh"}, true)
	if !slices.Equal(args, []string{"podman", "exec", "-i", "-t", "ownship-w", "sh"}) {
		t.Fatalf("args %v", args)
	}
}

func TestLocalDestroyForgetsTheWorkload(t *testing.T) {
	o := newTestLocal(t, NewDockerEngine(), NewPodmanEngine())
	_ = o.state.Set("w", KindPodman)
	fakeEngineCmds(t, func([]string) ([]byte, error) { return nil, nil })
	if err := o.Destroy(context.Background(), "w", true); err != nil {
		t.Fatal(err)
	}
	if _, ok := o.state.Recorded("w"); ok {
		t.Fatal("destroy must drop the engine record")
	}
}

func TestLocalDiskUsageSumsEveryEngine(t *testing.T) {
	o := newTestLocal(t, NewDockerEngine(), NewPodmanEngine())
	fakeEngineCmds(t, func(args []string) ([]byte, error) {
		if slices.Contains(args, "-v") {
			return nil, errors.New("no verbose")
		}
		return []byte(`{"Type":"Images","Size":"1GB","Reclaimable":"0B (0%)"}` + "\n"), nil
	})
	r, err := o.DiskUsage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.GetImagesBytes() != 2*parseBytes("1GB") {
		t.Fatalf("images %d", r.GetImagesBytes())
	}
}

func TestLocalPruneRunsEachEnginesSteps(t *testing.T) {
	o := newTestLocal(t, NewDockerEngine(), NewPodmanEngine())
	calls := fakeEngineCmds(t, func([]string) ([]byte, error) { return nil, nil })
	if err := o.Prune(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"docker", "image", "prune", "-af"}, {"docker", "builder", "prune", "-af"},
		{"podman", "image", "prune", "-af"},
	}
	if !slices.EqualFunc(*calls, want, slices.Equal[[]string]) {
		t.Fatalf("calls %v", *calls)
	}
}

// An engine not up yet when the agent started (docker.service still starting) must not
// make its workloads read as deleted: the inventory is incomplete until it answers.
func TestLocalListIsIncompleteWhileARecordedEngineIsMissing(t *testing.T) {
	o := newTestLocal(t, NewDockerEngine())
	o.state.detect = func(EngineKind) *Engine { return nil }
	_ = o.state.Set("p", KindPodman)
	fakeEngineCmds(t, func([]string) ([]byte, error) { return []byte("ownship-a\n"), nil })
	inv, err := o.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if inv.Complete {
		t.Fatal("podman is missing for a recorded workload; the inventory can't be complete")
	}
}

func TestLocalListIsIncompleteWithNoEngineAtAll(t *testing.T) {
	o := newTestLocal(t)
	o.state.detect = func(EngineKind) *Engine { return nil }
	inv, _ := o.List(context.Background())
	if inv.Complete {
		t.Fatal("no engine answered, so nothing can be read as gone")
	}
}

// An engine that comes up after the agent is picked up without a restart.
func TestAnEngineThatStartsLaterIsDetectedLazily(t *testing.T) {
	o := newTestLocal(t, NewDockerEngine())
	up := false
	o.state.detect = func(k EngineKind) *Engine {
		if up && k == KindPodman {
			return NewPodmanEngine()
		}
		return nil
	}
	if _, ok := o.state.Engine(KindPodman); ok {
		t.Fatal("podman is not up yet")
	}
	up = true
	if _, ok := o.state.Engine(KindPodman); !ok {
		t.Fatal("podman came up and must be used")
	}
	if len(o.state.Engines()) != 2 {
		t.Fatalf("engines %d", len(o.state.Engines()))
	}
}
