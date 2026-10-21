package executors

import (
	"context"
	"os/user"
	"slices"
	"syscall"
	"testing"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

func rootlessForTest() *Engine {
	return &Engine{Kind: KindRootless, Bin: "podman", Rootless: true,
		cred: &syscall.Credential{Uid: 990, Gid: 990}, home: "/var/lib/ownship-podman", runtimeDir: "/run/user/990"}
}

func TestEngineArgv(t *testing.T) {
	cases := []struct {
		name string
		e    *Engine
		want []string
	}{
		{"docker", NewDockerEngine(), []string{"docker", "ps"}},
		{"podman", NewPodmanEngine(), []string{"podman", "ps"}},
		{"docker in lxc", NewDockerEngine().InGuest(lxcPrefix("105")), []string{"pct", "exec", "105", "--", "docker", "ps"}},
		{"podman in lxc", NewPodmanEngine().InGuest(lxcPrefix("105")), []string{"pct", "exec", "105", "--", "podman", "ps"}},
	}
	for _, c := range cases {
		if got := c.e.Argv("ps"); !slices.Equal(got, c.want) {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestInLXCDoesNotMutateTheHostEngine(t *testing.T) {
	host := NewPodmanEngine()
	_ = host.InGuest(lxcPrefix("105"))
	if got := host.Argv("ps"); !slices.Equal(got, []string{"podman", "ps"}) {
		t.Fatalf("host engine changed: %v", got)
	}
}

func TestRootlessCmdRunsAsUser(t *testing.T) {
	cmd := rootlessForTest().Cmd(context.Background(), "ps")
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.Credential == nil || cmd.SysProcAttr.Credential.Uid != 990 {
		t.Fatalf("expected credential uid 990, got %+v", cmd.SysProcAttr)
	}
	if !slices.Contains(cmd.Env, "XDG_RUNTIME_DIR=/run/user/990") || !slices.Contains(cmd.Env, "HOME=/var/lib/ownship-podman") {
		t.Fatalf("env missing rootless vars: %v", cmd.Env)
	}
}

func TestRootfulCmdKeepsAgentIdentity(t *testing.T) {
	cmd := NewPodmanEngine().Cmd(context.Background(), "ps")
	if cmd.SysProcAttr != nil && cmd.SysProcAttr.Credential != nil {
		t.Fatal("rootful podman must run as the agent")
	}
}

// Podman hosts have no docker CLI to host the compose plugin, so they run the standalone
// docker-compose v2 binary against the Podman socket.
func TestComposeCmdPerEngine(t *testing.T) {
	ctx := context.Background()
	d := NewDockerEngine().ComposeCmd(ctx, "version")
	if !slices.Equal(d.Args, []string{"docker", "compose", "version"}) || slices.ContainsFunc(d.Env, isDockerHost) {
		t.Fatalf("docker compose: %v env-has-DOCKER_HOST=%v", d.Args, slices.ContainsFunc(d.Env, isDockerHost))
	}
	p := NewPodmanEngine().ComposeCmd(ctx, "version")
	if !slices.Equal(p.Args, []string{"docker-compose", "version"}) || !slices.Contains(p.Env, "DOCKER_HOST=unix:///run/podman/podman.sock") {
		t.Fatalf("podman compose: %v (DOCKER_HOST set: %v)", p.Args, slices.ContainsFunc(p.Env, isDockerHost))
	}
	r := rootlessForTest().ComposeCmd(ctx, "version")
	if !slices.Contains(r.Env, "DOCKER_HOST=unix:///run/user/990/podman/podman.sock") || r.SysProcAttr == nil {
		t.Fatalf("rootless compose: %v", r.Args)
	}
	l := NewPodmanEngine().InGuest(lxcPrefix("105")).ComposeCmd(ctx, "version")
	want := []string{"pct", "exec", "105", "--", "env", "DOCKER_HOST=unix:///run/podman/podman.sock", "docker-compose", "version"}
	if !slices.Equal(l.Args, want) {
		t.Fatalf("podman compose in lxc: %v", l.Args)
	}
}

func isDockerHost(s string) bool { return len(s) > 12 && s[:12] == "DOCKER_HOST=" }

func TestGPUArgsPerEngine(t *testing.T) {
	g := gpuDevices{Nvidia: []string{"/dev/nvidia0"}, Dri: []string{"/dev/dri/renderD128"}, Kfd: true}
	d, _, _ := NewDockerEngine().GPUArgs(g, true)
	if !slices.Equal(d, []string{"--gpus", "all", "--device", "/dev/dri", "--device", "/dev/kfd", "--group-add", "video"}) {
		t.Errorf("docker %v", d)
	}
	p, _, _ := NewPodmanEngine().GPUArgs(g, true)
	if !slices.Equal(p, []string{"--device", "nvidia.com/gpu=all", "--device", "/dev/dri", "--device", "/dev/kfd", "--group-add", "video"}) {
		t.Errorf("podman %v", p)
	}
	r, _, _ := rootlessForTest().GPUArgs(g, true)
	if !slices.Equal(r, []string{"--device", "nvidia.com/gpu=all", "--device", "/dev/dri", "--device", "/dev/kfd", "--group-add", "keep-groups"}) {
		t.Errorf("rootless %v", r)
	}
}

func TestGPUArgsHintsPerEngine(t *testing.T) {
	nv := gpuDevices{Nvidia: []string{"/dev/nvidia0"}}
	if _, notes, err := NewDockerEngine().GPUArgs(nv, false); err == nil || len(notes) != 1 || notes[0] != nvidiaToolkitHint {
		t.Errorf("docker without runtime: notes=%v err=%v", notes, err)
	}
	if _, notes, err := NewPodmanEngine().GPUArgs(nv, false); err == nil || len(notes) != 1 || notes[0] != nvidiaCDIHint {
		t.Errorf("podman without CDI: notes=%v err=%v", notes, err)
	}
	if _, _, err := NewPodmanEngine().GPUArgs(gpuDevices{}, true); err == nil {
		t.Error("no GPU must be an error")
	}
}

func TestPruneArgs(t *testing.T) {
	if len(NewDockerEngine().PruneArgs()) != 2 || len(NewPodmanEngine().PruneArgs()) != 1 {
		t.Fatal("docker prunes images+builder, podman images only")
	}
}

func TestNormalizeKind(t *testing.T) {
	if normalizeKind(agentv1.ContainerEngine_CONTAINER_ENGINE_UNSPECIFIED) != KindDocker {
		t.Fatal("unspecified must mean docker")
	}
	if normalizeKind(KindRootless) != KindRootless {
		t.Fatal("known kinds pass through")
	}
}

func TestEngineNames(t *testing.T) {
	if NewDockerEngine().Name() != "docker" || NewPodmanEngine().Name() != "podman" || rootlessForTest().Name() != "podman-rootless" {
		t.Fatal("names")
	}
}

// Terminals run the argv themselves (as root), so rootless needs its user switch in the argv.
func TestExecArgvPerEngine(t *testing.T) {
	if got := NewPodmanEngine().ExecArgv("exec", "-i", "c", "sh"); !slices.Equal(got, []string{"podman", "exec", "-i", "c", "sh"}) {
		t.Errorf("rootful %v", got)
	}
	want := []string{"runuser", "-u", RootlessUser, "--", "env", "HOME=/var/lib/ownship-podman", "XDG_RUNTIME_DIR=/run/user/990",
		"podman", "exec", "-i", "c", "sh"}
	if got := rootlessForTest().ExecArgv("exec", "-i", "c", "sh"); !slices.Equal(got, want) {
		t.Errorf("rootless %v", got)
	}
}

// Without its supplementary groups the rootless user loses video/render, and keep-groups
// would hand the container nothing.
func TestRootlessEngineKeepsSupplementaryGroups(t *testing.T) {
	u, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	e, err := NewRootlessPodmanEngine(u)
	if err != nil {
		t.Fatal(err)
	}
	gids, _ := u.GroupIds()
	if len(gids) > 0 && len(e.cred.Groups) == 0 {
		t.Fatal("supplementary groups dropped")
	}
}

func lxcPrefix(vmid string) GuestPrefix {
	p, _ := lxcGuest{}.ExecPrefix(context.Background(), vmid, false)
	return p
}
