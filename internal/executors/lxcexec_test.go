package executors

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func proxmoxWithLXC(t *testing.T, workload, vmid string) *ProxmoxLXC {
	t.Helper()
	state, err := loadLxcState(filepath.Join(t.TempDir(), "lxc.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := state.set(workload, vmid); err != nil {
		t.Fatal(err)
	}
	return &ProxmoxLXC{cfg: ProxmoxConfig{}, state: state, createLocks: newKeyedLocks()}
}

// The two targets must land on different layers: app descends into the container,
// machine stops at the LXC. Regressing this silently gives the wrong shell.
func TestProxmoxExecArgsPerTarget(t *testing.T) {
	o := proxmoxWithLXC(t, "proj-1", "142")
	ctx := context.Background()

	app, err := o.ExecArgs(ctx, "proj-1", []string{"sh"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(app, " "), "docker exec -i -t "+lxcAppContainer) {
		t.Fatalf("app shell should enter the container, got %v", app)
	}

	machine, err := o.LxcExecArgs(ctx, "proj-1", []string{"sh"}, true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"pct", "exec", "142", "--", "sh"}
	if strings.Join(machine, " ") != strings.Join(want, " ") {
		t.Fatalf("LXC shell should stop at the LXC, got %v", machine)
	}
}

func TestProxmoxLxcExecArgsUnknownWorkload(t *testing.T) {
	o := proxmoxWithLXC(t, "proj-1", "142")
	if _, err := o.LxcExecArgs(context.Background(), "other", []string{"sh"}, true); err == nil {
		t.Fatal("a workload with no LXC must not resolve to some other machine")
	}
}

// Plain Docker has no LXC layer; it must refuse rather than fall back to the
// agent host, which is not part of the workload the caller is authorized for.
func TestLocalDockerHasNoLxcShell(t *testing.T) {
	o := &LocalDocker{}
	if _, err := o.LxcExecArgs(context.Background(), "proj-1", []string{"sh"}, true); err == nil {
		t.Fatal("LocalDocker must refuse an LXC shell")
	}
}
