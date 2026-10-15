package executors

import (
	"context"
	"errors"
	"os/exec"
	"slices"
	"strings"
	"testing"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

func noLog(agentv1.LogStream, string) {}

type fakeCopier struct {
	failOn string
	copied []string
}

func (f *fakeCopier) TarCmd(ctx context.Context, _ *Engine, volume string) *exec.Cmd {
	if f.failOn == volume {
		return exec.CommandContext(ctx, "sh", "-c", "exit 3")
	}
	return exec.CommandContext(ctx, "printf", "data-"+volume)
}

func (f *fakeCopier) UntarCmd(ctx context.Context, _ *Engine, volume string) *exec.Cmd {
	f.copied = append(f.copied, volume)
	return exec.CommandContext(ctx, "cat")
}

func TestMigrateVolumesCopiesEach(t *testing.T) {
	c := &fakeCopier{}
	err := migrateVolumes(context.Background(), c, NewDockerEngine(), NewPodmanEngine(), []string{"v-data", "v-cache"}, noLog)
	if err != nil || !slices.Equal(c.copied, []string{"v-data", "v-cache"}) {
		t.Fatalf("err=%v copied=%v", err, c.copied)
	}
}

func TestMigrateVolumesStopsOnFailureAndNamesTheVolume(t *testing.T) {
	c := &fakeCopier{failOn: "v-cache"}
	err := migrateVolumes(context.Background(), c, NewDockerEngine(), NewPodmanEngine(), []string{"v-data", "v-cache", "v-later"}, noLog)
	if err == nil || !strings.Contains(err.Error(), `volume "v-cache"`) {
		t.Fatalf("err=%v", err)
	}
	if slices.Contains(c.copied, "v-later") {
		t.Fatal("must stop at the first failure")
	}
}

// recorder logs each switch step so the tests can assert order and what never ran.
type recorder struct {
	steps []string
	fail  string
}

func (r *recorder) op(name string) error {
	r.steps = append(r.steps, name)
	if r.fail == name {
		return errors.New(name + " broke")
	}
	return nil
}

func (r *recorder) ops() switchOps {
	return switchOps{
		ensureImage: func() error { return r.op("image") },
		stopOld:     func() error { return r.op("stop-old") },
		copyVolumes: func() error { return r.op("copy") },
		runNew: func() (Target, error) {
			return Target{URL: "http://new"}, r.op("run-new")
		},
		removeNew: func() { _ = r.op("remove-new") },
		startOld:  func() { _ = r.op("start-old") },
	}
}

func TestSwitchHappyPathKeepsOldUntilFinalize(t *testing.T) {
	r := &recorder{}
	tgt, err := runSwitch(r.ops())
	if err != nil || tgt.URL != "http://new" {
		t.Fatalf("tgt=%v err=%v", tgt, err)
	}
	if !slices.Equal(r.steps, []string{"image", "stop-old", "copy", "run-new"}) {
		t.Fatalf("steps %v", r.steps)
	}
}

func TestSwitchFailureRestoresTheOldWorkload(t *testing.T) {
	cases := map[string][]string{
		"image":    {"image"},
		"stop-old": {"image", "stop-old", "start-old"},
		"copy":     {"image", "stop-old", "copy", "start-old"},
		"run-new":  {"image", "stop-old", "copy", "run-new", "remove-new", "start-old"},
	}
	for fail, want := range cases {
		r := &recorder{fail: fail}
		if _, err := runSwitch(r.ops()); err == nil || !strings.Contains(err.Error(), fail) {
			t.Errorf("%s: err %v", fail, err)
		}
		if !slices.Equal(r.steps, want) {
			t.Errorf("%s: steps %v want %v", fail, r.steps, want)
		}
	}
}

func TestVolumeCopyCommands(t *testing.T) {
	c := engineVolumeCopier{}
	tar := c.TarCmd(context.Background(), NewDockerEngine(), "ownship-w-data")
	if !slices.Equal(tar.Args, []string{"docker", "run", "--rm", "-i", "-v", "ownship-w-data:/data:ro", "--", "busybox", "tar", "czf", "-", "-C", "/data", "."}) {
		t.Fatalf("tar %v", tar.Args)
	}
	untar := c.UntarCmd(context.Background(), NewPodmanEngine(), "ownship-w-data")
	// Wiped first, so a retried switch never merges into a half-copied volume.
	if !slices.Equal(untar.Args, []string{"podman", "run", "--rm", "-i", "-v", "ownship-w-data:/data", "--", "busybox",
		"sh", "-c", "find /data -mindepth 1 -delete && tar xzf - -C /data"}) {
		t.Fatalf("untar %v", untar.Args)
	}
}

func TestLocalFinalizeSwitchRemovesTheOldEngineCopy(t *testing.T) {
	o := newTestLocal(t, NewDockerEngine(), NewPodmanEngine())
	_ = o.state.Set("w", KindPodman)
	_ = o.state.SetPendingOld("w", KindDocker)
	calls := fakeEngineCmds(t, func(args []string) ([]byte, error) {
		if slices.Equal(args[:3], []string{"docker", "volume", "ls"}) {
			return []byte("ownship-w-data\n"), nil
		}
		return nil, nil
	})
	if err := o.FinalizeSwitch(context.Background(), "w"); err != nil {
		t.Fatal(err)
	}
	for _, want := range [][]string{{"docker", "rm", "-f", "ownship-w"}, {"docker", "volume", "rm", "ownship-w-data"}} {
		if !slices.ContainsFunc(*calls, func(a []string) bool { return slices.Equal(a, want) }) {
			t.Errorf("missing %v in %v", want, *calls)
		}
	}
	for _, c := range *calls {
		if c[0] == "podman" {
			t.Fatalf("the new engine must not be touched: %v", c)
		}
	}
	if _, ok := o.state.PendingOld("w"); ok {
		t.Fatal("pending old must be cleared")
	}
}

func TestLocalFinalizeSwitchIsANoOpWithoutPending(t *testing.T) {
	o := newTestLocal(t, NewDockerEngine())
	calls := fakeEngineCmds(t, func([]string) ([]byte, error) { return nil, nil })
	if err := o.FinalizeSwitch(context.Background(), "w"); err != nil || len(*calls) != 0 {
		t.Fatalf("err=%v calls=%v", err, *calls)
	}
}

type finalizingOrch struct {
	Orchestrator
	finalized []string
	aborted   []string
}

func (f *finalizingOrch) FinalizeSwitch(_ context.Context, id string) error {
	f.finalized = append(f.finalized, id)
	return nil
}

func (f *finalizingOrch) AbortSwitch(_ context.Context, id string) error {
	f.aborted = append(f.aborted, id)
	return nil
}

// The old engine's copy goes only after the new one passed the health gate.
func TestDeployerFinalizesAnEngineSwitchAfterHealth(t *testing.T) {
	f := &finalizingOrch{}
	d := &Deployer{orch: f}
	d.finalizeEngineSwitch(context.Background(), "w", noLog)
	if !slices.Equal(f.finalized, []string{"w"}) {
		t.Fatalf("finalized %v", f.finalized)
	}
	(&Deployer{orch: &finalizingOrch{}}).finalizeEngineSwitch(context.Background(), "x", noLog)
}

// A switch that fails after Run (pre-deploy hook, health gate) goes back to the old engine:
// the new copy is removed, the old container restarted and recorded as current again.
func TestLocalAbortSwitchRestoresTheOldEngine(t *testing.T) {
	o := newTestLocal(t, NewDockerEngine(), NewPodmanEngine())
	_ = o.state.Set("w", KindPodman)
	_ = o.state.SetPendingOld("w", KindDocker)
	calls := fakeEngineCmds(t, func([]string) ([]byte, error) { return nil, nil })
	if err := o.AbortSwitch(context.Background(), "w"); err != nil {
		t.Fatal(err)
	}
	has := func(want ...string) bool {
		return slices.ContainsFunc(*calls, func(a []string) bool { return slices.Equal(a, want) })
	}
	if !has("podman", "rm", "-f", "ownship-w") || !has("docker", "start", "ownship-w") {
		t.Fatalf("calls %v", *calls)
	}
	if k, _ := o.state.Recorded("w"); k != KindDocker {
		t.Fatalf("recorded %v, want docker", k)
	}
	if _, ok := o.state.PendingOld("w"); ok {
		t.Fatal("pending old must be cleared")
	}
}

func TestLocalAbortSwitchIsANoOpWithoutPending(t *testing.T) {
	o := newTestLocal(t, NewDockerEngine())
	calls := fakeEngineCmds(t, func([]string) ([]byte, error) { return nil, nil })
	if err := o.AbortSwitch(context.Background(), "w"); err != nil || len(*calls) != 0 {
		t.Fatalf("err=%v calls=%v", err, *calls)
	}
}

func TestDeployerAbortsAnEngineSwitchOnALateFailure(t *testing.T) {
	f := &finalizingOrch{}
	(&Deployer{orch: f}).abortEngineSwitch(context.Background(), "w", noLog)
	if !slices.Equal(f.aborted, []string{"w"}) {
		t.Fatalf("aborted %v", f.aborted)
	}
}

// A missing workload (container deleted outside Ownship) has nothing to stop: the switch
// must go ahead rather than fail forever at its first step.
func TestSwitchStopToleratesAMissingOldContainer(t *testing.T) {
	o := newTestLocal(t, NewDockerEngine(), NewPodmanEngine())
	fakeEngineCmds(t, func(args []string) ([]byte, error) {
		if slices.Equal(args, []string{"docker", "stop", "ownship-w"}) {
			return nil, errors.New("Error response from daemon: No such container: ownship-w")
		}
		return nil, nil
	})
	if err := o.stopForSwitch(context.Background(), NewDockerEngine(), "w"); err != nil {
		t.Fatalf("a missing container must not block the switch: %v", err)
	}
	fakeEngineCmds(t, func(args []string) ([]byte, error) {
		if slices.Equal(args, []string{"docker", "stop", "ownship-w"}) {
			return nil, errors.New("Cannot connect to the Docker daemon")
		}
		return nil, nil
	})
	if err := o.stopForSwitch(context.Background(), NewDockerEngine(), "w"); err == nil {
		t.Fatal("any other failure still stops the switch")
	}
}
