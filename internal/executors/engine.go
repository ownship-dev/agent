package executors

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"syscall"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

type EngineKind = agentv1.ContainerEngine

const (
	KindDocker   = agentv1.ContainerEngine_CONTAINER_ENGINE_DOCKER
	KindPodman   = agentv1.ContainerEngine_CONTAINER_ENGINE_PODMAN
	KindRootless = agentv1.ContainerEngine_CONTAINER_ENGINE_PODMAN_ROOTLESS
)

// RootlessUser owns rootless Podman workloads; the agent stays root and switches per call.
const RootlessUser = "ownship-podman"

// Engine is the only place that names a container CLI.
type Engine struct {
	Kind     EngineKind
	Bin      string
	Rootless bool
	prefix   []string
	// Shell-quote every word after prefix (an SSH prefix, whose words a remote shell re-parses).
	quote      bool
	cred       *syscall.Credential
	home       string
	runtimeDir string
	// When set, every command fails with this reason.
	unavailable string
}

func failArgv(reason string) []string {
	return []string{"sh", "-c", `echo "$0" >&2; exit 1`, reason}
}

func normalizeKind(k EngineKind) EngineKind {
	if k == agentv1.ContainerEngine_CONTAINER_ENGINE_UNSPECIFIED {
		return KindDocker
	}
	return k
}

func NewDockerEngine() *Engine { return &Engine{Kind: KindDocker, Bin: "docker"} }
func NewPodmanEngine() *Engine { return &Engine{Kind: KindPodman, Bin: "podman"} }

func NewRootlessPodmanEngine(u *user.User) (*Engine, error) {
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return nil, err
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return nil, err
	}
	// Its supplementary groups (video, render) are what a GPU container gets.
	var groups []uint32
	if ids, err := u.GroupIds(); err == nil {
		for _, g := range ids {
			if n, err := strconv.ParseUint(g, 10, 32); err == nil {
				groups = append(groups, uint32(n))
			}
		}
	}
	return &Engine{
		Kind: KindRootless, Bin: "podman", Rootless: true,
		cred:       &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: groups},
		home:       u.HomeDir,
		runtimeDir: fmt.Sprintf("/run/user/%d", uid),
	}, nil
}

func (e *Engine) Name() string {
	if e.Rootless {
		return "podman-rootless"
	}
	return e.Bin
}

// InGuest returns a copy that runs inside a Proxmox guest through its exec prefix.
func (e *Engine) InGuest(p GuestPrefix) *Engine {
	c := *e
	c.prefix = append([]string{}, p.Argv...)
	c.quote = p.Quote
	return &c
}

// placed puts words after the prefix, quoted when the prefix needs it.
func (e *Engine) placed(words ...string) []string {
	return GuestPrefix{Argv: e.prefix, Quote: e.quote}.Wrap(words...)
}

func (e *Engine) Argv(args ...string) []string {
	if e.unavailable != "" {
		return failArgv(e.unavailable)
	}
	return e.placed(append([]string{e.Bin}, args...)...)
}

// ExecArgv is for callers that run argv themselves as root, so rootless includes the user switch.
func (e *Engine) ExecArgv(args ...string) []string {
	if !e.Rootless || e.unavailable != "" {
		return e.Argv(args...)
	}
	return e.placed(append([]string{"runuser", "-u", RootlessUser, "--",
		"env", "HOME=" + e.home, "XDG_RUNTIME_DIR=" + e.runtimeDir, e.Bin}, args...)...)
}

func (e *Engine) env(extra ...string) []string {
	env := os.Environ()
	if e.Rootless {
		env = append(env, "HOME="+e.home, "XDG_RUNTIME_DIR="+e.runtimeDir)
	}
	return append(env, extra...)
}

func (e *Engine) command(ctx context.Context, argv []string, extraEnv ...string) *exec.Cmd {
	if e.unavailable != "" {
		argv = failArgv(e.unavailable)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = e.env(extraEnv...)
	if e.cred != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: e.cred}
	}
	return cmd
}

func (e *Engine) Cmd(ctx context.Context, args ...string) *exec.Cmd {
	return e.command(ctx, e.Argv(args...))
}

// Raw runs another program with this engine's user and placement.
func (e *Engine) Raw(ctx context.Context, argv ...string) *exec.Cmd {
	return e.command(ctx, e.placed(argv...))
}

func (e *Engine) SocketPath() string {
	switch {
	case e.Kind == KindDocker:
		return ""
	case e.Rootless:
		return e.runtimeDir + "/podman/podman.sock"
	default:
		return "/run/podman/podman.sock"
	}
}

// ComposeCmd: Docker uses its plugin; Podman runs standalone docker-compose against its
// Docker-compatible socket.
func (e *Engine) ComposeCmd(ctx context.Context, args ...string) *exec.Cmd {
	s := e.SocketPath()
	if s == "" {
		return e.command(ctx, e.placed(append([]string{"docker", "compose"}, args...)...))
	}
	if len(e.prefix) > 0 {
		// Inside a guest the variable must cross the exec prefix, so it goes on argv.
		return e.command(ctx, e.placed(append([]string{"env", "DOCKER_HOST=unix://" + s, composeBinary}, args...)...))
	}
	return e.command(ctx, append([]string{composeBinary}, args...), "DOCKER_HOST=unix://"+s)
}

const composeBinary = "docker-compose"

// HasNvidia: Docker needs the nvidia runtime, Podman a CDI spec.
func (e *Engine) HasNvidia(ctx context.Context) bool {
	if e.Kind == KindDocker {
		out, err := e.Cmd(ctx, "info", "--format", "{{json .Runtimes}}").Output()
		return err == nil && strings.Contains(string(out), "nvidia")
	}
	return e.Raw(ctx, "sh", "-c", "test -e /etc/cdi/nvidia.yaml || test -e /var/run/cdi/nvidia.yaml").Run() == nil
}

func (e *Engine) nvidiaHint() string {
	if e.Kind == KindDocker {
		return nvidiaToolkitHint
	}
	return nvidiaCDIHint
}

// GPUArgs hands GPUs to a container: NVIDIA via the runtime or CDI, other nodes by passthrough.
func (e *Engine) GPUArgs(g gpuDevices, nvidia bool) ([]string, []string, error) {
	var args, notes []string
	if len(g.Nvidia) > 0 {
		switch {
		case !nvidia:
			notes = append(notes, e.nvidiaHint())
		case e.Kind == KindDocker:
			args = append(args, "--gpus", "all")
		default:
			args = append(args, "--device", "nvidia.com/gpu=all")
		}
	}
	if len(g.Dri) > 0 {
		args = append(args, "--device", "/dev/dri")
	}
	if g.Kfd {
		args = append(args, "--device", "/dev/kfd")
	}
	if len(args) == 0 {
		if len(notes) > 0 {
			return nil, notes, errors.New(notes[0])
		}
		return nil, nil, errors.New("this project asks for a GPU, but none was found on the host")
	}
	switch {
	case e.Rootless:
		// Rootless can't add host groups; keep-groups carries the user's own.
		args = append(args, "--group-add", "keep-groups")
	case g.Kfd:
		args = append(args, "--group-add", "video")
	}
	return args, notes, nil
}

const nvidiaCDIHint = "NVIDIA GPU found but Podman has no CDI spec: run nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml"

// RestartPolicy: older podman-restart units only start `always` containers, so Podman uses
// always and slept apps are re-stopped by Resume.
func (e *Engine) RestartPolicy() string {
	if e.Kind == KindDocker {
		return "unless-stopped"
	}
	return "always"
}

func (e *Engine) PruneArgs() [][]string {
	if e.Kind == KindDocker {
		return [][]string{{"image", "prune", "-af"}, {"builder", "prune", "-af"}}
	}
	return [][]string{{"image", "prune", "-af"}}
}

// DetectEngine returns nil when the engine isn't answering.
func DetectEngine(ctx context.Context, k EngineKind) *Engine {
	var e *Engine
	switch k {
	case KindDocker:
		e = NewDockerEngine()
	case KindPodman:
		e = NewPodmanEngine()
	case KindRootless:
		u, err := user.Lookup(RootlessUser)
		if err != nil {
			return nil
		}
		if e, err = NewRootlessPodmanEngine(u); err != nil {
			return nil
		}
	default:
		return nil
	}
	if e.Cmd(ctx, "version", "--format", "{{.Client.Version}}").Run() != nil {
		return nil
	}
	return e
}

// DetectEngines probes rootless only if the installer created its user.
func DetectEngines(ctx context.Context) ([]*Engine, []*agentv1.EngineInfo) {
	var engines []*Engine
	var infos []*agentv1.EngineInfo
	try := func(e *Engine) {
		out, err := e.Cmd(ctx, "version", "--format", "{{.Client.Version}}").Output()
		if err != nil {
			return
		}
		engines = append(engines, e)
		infos = append(infos, &agentv1.EngineInfo{
			Kind:    e.Kind,
			Version: strings.TrimSpace(string(out)),
			Compose: e.ComposeCmd(ctx, "version").Run() == nil,
			GpuCdi:  e.Kind != KindDocker && e.HasNvidia(ctx),
		})
	}
	try(NewDockerEngine())
	try(NewPodmanEngine())
	if u, err := user.Lookup(RootlessUser); err == nil {
		if e, err := NewRootlessPodmanEngine(u); err == nil {
			try(e)
		}
	}
	return engines, infos
}
