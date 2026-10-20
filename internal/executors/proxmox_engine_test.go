package executors

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

func newTestProxmox(t *testing.T) *ProxmoxLXC {
	t.Helper()
	dir := t.TempDir()
	state, err := loadLxcState(filepath.Join(dir, "lxc.json"))
	if err != nil {
		t.Fatal(err)
	}
	o := &ProxmoxLXC{state: state, createLocks: newKeyedLocks()}
	o.engines = loadEngineState(filepath.Join(dir, "engines.json"), proxmoxEngines(), nil)
	return o
}

func TestProxmoxRefusesRootlessBeforeTouchingTheLXC(t *testing.T) {
	o := newTestProxmox(t)
	_, err := o.Run(context.Background(), RunSpec{WorkloadID: "w", Engine: KindRootless, LogLine: func(agentv1.LogStream, string) {}})
	if err == nil || !strings.Contains(err.Error(), "not offered on Proxmox") {
		t.Fatalf("err %v", err)
	}
}

func TestProxmoxExecUsesTheWorkloadsEngine(t *testing.T) {
	o := newTestProxmox(t)
	_ = o.state.set("w", "105")
	_ = o.engines.Set("w", KindPodman)
	args, err := o.ExecArgs(context.Background(), "w", []string{"sh"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(args, []string{"pct", "exec", "105", "--", "podman", "exec", "-i", "-t", "app", "sh"}) {
		t.Fatalf("args %v", args)
	}
	cmd, _ := o.AppExecCmd(context.Background(), "w", "true")
	if !slices.Equal(cmd.Args[:5], []string{"pct", "exec", "105", "--", "podman"}) {
		t.Fatalf("app exec %v", cmd.Args)
	}
}

func TestLxcScriptsFollowTheEngine(t *testing.T) {
	for _, script := range []string{lxcRunPrepScript(NewPodmanEngine()), lxcStatsScript(NewPodmanEngine())} {
		if strings.Contains(script, "docker ") || !strings.Contains(script, "podman ") {
			t.Errorf("podman script still names docker:\n%s", script)
		}
	}
	if !strings.Contains(lxcRunPrepScript(NewDockerEngine()), "docker rm -f app") {
		t.Error("docker prep script changed")
	}
}

func TestLxcBuildRunsTheHostStepsInsideTheContainer(t *testing.T) {
	for _, step := range buildSteps(NewPodmanEngine(), "img", false) {
		got := strings.Join(lxcStepCmd(context.Background(), "105", "/root/app", step).Args, " ")
		want := `pct exec 105 -- sh -c cd "$1" && shift && exec "$@" sh /root/app ` + strings.Join(step, " ")
		if got != want {
			t.Errorf("argv %q, want %q", got, want)
		}
	}
}

func TestEngineInstallScripts(t *testing.T) {
	if !strings.Contains(engineInstallScript(KindPodman), "podman") {
		t.Error("podman install")
	}
	c := composeInstallScript(NewPodmanEngine())
	if !strings.Contains(c, "/usr/local/bin/docker-compose") || !strings.Contains(c, composeVersion) {
		t.Errorf("podman compose install %q", c)
	}
	if !strings.Contains(composeInstallScript(NewDockerEngine()), "docker-compose-plugin") {
		t.Error("docker keeps its plugin package")
	}
}

// An engine this agent doesn't know (a newer control plane) must fail, never fall back to Docker.
func TestProxmoxRefusesUnknownEngines(t *testing.T) {
	o := newTestProxmox(t)
	_, err := o.Run(context.Background(), RunSpec{WorkloadID: "w", Engine: 99, LogLine: func(agentv1.LogStream, string) {}})
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("err %v", err)
	}
}
