package executors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ownship/agent/internal/compose"
	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

// ProxmoxConfig configures the LXC orchestrator, which runs on the Proxmox host via pct/pvesh.
type ProxmoxConfig struct {
	StatePath string
	// Engine inside each workload's LXC (docker or podman).
	EngineStatePath string
	// pct create defaults, overridable via env.
	Storage  string // rootfs storage, e.g. "local-lvm"
	Bridge   string // network bridge, e.g. "vmbr0"
	RootfsGB int    // rootfs size in GB
}

// ProxmoxLXC runs each project in its own LXC, building inside it so the host needs no Docker.
type ProxmoxLXC struct {
	cfg     ProxmoxConfig
	state   *lxcState
	engines *engineState

	// hello() calls Nodes() three times in a burst; a short TTL makes that one subprocess.
	nodesMu    sync.Mutex
	nodesCache []*agentv1.NodeInfo
	nodesAt    time.Time

	// The node name is stable while the agent runs, so it is resolved once.
	nodeNameMu sync.Mutex
	nodeName   string

	// Storage, template and bridge inventory rarely changes; InvalidateCaches forces a re-read.
	discoMu        sync.Mutex
	templatesCache []string
	templatesAt    time.Time
	bridgesCache   []string
	bridgesAt      time.Time

	// Serializes creates per workload: nextVmid() reports a free id without reserving it.
	createLocks *keyedLocks

	// Serializes nextVmid() through pct create/clone across all workloads, so two projects
	// deploying at once can't claim the same vmid.
	vmidMu sync.Mutex

	// Until the control plane's Adopt lands, an untracked ownship LXC may be a lost mapping.
	adopted atomic.Bool
}

const (
	nodesCacheTTL = 15 * time.Second
	discoCacheTTL = 5 * time.Minute
)

const lxcAppContainer = "app"

// lxcRunPrepScript cleans up before each start: old containers, the bridge, a stale Caddyfile.
func lxcRunPrepScript(e *Engine) string {
	b := e.Bin
	return b + " rm -f " + lxcAppContainer + " " + lxcAppContainer + authSidecarSuffix + " 2>/dev/null\n" +
		// Services left behind by a project that switched from compose to a single image.
		b + " ps -aq --filter label=com.docker.compose.project | xargs -r " + b + " rm -f\n" +
		b + " network ls -q --filter label=com.docker.compose.project | xargs -r " + b + " network rm 2>/dev/null\n" +
		b + " network create " + linkNetwork + " 2>/dev/null\n" +
		"rm -f " + lxcCaddyfilePath + "\n" +
		"true"
}

func NewProxmoxLXC(cfg ProxmoxConfig) (*ProxmoxLXC, error) {
	if cfg.Storage == "" {
		cfg.Storage = "local-lvm"
	}
	if cfg.Bridge == "" {
		cfg.Bridge = "vmbr0"
	}
	if cfg.RootfsGB <= 0 {
		cfg.RootfsGB = 8
	}
	state, err := loadLxcState(cfg.StatePath)
	if err != nil {
		return nil, fmt.Errorf("load lxc state: %w", err)
	}
	o := &ProxmoxLXC{cfg: cfg, state: state, createLocks: newKeyedLocks()}
	o.engines = loadEngineState(cfg.EngineStatePath, proxmoxEngines(), o.probeEngine)
	return o, nil
}

// Engines: both are installed in the LXC on demand, so both are offered.
func (o *ProxmoxLXC) Engines(context.Context) []*agentv1.EngineInfo {
	return []*agentv1.EngineInfo{{Kind: KindDocker, Compose: true}, {Kind: KindPodman, Compose: true}}
}

// Rootless is never offered on Proxmox.
func proxmoxEngines() []*Engine { return []*Engine{NewDockerEngine(), NewPodmanEngine()} }

func (o *ProxmoxLXC) probeEngine(ctx context.Context, e *Engine, workloadID string) bool {
	vmid, ok := o.state.get(workloadID)
	return ok && e.InLXC(vmid).Cmd(ctx, "container", "inspect", "--", lxcAppContainer).Run() == nil
}

func (o *ProxmoxLXC) lxcEngine(ctx context.Context, workloadID, vmid string) *Engine {
	if o.engines == nil {
		return NewDockerEngine().InLXC(vmid)
	}
	return o.engines.For(ctx, workloadID).InLXC(vmid)
}

func (o *ProxmoxLXC) HostBuilds() bool { return false }

func (o *ProxmoxLXC) Run(ctx context.Context, spec RunSpec) (Target, error) {
	kind := normalizeKind(spec.Engine)
	switch kind {
	case KindDocker, KindPodman:
	case KindRootless:
		return Target{}, errors.New("rootless Podman is not offered on Proxmox; the LXC is already unprivileged")
	default:
		// Fail closed: an unknown engine must never silently fall back to Docker.
		return Target{}, fmt.Errorf("container engine %v is not supported by this agent; update it", kind)
	}
	target, err := o.run(ctx, kind, spec)
	if err != nil {
		return Target{}, err
	}
	_ = o.engines.Set(spec.WorkloadID, kind)
	_ = o.engines.SetAsleep(spec.WorkloadID, false)
	return target, nil
}

func (o *ProxmoxLXC) run(ctx context.Context, kind EngineKind, spec RunSpec) (Target, error) {
	vmid, err := o.ensureLXC(ctx, spec.WorkloadID, spec.BaseTemplate, spec.TemplateVmid, spec.BaseOS, spec.Resources, spec.TargetNode, spec.Hostname, spec.Network, spec.LogLine)
	if err != nil {
		return Target{}, err
	}

	// Re-applied every deploy, since a reused LXC would otherwise keep the old password.
	o.applyRootPassword(ctx, vmid, spec.MachinePassword, spec.LogLine)
	// Same for GPU passthrough.
	hostGPU := detectGPUs(ctx, "/")
	if err := o.applyGPUPassthrough(ctx, vmid, spec.Resources.GetGpu(), hostGPU, spec.LogLine); err != nil {
		return Target{}, err
	}

	// Golden templates usually ship Docker; base-OS builds install it.
	eng := o.engines.byKind(kind).InLXC(vmid)
	if err := o.ensureEngine(ctx, eng, vmid, spec.LogLine); err != nil {
		return Target{}, err
	}
	// Volumes are LXC dirs both engines bind, so a switch only retires the old engine's containers.
	if rec, ok := o.engines.Recorded(spec.WorkloadID); ok && normalizeKind(rec) != kind {
		old := o.engines.byKind(rec).InLXC(vmid)
		_ = old.Raw(ctx, "sh", "-c", lxcRunPrepScript(old)).Run()
		spec.LogLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "moved from "+old.Name()+" to "+eng.Name()+" inside LXC "+vmid)
	}

	// Short-lived registry credentials for the build and the pull, removed afterwards.
	if spec.RegistryAuth != nil {
		_ = pct(ctx, "exec", vmid, "--", "mkdir", "-p", "-m", "0700", "/root/.docker").Run()
		if err := o.writeEnvFile(ctx, vmid, "/root/.docker/config.json", dockerConfigJSON(spec.RegistryAuth)); err != nil {
			return Target{}, fmt.Errorf("write registry auth: %w", err)
		}
		defer func() {
			_ = pct(context.Background(), "exec", vmid, "--", "rm", "-f", "/root/.docker/config.json").Run()
		}()
		spec.LogLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "authenticated to registry "+spec.RegistryAuth.GetHost())
	}

	if spec.Compose != nil {
		return o.runComposeInLXC(ctx, eng, vmid, spec, hostGPU)
	}

	// An image with no repo (managed DB, rollback) runs as-is.
	if spec.Repo.GetCloneUrl() != "" {
		if err := o.buildInLXC(ctx, eng, vmid, spec.Image, spec.Repo, spec.Build, spec.Cache, spec.LogLine); err != nil {
			return Target{}, err
		}
	}

	// 0600 env file inside the LXC keeps secrets off argv.
	const envPath = "/run/ownship-app.env"
	if err := o.writeEnvFile(ctx, vmid, envPath, envFileContent(spec.Env, spec.Port)); err != nil {
		return Target{}, err
	}

	// Re-pull moved tags, but keep the current image if the registry is down.
	if spec.PullImage {
		if out, err := eng.Cmd(ctx, "pull", "--", spec.Image).CombinedOutput(); err != nil {
			spec.LogLine(agentv1.LogStream_LOG_STREAM_SYSTEM,
				"image pull failed, using local copy if present: "+strings.TrimSpace(string(out)))
		}
	}

	// Force non-root unless it's a managed DB, AllowRoot is set, or the image already is non-root.
	nonRootEnforced := !spec.IsManagedService && !spec.AllowRoot
	var userArgs []string
	if nonRootEnforced {
		imageUser, err := o.imageUser(ctx, eng, spec.Image)
		if err != nil {
			// Fail safe: force non-root even if inspect failed.
			spec.LogLine(agentv1.LogStream_LOG_STREAM_SYSTEM,
				"image inspect failed, forcing non-root as a fail-safe: "+err.Error())
			imageUser = ""
		}
		userArgs = dockerUserArgs(imageUser)
	}

	if spec.Resources.GetGpu() {
		gpuArgs, notes, err := eng.GPUArgs(hostGPU, eng.HasNvidia(ctx))
		for _, n := range notes {
			spec.LogLine(agentv1.LogStream_LOG_STREAM_SYSTEM, n+" (inside the LXC, with driver libraries matching the host)")
		}
		if err != nil {
			return Target{}, err
		}
		userArgs = append(userArgs, gpuArgs...)
	}

	_ = eng.Raw(ctx, "sh", "-c", lxcRunPrepScript(eng)).Run()
	// The LXC boots on its own (onboot=1); the restart policy brings the app back.
	run := []string{"run", "-d", "--name", lxcAppContainer,
		"--restart", eng.RestartPolicy(), "--network", linkNetwork, "--env-file", envPath}
	run = append(run, userArgs...)
	if spec.BasicAuth == nil {
		run = append(run, "-p", fmt.Sprintf("%d:%d", spec.Port, spec.Port))
	}
	// 0777 under non-root enforcement so the container's UID can write.
	volMode := "0755"
	if nonRootEnforced {
		volMode = "0777"
	}
	for _, v := range spec.Volumes {
		host, err := safeVolumeHostPath(v.GetName())
		if err != nil {
			return Target{}, err
		}
		// mkdir -m only applies on creation, so older volumes need an explicit chmod.
		_ = pct(ctx, "exec", vmid, "--", "mkdir", "-p", host).Run()
		_ = pct(ctx, "exec", vmid, "--", "chmod", volMode, host).Run()
		run = append(run, "-v", host+":"+v.GetMountPath())
	}
	// -- stops an image ref starting with "-" being parsed as a flag.
	run = append(run, "--", spec.Image)
	run = append(run, spec.Args...)
	if out, err := eng.Cmd(ctx, run...).CombinedOutput(); err != nil {
		return Target{}, fmt.Errorf("%s run in lxc: %s", eng.Name(), strings.TrimSpace(string(out)))
	}

	// Behind Basic Auth the sidecar owns the port; its Caddyfile (0600) survives reboots.
	if spec.BasicAuth != nil {
		caddyfile, err := renderCaddyfile(spec.BasicAuth, fmt.Sprintf("%s:%d", lxcAppContainer, spec.Port))
		if err != nil {
			return Target{}, err
		}
		_ = pct(ctx, "exec", vmid, "--", "mkdir", "-p", "-m", "0700", lxcCaddyfileDir).Run()
		if err := o.writeEnvFile(ctx, vmid, lxcCaddyfilePath, caddyfile); err != nil {
			return Target{}, fmt.Errorf("write auth proxy config: %w", err)
		}
		if out, err := eng.Cmd(ctx, "run", "-d",
			"--name", lxcAppContainer+authSidecarSuffix, "--restart", eng.RestartPolicy(), "--network", linkNetwork,
			"-p", fmt.Sprintf("%d:80", spec.Port),
			"-v", lxcCaddyfilePath+":/etc/caddy/Caddyfile:ro",
			"--", authProxyImage).CombinedOutput(); err != nil {
			return Target{}, fmt.Errorf("auth proxy in lxc: %s", strings.TrimSpace(string(out)))
		}
		spec.LogLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "basic auth proxy fronting the app")
	}

	ip, err := o.lxcIP(ctx, vmid)
	if err != nil {
		return Target{}, err
	}
	// Detached ctx so pruning outlives the deploy.
	go o.pruneImages(context.Background(), eng, spec.WorkloadID, 3)
	spec.LogLine(agentv1.LogStream_LOG_STREAM_SYSTEM, fmt.Sprintf("running in LXC %s at %s:%d", vmid, ip, spec.Port))
	return Target{URL: fmt.Sprintf("http://%s:%d", ip, spec.Port), LxcID: vmid}, nil
}

// runComposeInLXC runs a sanitized compose project in the LXC, with named volumes on the
// same dirs single-image apps use so backups keep working.
func (o *ProxmoxLXC) runComposeInLXC(ctx context.Context, eng *Engine, vmid string, spec RunSpec, hostGPU gpuDevices) (Target, error) {
	if spec.BasicAuth != nil {
		return Target{}, errors.New("basic auth is not available for compose projects")
	}
	if err := o.ensureComposeInLXC(ctx, eng, vmid, spec.LogLine); err != nil {
		return Target{}, err
	}
	const repoDir = "/root/app"
	hasRepo := spec.Repo.GetCloneUrl() != ""
	if hasRepo {
		if err := o.syncRepoInLXC(ctx, vmid, spec.Repo, repoDir, spec.Cache, spec.LogLine); err != nil {
			return Target{}, err
		}
	}
	// Kept outside the checkout, with every repo path pinned inside it.
	const preparedDir = "/root/ownship-compose"
	inRepo := func(p string) (string, error) {
		out, err := pct(ctx, "exec", vmid, "--", "realpath", "-e", "--", p).Output()
		real := strings.TrimSpace(string(out))
		if err != nil {
			return "", fmt.Errorf("%s not found in the repository", strings.TrimPrefix(p, repoDir+"/"))
		}
		if real != repoDir && !strings.HasPrefix(real, repoDir+"/") {
			return "", errors.New("a path resolves outside the repository")
		}
		return real, nil
	}
	projectDir := preparedDir
	var resolveBuild func(string, string) (string, string, error)
	if hasRepo {
		base := repoDir
		if spec.Compose.Inline == "" {
			if err := validComposePath(spec.Compose.Path); err != nil {
				return Target{}, err
			}
			b, err := inRepo(repoDir + "/" + path.Dir(spec.Compose.Path))
			if err != nil {
				return Target{}, fmt.Errorf("compose file directory: %w", err)
			}
			base = b
		}
		projectDir = base
		resolveBuild = func(ctxRel, dfRel string) (string, string, error) {
			c, err := inRepo(base + "/" + ctxRel)
			if err != nil {
				return "", "", fmt.Errorf("build context %s: %w", ctxRel, err)
			}
			if dfRel == "" {
				return c, "", nil
			}
			d, err := inRepo(c + "/" + dfRel)
			if err != nil {
				return "", "", fmt.Errorf("dockerfile %s: %w", dfRel, err)
			}
			return c, d, nil
		}
	}
	var src []byte
	if spec.Compose.Inline != "" {
		src = []byte(spec.Compose.Inline)
	} else {
		if !hasRepo {
			return Target{}, errors.New("a compose path needs a repository")
		}
		file, err := inRepo(repoDir + "/" + spec.Compose.Path)
		if err != nil {
			return Target{}, fmt.Errorf("compose file: %w", err)
		}
		out, err := pct(ctx, "exec", vmid, "--", "head", "-c", strconv.Itoa(maxComposeBytes+1), file).Output()
		if err != nil {
			return Target{}, fmt.Errorf("compose file %s could not be read", spec.Compose.Path)
		}
		src = out
	}

	const envPath = "/run/ownship-app.env"
	if err := o.writeEnvFile(ctx, vmid, envPath, envFileContent(spec.Env, spec.Port)); err != nil {
		return Target{}, err
	}
	gpu, err := composeGPU(spec, hostGPU, func() bool { return eng.HasNvidia(ctx) })
	if err != nil {
		return Target{}, err
	}
	var volumeDirs []string
	file, err := prepareCompose(src, compose.Options{
		Env:              composeEnv(spec.Env, spec.Port),
		Restart:          eng.RestartPolicy(),
		PrimaryService:   spec.Compose.PrimaryService,
		PrimaryContainer: lxcAppContainer,
		PrimaryPublish:   fmt.Sprintf("%d:%d", spec.Port, spec.Port),
		EnvFile:          envPath,
		LinkNetwork:      linkNetwork,
		AllowBuild:       hasRepo,
		WorkloadID:       spec.WorkloadID,
		Volume: func(v string) map[string]any {
			host, _ := safeVolumeHostPath(v)
			volumeDirs = append(volumeDirs, host)
			return map[string]any{"driver": "local", "driver_opts": map[string]any{"type": "none", "o": "bind", "device": host}}
		},
		ResolveBuild: resolveBuild,
		GPU:          gpu,
	}, spec.LogLine)
	if err != nil {
		return Target{}, err
	}
	for _, d := range volumeDirs {
		if d == "" {
			return Target{}, errors.New("compose volume name is not usable")
		}
		_ = pct(ctx, "exec", vmid, "--", "mkdir", "-p", d).Run()
	}
	_ = pct(ctx, "exec", vmid, "--", "mkdir", "-p", "-m", "0700", preparedDir).Run()
	_ = eng.Cmd(ctx, "network", "create", linkNetwork).Run()
	// Don't let compose replace a single-image container of the same name.
	if out, _ := eng.Cmd(ctx, "inspect", "--format",
		`{{index .Config.Labels "com.docker.compose.project"}}`, lxcAppContainer).Output(); strings.TrimSpace(string(out)) != composeProject(spec.WorkloadID) {
		_ = eng.Cmd(ctx, "rm", "-f", lxcAppContainer, lxcAppContainer+authSidecarSuffix).Run()
	}
	composePath := preparedDir + "/" + preparedComposeName
	if err := o.writeEnvFile(ctx, vmid, composePath, string(file)); err != nil {
		return Target{}, err
	}
	spec.LogLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "starting compose project in LXC "+vmid)
	up := eng.ComposeCmd(ctx, composeUpArgs(spec.WorkloadID, composePath, projectDir)[1:]...)
	if err := runStreamingCmd(up, spec.LogLine); err != nil {
		return Target{}, fmt.Errorf("docker compose up in lxc: %w", err)
	}
	ip, err := o.lxcIP(ctx, vmid)
	if err != nil {
		return Target{}, err
	}
	spec.LogLine(agentv1.LogStream_LOG_STREAM_SYSTEM, fmt.Sprintf("compose project up in LXC %s at %s:%d", vmid, ip, spec.Port))
	return Target{URL: fmt.Sprintf("http://%s:%d", ip, spec.Port), LxcID: vmid}, nil
}

// applyGPUPassthrough sets the LXC's devN entries to every host GPU node or none,
// restarting the LXC when they change.
func (o *ProxmoxLXC) applyGPUPassthrough(ctx context.Context, vmid string, want bool, g gpuDevices, logLine LogFunc) error {
	cfg, err := pct(ctx, "config", vmid).Output()
	if err != nil {
		return fmt.Errorf("read lxc config: %w", err)
	}
	ours, used := parseGPUDevEntries(string(cfg))
	var desired []string
	if want {
		// Older hosts reject devN, and only after a restart, so check first.
		if v := o.PveVersion(ctx); !supportsGPUPassthrough(v) {
			return gpuPassthroughRefusal(v)
		}
		if !g.any() {
			return errors.New("this project asks for a GPU, but none was found on the Proxmox host")
		}
		desired = g.nodes()
	}
	if sameStrings(sortedValues(ours), desired) {
		return nil
	}
	if len(ours) > 0 {
		keys := make([]string, 0, len(ours))
		for k := range ours {
			keys = append(keys, k)
			delete(used, k)
		}
		sort.Strings(keys)
		if out, err := pct(ctx, "set", vmid, "--delete", strings.Join(keys, ",")).CombinedOutput(); err != nil {
			return fmt.Errorf("remove gpu passthrough: %s", strings.TrimSpace(string(out)))
		}
	}
	if len(desired) > 0 {
		args := []string{"set", vmid}
		next := 0
		for _, dev := range desired {
			for used[fmt.Sprintf("dev%d", next)] {
				next++
			}
			used[fmt.Sprintf("dev%d", next)] = true
			args = append(args, fmt.Sprintf("--dev%d", next), dev+",mode=0666")
			next++
		}
		if out, err := pct(ctx, args...).CombinedOutput(); err != nil {
			return fmt.Errorf("gpu passthrough (needs Proxmox 8.1+): %s", strings.TrimSpace(string(out)))
		}
		logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "passing the host GPU into LXC "+vmid+": "+strings.Join(desired, " "))
	} else {
		logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "removed GPU passthrough from LXC "+vmid)
	}
	if out, err := pct(ctx, "reboot", vmid).CombinedOutput(); err != nil {
		return fmt.Errorf("restart lxc for gpu passthrough: %s", strings.TrimSpace(string(out)))
	}
	_, err = o.waitForIP(ctx, vmid)
	return err
}

// parseGPUDevEntries returns our GPU devN entries and every devN key in use.
func parseGPUDevEntries(cfg string) (map[string]string, map[string]bool) {
	ours, used := map[string]string{}, map[string]bool{}
	for _, line := range strings.Split(cfg, "\n") {
		key, val, ok := strings.Cut(line, ": ")
		if !ok || !regexp.MustCompile(`^dev[0-9]+$`).MatchString(key) {
			continue
		}
		used[key] = true
		dev, _, _ := strings.Cut(strings.TrimSpace(val), ",")
		dev = strings.TrimPrefix(dev, "path=")
		if strings.HasPrefix(dev, "/dev/nvidia") || strings.HasPrefix(dev, "/dev/dri/") || dev == "/dev/kfd" {
			ours[key] = dev
		}
	}
	return ours, used
}

func sortedValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (o *ProxmoxLXC) ensureComposeInLXC(ctx context.Context, eng *Engine, vmid string, logLine LogFunc) error {
	if eng.ComposeCmd(ctx, "version").Run() == nil {
		return nil
	}
	logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "installing docker compose for "+eng.Name()+" in LXC "+vmid)
	if err := runStreaming(ctx, "", "pct", []string{"exec", vmid, "--", "sh", "-c", composeInstallScript(eng)}, logLine); err != nil {
		return fmt.Errorf("install docker compose in lxc: %w", err)
	}
	return nil
}

// Standalone compose binary for Podman LXCs.
const composeVersion = "v2.32.4"

// composeInstallScript: Docker gets the plugin; Podman has no docker CLI, so it gets the
// standalone binary.
func composeInstallScript(eng *Engine) string {
	if eng.Kind == KindDocker {
		return pkgInstallScript("docker-compose-plugin")
	}
	return fmt.Sprintf(`set -e
command -v curl >/dev/null 2>&1 || { %s
}
curl -fsSL -o /usr/local/bin/docker-compose "https://github.com/docker/compose/releases/download/%s/docker-compose-linux-$(uname -m)"
chmod 0755 /usr/local/bin/docker-compose`, pkgInstallScript("curl"), composeVersion)
}

// pruneImages keeps the newest `keep` deploy images.
func (o *ProxmoxLXC) pruneImages(ctx context.Context, eng *Engine, projectID string, keep int) {
	ref := "ownship/" + projectID + ":*"
	out, err := eng.Cmd(ctx, "images",
		"--filter=reference="+ref, "--format", "{{.Repository}}:{{.Tag}}").Output()
	if err != nil {
		return
	}
	tags := strings.Fields(string(out))
	if len(tags) > keep {
		// In-use images fail to remove and are skipped.
		_ = eng.Cmd(ctx, append([]string{"rmi"}, tags[keep:]...)...).Run()
	}
}

// List reports tracked workloads whose LXC still exists. Only ensureLXC and Destroy forget
// a vmid, once they've seen it gone.
func (o *ProxmoxLXC) List(ctx context.Context) (Inventory, error) {
	tracked := o.state.snapshot()
	lxcs, err := clusterLXCs(ctx)
	if err != nil {
		return Inventory{IDs: o.state.projectIDs()}, err
	}
	inv := Inventory{Complete: true}
	trackedVmids := make(map[string]bool, len(tracked))
	for id, vmid := range tracked {
		trackedVmids[vmid] = true
		if _, ok := lxcs[vmid]; ok {
			inv.IDs = append(inv.IDs, id)
		}
	}
	// Before Adopt, an untracked ownship LXC may be a mapping a reinstall lost. After it, a
	// stray one (a failed create, another homelab on the cluster) must not mute detection.
	if !o.adopted.Load() {
		for vmid, tagged := range lxcs {
			if tagged && !trackedVmids[vmid] {
				inv.Complete = false
				break
			}
		}
	}
	sort.Strings(inv.IDs)
	return inv, nil
}

// Adopt tracks workload->vmid pairs the control plane recorded, for a state file that was
// lost. Only an existing ownship-tagged LXC no other workload holds is taken.
func (o *ProxmoxLXC) Adopt(ctx context.Context, pairs map[string]string) ([]string, error) {
	lxcs, err := clusterLXCs(ctx)
	if err != nil {
		return nil, err
	}
	held := map[string]bool{}
	for _, vmid := range o.state.snapshot() {
		held[vmid] = true
	}
	// Two rows naming one vmid can't both be right; picking one would hand it the other's LXC.
	claims := map[string]int{}
	for _, vmid := range pairs {
		claims[vmid]++
	}
	var adopted []string
	for id, vmid := range pairs {
		if _, ok := o.state.get(id); ok || held[vmid] || claims[vmid] > 1 || !lxcs[vmid] {
			continue
		}
		if err := o.state.set(id, vmid); err != nil {
			sort.Strings(adopted)
			return adopted, err
		}
		held[vmid] = true
		adopted = append(adopted, id)
	}
	o.adopted.Store(true)
	sort.Strings(adopted)
	return adopted, nil
}

// FollowLogs tails the app container; compose projects tail every service, prefixed by name.
func (o *ProxmoxLXC) FollowLogs(ctx context.Context, workloadID string, onLine LogFunc) error {
	vmid, ok := o.state.get(workloadID)
	if !ok {
		return fmt.Errorf("no LXC for workload %s", workloadID)
	}
	eng := o.lxcEngine(ctx, workloadID, vmid)
	out, _ := eng.Cmd(ctx, "ps", "-aq",
		"--filter", "label=com.docker.compose.project="+composeProject(workloadID)).Output()
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return runStreamingCmd(eng.Cmd(ctx, "logs", "-f", "--tail", "0", lxcAppContainer), onLine)
	}
	var wg sync.WaitGroup
	for _, id := range ids {
		svc, _ := eng.Cmd(ctx, "inspect", "--format",
			`{{index .Config.Labels "com.docker.compose.service"}}`, id).Output()
		prefix := "[" + strings.TrimSpace(string(svc)) + "] "
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			_ = runStreamingCmd(eng.Cmd(ctx, "logs", "-f", "--tail", "0", id),
				func(s agentv1.LogStream, line string) { onLine(s, prefix+line) })
		}(id)
	}
	wg.Wait()
	return nil
}

// Stop stops the app and shuts the LXC down to free node resources.
func (o *ProxmoxLXC) Stop(ctx context.Context, workloadID string) error {
	vmid, ok := o.state.get(workloadID)
	if !ok {
		return nil
	}
	_ = o.lxcEngine(ctx, workloadID, vmid).Cmd(ctx, "stop", lxcAppContainer).Run()
	// Otherwise Podman's restart service starts it on the next boot; Resume re-applies this.
	_ = o.engines.SetAsleep(workloadID, true)
	return pct(ctx, "shutdown", vmid).Run()
}

func (o *ProxmoxLXC) Start(ctx context.Context, workloadID string) error {
	vmid, ok := o.state.get(workloadID)
	if !ok {
		return fmt.Errorf("no LXC for workload %s", workloadID)
	}
	if err := pct(ctx, "start", vmid).Run(); err != nil {
		return err
	}
	if _, err := o.waitForIP(ctx, vmid); err != nil {
		return err
	}
	_ = o.engines.SetAsleep(workloadID, false)
	return o.lxcEngine(ctx, workloadID, vmid).Cmd(ctx, "start", lxcAppContainer).Run()
}

// NetworkUpdate changes net0/nameserver in place and restarts the LXC.
func (o *ProxmoxLXC) NetworkUpdate(ctx context.Context, workloadID string, network *agentv1.NetworkConfig) error {
	vmid, ok := o.state.get(workloadID)
	if !ok {
		return fmt.Errorf("no LXC for workload %s", workloadID)
	}
	net0, nameservers, err := net0Arg(o.cfg.Bridge, network)
	if err != nil {
		return err
	}
	args := []string{"set", vmid, "--net0", net0}
	if len(nameservers) > 0 {
		args = append(args, "--nameserver", strings.Join(nameservers, " "))
	} else {
		// Inheriting the host resolver means clearing any nameserver set before.
		args = append(args, "--delete", "nameserver")
	}
	if err := pct(ctx, args...).Run(); err != nil {
		return fmt.Errorf("pct set net0: %w", err)
	}
	if err := o.Stop(ctx, workloadID); err != nil {
		return fmt.Errorf("stop before network restart: %w", err)
	}
	return o.Start(ctx, workloadID)
}

// lxcStatsScript returns the stats tuple on line 1 and the restart count on the last line.
func lxcStatsScript(e *Engine) string {
	return e.Bin + " stats --no-stream --format '{{.CPUPerc}};{{.MemUsage}};{{.NetIO}}' " + lxcAppContainer + "\n" +
		e.Bin + " inspect --format '{{.RestartCount}}' " + lxcAppContainer
}

// StatsAll samples every LXC concurrently; a failure drops only that workload's sample.
func (o *ProxmoxLXC) StatsAll(ctx context.Context) (map[string]StatsSample, error) {
	pairs := o.state.snapshot()
	out := make(map[string]StatsSample, len(pairs))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for wl, vmid := range pairs {
		wg.Add(1)
		sem <- struct{}{}
		go func(wl, vmid string) {
			defer wg.Done()
			defer func() { <-sem }()
			s, err := o.statsOne(ctx, o.lxcEngine(ctx, wl, vmid))
			if err != nil {
				return
			}
			mu.Lock()
			out[wl] = s
			mu.Unlock()
		}(wl, vmid)
	}
	wg.Wait()
	return out, nil
}

func (o *ProxmoxLXC) statsOne(ctx context.Context, eng *Engine) (StatsSample, error) {
	out, err := eng.Raw(ctx, "sh", "-c", lxcStatsScript(eng)).Output()
	if err != nil {
		return StatsSample{}, err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 {
		return StatsSample{}, fmt.Errorf("no stats output")
	}
	s := parseDockerStats(strings.TrimSpace(lines[0]))
	if len(lines) >= 2 {
		s.RestartCount = parseUint(strings.TrimSpace(lines[len(lines)-1]))
	}
	return s, nil
}

func (o *ProxmoxLXC) ExecArgs(ctx context.Context, workloadID string, inner []string, tty bool) ([]string, error) {
	vmid, ok := o.state.get(workloadID)
	if !ok {
		return nil, fmt.Errorf("no LXC for workload %s", workloadID)
	}
	args := []string{"exec", "-i"}
	if tty {
		args = append(args, "-t")
	}
	args = append(args, lxcAppContainer)
	return o.lxcEngine(ctx, workloadID, vmid).ExecArgv(append(args, inner...)...), nil
}

// MachineExecArgs opens a shell in the LXC rather than the app container.
func (o *ProxmoxLXC) MachineExecArgs(ctx context.Context, workloadID string, inner []string, tty bool) ([]string, error) {
	vmid, ok := o.state.get(workloadID)
	if !ok {
		return nil, fmt.Errorf("no LXC for workload %s", workloadID)
	}
	return append([]string{"pct", "exec", vmid, "--"}, inner...), nil
}

// Destroy removes the workload's LXC.
func (o *ProxmoxLXC) Destroy(ctx context.Context, workloadID string, keepVolumes bool) error {
	vmid, ok := o.state.get(workloadID)
	if !ok {
		return nil
	}
	// Deleted outside Ownship: only the mapping is left.
	if lxcGone(ctx, vmid) {
		return o.state.remove(workloadID)
	}
	if keepVolumes {
		// Volumes live on the LXC's rootfs, so keeping them means keeping the LXC, stopped.
		_ = o.lxcEngine(ctx, workloadID, vmid).Cmd(ctx, "rm", "-f", lxcAppContainer).Run()
		_ = pct(ctx, "stop", vmid).Run()
		return nil
	}
	_ = pct(ctx, "stop", vmid).Run()
	if out, err := pct(ctx, "destroy", vmid, "--purge").CombinedOutput(); err != nil {
		return fmt.Errorf("destroy lxc: %s", strings.TrimSpace(string(out)))
	}
	_ = o.engines.Remove(workloadID)
	return o.state.remove(workloadID)
}

func (o *ProxmoxLXC) Nodes(ctx context.Context) []*agentv1.NodeInfo {
	o.nodesMu.Lock()
	if o.nodesCache != nil && time.Since(o.nodesAt) < nodesCacheTTL {
		cached := o.nodesCache
		o.nodesMu.Unlock()
		return cached
	}
	o.nodesMu.Unlock()

	out, err := exec.CommandContext(ctx, "pvesh", "get", "/nodes", "--output-format", "json").Output()
	if err != nil {
		return nil
	}
	var raw []struct {
		Node    string  `json:"node"`
		MaxMem  uint64  `json:"maxmem"`
		Mem     uint64  `json:"mem"`
		MaxCPU  float64 `json:"maxcpu"`
		CPU     float64 `json:"cpu"`
		Disk    uint64  `json:"disk"`
		MaxDisk uint64  `json:"maxdisk"`
	}
	if json.Unmarshal(out, &raw) != nil {
		return nil
	}
	nodes := make([]*agentv1.NodeInfo, 0, len(raw))
	for _, n := range raw {
		nodes = append(nodes, &agentv1.NodeInfo{
			Name:     n.Node,
			MemTotal: n.MaxMem,
			MemFree:  n.MaxMem - n.Mem,
			DiskFree: n.MaxDisk - n.Disk,
			Load:     n.CPU,
		})
	}

	o.nodesMu.Lock()
	o.nodesCache = nodes
	o.nodesAt = time.Now()
	o.nodesMu.Unlock()
	return nodes
}

// Templates lists the vztmpl volids visible on this node, in the form pct create takes.
func (o *ProxmoxLXC) Templates(ctx context.Context) []string {
	o.discoMu.Lock()
	if !o.templatesAt.IsZero() && time.Since(o.templatesAt) < discoCacheTTL {
		cached := o.templatesCache
		o.discoMu.Unlock()
		return cached
	}
	o.discoMu.Unlock()

	volids, _ := o.templatesOn(ctx, o.localNodeName(ctx))

	o.discoMu.Lock()
	o.templatesCache = volids
	o.templatesAt = time.Now()
	o.discoMu.Unlock()
	return volids
}

// InvalidateCaches forces fresh inventory on the next report, e.g. after a template download.
func (o *ProxmoxLXC) InvalidateCaches() {
	o.nodesMu.Lock()
	o.nodesCache = nil
	o.nodesAt = time.Time{}
	o.nodesMu.Unlock()

	o.discoMu.Lock()
	o.templatesCache = nil
	o.templatesAt = time.Time{}
	o.bridgesCache = nil
	o.bridgesAt = time.Time{}
	o.discoMu.Unlock()
}

// Bridges lists the network bridges on the local node.
func (o *ProxmoxLXC) Bridges(ctx context.Context) []string {
	o.discoMu.Lock()
	if !o.bridgesAt.IsZero() && time.Since(o.bridgesAt) < discoCacheTTL {
		cached := o.bridgesCache
		o.discoMu.Unlock()
		return cached
	}
	o.discoMu.Unlock()

	bridges := o.readBridges(ctx)

	o.discoMu.Lock()
	o.bridgesCache = bridges
	o.bridgesAt = time.Now()
	o.discoMu.Unlock()
	return bridges
}

func (o *ProxmoxLXC) readBridges(ctx context.Context) []string {
	seen := map[string]bool{}
	var bridges []string
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			bridges = append(bridges, name)
		}
	}

	// sysfs first: it needs no pvesh or permissions. fwbr* are per-guest firewall bridges.
	if entries, err := os.ReadDir("/sys/class/net"); err == nil {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "fwbr") {
				continue
			}
			if _, err := os.Stat("/sys/class/net/" + e.Name() + "/bridge"); err == nil {
				add(e.Name())
			}
		}
	}

	// pvesh also finds OVS bridges and ones that are down.
	node := o.localNodeName(ctx)
	if b, err := exec.CommandContext(ctx, "pvesh", "get", "/nodes/"+node+"/network", "--output-format", "json").Output(); err == nil {
		var items []struct {
			Iface string `json:"iface"`
			Type  string `json:"type"`
		}
		if json.Unmarshal(b, &items) == nil {
			for _, it := range items {
				t := strings.ToLower(it.Type)
				if t == "bridge" || t == "ovsbridge" {
					add(it.Iface)
				}
			}
		}
	}
	return bridges
}

func (o *ProxmoxLXC) localNodeName(ctx context.Context) string {
	o.nodeNameMu.Lock()
	if o.nodeName != "" {
		name := o.nodeName
		o.nodeNameMu.Unlock()
		return name
	}
	o.nodeNameMu.Unlock()

	name := o.resolveLocalNodeName(ctx)

	o.nodeNameMu.Lock()
	o.nodeName = name
	o.nodeNameMu.Unlock()
	return name
}

// resolveLocalNodeName matches the short hostname, falling back when hostname fails.
func (o *ProxmoxLXC) resolveLocalNodeName(ctx context.Context) string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		if nodes := o.Nodes(ctx); len(nodes) > 0 {
			return nodes[0].GetName()
		}
		return "localhost"
	}
	if short, _, ok := strings.Cut(host, "."); ok {
		host = short
	}
	if nodes := o.Nodes(ctx); len(nodes) > 0 {
		for _, n := range nodes {
			if strings.EqualFold(n.GetName(), host) {
				return n.GetName()
			}
		}
		// A single node is the local one even if the OS hostname was renamed.
		if len(nodes) == 1 {
			return nodes[0].GetName()
		}
	}
	// With several nodes and no match, guessing a peer would misroute discovery.
	return host
}

// templatesOn returns failures as diagnostics, since an unexplained empty list hides the fix.
func (o *ProxmoxLXC) templatesOn(ctx context.Context, node string) (volids, diags []string) {
	storages, diags := o.vztmplStoragesOn(ctx, node)
	for _, storage := range storages {
		b, err := exec.CommandContext(ctx, "pvesh", "get",
			"/nodes/"+node+"/storage/"+storage+"/content", "--content", "vztmpl", "--output-format", "json").Output()
		if err != nil {
			diags = append(diags, fmt.Sprintf("listing templates on %s/%s failed: %s", node, storage, execErrDetail(err)))
			continue
		}
		var items []struct {
			Volid string `json:"volid"`
		}
		if err := json.Unmarshal(b, &items); err != nil {
			diags = append(diags, fmt.Sprintf("listing templates on %s/%s: unparseable pvesh output", node, storage))
			continue
		}
		for _, it := range items {
			if it.Volid != "" {
				volids = append(volids, it.Volid)
			}
		}
	}
	return volids, diags
}

// vztmplStoragesOn falls back to `local` when discovery fails.
func (o *ProxmoxLXC) vztmplStoragesOn(ctx context.Context, node string) (storages, diags []string) {
	b, err := exec.CommandContext(ctx, "pvesh", "get",
		"/nodes/"+node+"/storage", "--content", "vztmpl", "--output-format", "json").Output()
	if err != nil {
		return []string{"local"}, []string{fmt.Sprintf("storage discovery on node %s failed (%s), assuming [local]", node, execErrDetail(err))}
	}
	var items []struct {
		Storage string `json:"storage"`
		Enabled *int   `json:"enabled"`
		Active  *int   `json:"active"`
	}
	if err := json.Unmarshal(b, &items); err != nil {
		return []string{"local"}, []string{fmt.Sprintf("storage discovery on node %s: unparseable pvesh output, assuming [local]", node)}
	}
	for _, it := range items {
		if it.Storage == "" || (it.Enabled != nil && *it.Enabled == 0) {
			continue
		}
		storages = append(storages, it.Storage)
	}
	if len(storages) == 0 {
		return []string{"local"}, []string{fmt.Sprintf("no storage on node %s advertises vztmpl content, assuming [local]", node)}
	}
	return storages, nil
}

// firstSharedVztmplStorage: only shared storage is useful for a download from another node.
func (o *ProxmoxLXC) firstSharedVztmplStorage(ctx context.Context, node string) (string, error) {
	b, err := exec.CommandContext(ctx, "pvesh", "get",
		"/nodes/"+node+"/storage", "--content", "vztmpl", "--output-format", "json").Output()
	if err != nil {
		return "", fmt.Errorf("storage discovery on node %s: %s", node, execErrDetail(err))
	}
	var items []struct {
		Storage string `json:"storage"`
		Shared  *int   `json:"shared"`
	}
	if err := json.Unmarshal(b, &items); err != nil {
		return "", fmt.Errorf("storage discovery on node %s: unparseable pvesh output", node)
	}
	for _, it := range items {
		if it.Shared != nil && *it.Shared == 1 && it.Storage != "" {
			return it.Storage, nil
		}
	}
	return "", fmt.Errorf("no shared vztmpl storage on node %s", node)
}

// ensureLXC returns the project's LXC, creating it on first deploy from templateVmid,
// else baseTemplate, baseOS or a fallback.
func (o *ProxmoxLXC) ensureLXC(ctx context.Context, workloadID, baseTemplate, templateVmid, baseOS string, res *agentv1.Resources, targetNode, requestedHostname string, network *agentv1.NetworkConfig, logLine LogFunc) (string, error) {
	defer o.createLocks.lock(workloadID)()

	if vmid, ok := o.state.get(workloadID); ok {
		if !lxcGone(ctx, vmid) {
			// Redeploys need the same resize, or new cores/memory limits are ignored.
			if err := o.sizeAndStart(ctx, vmid, workloadID, res); err != nil {
				return "", err
			}
			return vmid, nil
		}
		logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "LXC "+vmid+" no longer exists in Proxmox; creating a new one")
		if err := o.state.remove(workloadID); err != nil {
			return "", err
		}
	}

	// These reach pct argv: reject anything that could pass as a flag.
	if !validPctArg(templateVmid, reVmid) {
		return "", fmt.Errorf("invalid template VMID %q", templateVmid)
	}
	if !validPctArg(targetNode, reNode) {
		return "", fmt.Errorf("invalid target node %q", targetNode)
	}
	if !validPctArg(baseTemplate, reVolid) {
		return "", fmt.Errorf("invalid base template %q", baseTemplate)
	}
	if !validPctArg(baseOS, reBaseOS) {
		return "", fmt.Errorf("invalid base OS %q", baseOS)
	}

	o.vmidMu.Lock()
	newVmid, err := nextVmid(ctx)
	if err != nil {
		o.vmidMu.Unlock()
		return "", err
	}
	hostname := lxcHostname(requestedHostname, workloadID)

	if templateVmid != "" {
		args := []string{"clone", templateVmid, newVmid, "--hostname", hostname}
		if targetNode != "" {
			args = append(args, "--target", targetNode)
		}
		logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "cloning LXC "+newVmid+" from template "+templateVmid)
		out, cloneErr := pct(ctx, args...).CombinedOutput()
		o.vmidMu.Unlock() // vmid is claimed once clone/create runs
		if cloneErr != nil {
			return "", fmt.Errorf("clone template: %s", strings.TrimSpace(string(out)))
		}
		// pct clone takes no --tags; the marker is best-effort.
		_ = pct(ctx, "set", newVmid, "--tags", ownshipTag).Run()
		if err := o.sizeAndStart(ctx, newVmid, workloadID, res); err != nil {
			return "", err
		}
		return newVmid, nil
	}

	// pct create has no --target, so refuse rather than create on the wrong node.
	if local := o.localNodeName(ctx); targetNode != "" && targetNode != local {
		o.vmidMu.Unlock()
		return "", fmt.Errorf(
			"creating an LXC from a base template on node %s isn't supported from this agent (running on %s) — use a golden template VMID (clone supports --target) or leave the node unpinned",
			targetNode, local)
	}

	// Base image priority: explicit volid, then curated base OS, then best-effort pick.
	if baseTemplate == "" {
		var err error
		if baseOS != "" {
			baseTemplate, err = o.resolveBaseOS(ctx, baseOS, targetNode, logLine)
		} else {
			volids, diags := o.templatesOn(ctx, o.localNodeName(ctx))
			for _, d := range diags {
				logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, d)
			}
			baseTemplate, err = rankFallback(volids)
			if err == nil {
				logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "no base OS specified, auto-picked template "+baseTemplate)
			}
		}
		if err != nil {
			o.vmidMu.Unlock()
			return "", err
		}
	}
	cores := "1"
	if v := res.GetVcpu(); v > 0 {
		cores = strconv.FormatUint(uint64(v), 10)
	}
	mem := "512"
	if m := res.GetMemoryMb(); m > 0 {
		mem = strconv.FormatUint(uint64(m), 10)
	}
	net0, nameservers, err := net0Arg(o.cfg.Bridge, network)
	if err != nil {
		o.vmidMu.Unlock()
		return "", err
	}
	args := []string{"create", newVmid, baseTemplate,
		"--hostname", hostname,
		"--cores", cores,
		"--memory", mem,
		"--rootfs", fmt.Sprintf("%s:%d", o.cfg.Storage, o.cfg.RootfsGB),
		"--net0", net0,
		// Required to run Docker in an unprivileged LXC.
		"--features", "nesting=1,keyctl=1",
		"--unprivileged", "1",
		"--onboot", "1",
		"--tags", ownshipTag,
	}
	// Empty inherits the host's resolver, like stock pct.
	if len(nameservers) > 0 {
		args = append(args, "--nameserver", strings.Join(nameservers, " "))
	}
	logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "creating LXC "+newVmid+" from "+baseTemplate)
	out, createErr := pct(ctx, args...).CombinedOutput()
	o.vmidMu.Unlock() // vmid is claimed once clone/create runs
	if createErr != nil {
		return "", fmt.Errorf("create lxc: %s", strings.TrimSpace(string(out)))
	}
	if err := o.startAndTrack(ctx, newVmid, workloadID); err != nil {
		return "", err
	}
	return newVmid, nil
}

// sizeAndStart resizes the LXC, starts it and waits for an IP.
func (o *ProxmoxLXC) sizeAndStart(ctx context.Context, vmid, workloadID string, res *agentv1.Resources) error {
	setArgs := []string{"set", vmid}
	if v := res.GetVcpu(); v > 0 {
		setArgs = append(setArgs, "--cores", strconv.FormatUint(uint64(v), 10))
	}
	if m := res.GetMemoryMb(); m > 0 {
		setArgs = append(setArgs, "--memory", strconv.FormatUint(uint64(m), 10))
	}
	if len(setArgs) > 2 {
		if out, err := pct(ctx, setArgs...).CombinedOutput(); err != nil {
			return fmt.Errorf("size lxc: %s", strings.TrimSpace(string(out)))
		}
	}
	return o.startAndTrack(ctx, vmid, workloadID)
}

// startAndTrack starts the LXC, records the mapping and waits for an IP.
func (o *ProxmoxLXC) startAndTrack(ctx context.Context, vmid, workloadID string) error {
	if out, err := pct(ctx, "start", vmid).CombinedOutput(); err != nil {
		return fmt.Errorf("start lxc: %s", strings.TrimSpace(string(out)))
	}
	if err := o.state.set(workloadID, vmid); err != nil {
		return err
	}
	if _, err := o.waitForIP(ctx, vmid); err != nil {
		return err
	}
	return nil
}

// pkgInstallScript uses apt-get, dnf or yum, and exits 0 when none exists.
func pkgInstallScript(pkgs ...string) string {
	list := strings.Join(pkgs, " ")
	return fmt.Sprintf(`set -e
export LC_ALL=C LANG=C DEBIAN_FRONTEND=noninteractive
if command -v apt-get >/dev/null 2>&1; then
  apt-get update -y -q && apt-get install -y -q %s
elif command -v dnf >/dev/null 2>&1; then
  dnf install -y %s
elif command -v yum >/dev/null 2>&1; then
  yum install -y %s
else
  echo "no known package manager (apt-get/dnf/yum) found, skipping install of %s"
fi`, list, list, list, list)
}

// ensureDocker installs Docker via get.docker.com, which supports any base template.
func (o *ProxmoxLXC) ensureDocker(ctx context.Context, vmid string, logLine LogFunc) error {
	if pct(ctx, "exec", vmid, "--", "sh", "-c", "command -v docker >/dev/null 2>&1").Run() == nil {
		return nil
	}
	logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "installing Docker in LXC "+vmid)
	curlScript := fmt.Sprintf(`command -v curl >/dev/null 2>&1 || { %s
}`, pkgInstallScript("curl"))
	if err := runStreaming(ctx, "", "pct", []string{"exec", vmid, "--", "sh", "-c", curlScript}, logLine); err != nil {
		return fmt.Errorf("install curl in lxc: %w", err)
	}
	script := `set -e
export LC_ALL=C LANG=C DEBIAN_FRONTEND=noninteractive
curl -fsSL https://get.docker.com | sh
(systemctl enable --now docker 2>/dev/null || service docker start 2>/dev/null || true)`
	if err := runStreaming(ctx, "", "pct", []string{"exec", vmid, "--", "sh", "-c", script}, logLine); err != nil {
		return fmt.Errorf("install docker in lxc: %w", err)
	}
	return nil
}

func (o *ProxmoxLXC) ensureEngine(ctx context.Context, eng *Engine, vmid string, logLine LogFunc) error {
	if eng.Kind == KindDocker {
		return o.ensureDocker(ctx, vmid, logLine)
	}
	if pct(ctx, "exec", vmid, "--", "sh", "-c", "command -v podman >/dev/null 2>&1").Run() == nil {
		return nil
	}
	logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "installing Podman in LXC "+vmid)
	if err := runStreaming(ctx, "", "pct", []string{"exec", vmid, "--", "sh", "-c", engineInstallScript(eng.Kind)}, logLine); err != nil {
		return fmt.Errorf("install podman in lxc: %w", err)
	}
	return nil
}

// engineInstallScript installs Podman with its API socket (compose) and restart service.
func engineInstallScript(k EngineKind) string {
	return pkgInstallScript("podman", "fuse-overlayfs") + `
(systemctl enable --now podman.socket podman-restart.service 2>/dev/null || true)`
}

// imageUser returns the image's Config.User, pulling it if needed; "" (root) on failure.
func (o *ProxmoxLXC) imageUser(ctx context.Context, eng *Engine, image string) (string, error) {
	out, err := eng.Cmd(ctx, "image", "inspect",
		"--format", "{{.Config.User}}", "--", image).Output()
	if err != nil {
		if _, pullErr := eng.Cmd(ctx, "pull", "--", image).CombinedOutput(); pullErr != nil {
			return "", fmt.Errorf("pull for inspect: %w", pullErr)
		}
		out, err = eng.Cmd(ctx, "image", "inspect",
			"--format", "{{.Config.User}}", "--", image).Output()
		if err != nil {
			return "", fmt.Errorf("%s image inspect: %w", eng.Name(), err)
		}
	}
	return string(out), nil
}

// buildInLXC builds with the repo's Dockerfile if present or forced, else Nixpacks.
func (o *ProxmoxLXC) buildInLXC(ctx context.Context, eng *Engine, vmid, image string, repo *agentv1.RepoSource, build *agentv1.BuildConfig, cache bool, logLine LogFunc) error {
	const dir = "/root/app"
	if err := o.syncRepoInLXC(ctx, vmid, repo, dir, cache, logLine); err != nil {
		return err
	}

	useDockerfile := chooseDockerfile(build.GetType(), func() bool {
		return pct(ctx, "exec", vmid, "--", "test", "-f", dir+"/Dockerfile").Run() == nil
	})
	if useDockerfile {
		return runStreamingCmd(eng.Cmd(ctx, "build", "-t", image, dir), logLine)
	}
	install := "command -v nixpacks >/dev/null 2>&1 || curl -fsSL https://nixpacks.com/install.sh | bash"
	if err := runStreaming(ctx, "", "pct", []string{"exec", vmid, "--", "sh", "-c", install}, logLine); err != nil {
		return fmt.Errorf("install nixpacks in lxc: %w", err)
	}
	// dir and image are positional args, never part of the script.
	return runStreaming(ctx, "", "pct",
		[]string{"exec", vmid, "--", "sh", "-c", lxcNixpacksScript(eng), "sh", dir, image}, logLine)
}

// lxcNixpacksScript: Docker lets nixpacks build; Podman builds the plan nixpacks writes.
func lxcNixpacksScript(eng *Engine) string {
	if eng.Kind == KindDocker {
		return `cd "$1" && nixpacks build . --name "$2"`
	}
	return `cd "$1" && nixpacks build . --out . && ` + eng.Bin + ` build -f .nixpacks/Dockerfile -t "$2" .`
}

// syncRepoInLXC fetches into a cached checkout, falling back to a fresh shallow clone.
func (o *ProxmoxLXC) syncRepoInLXC(ctx context.Context, vmid string, repo *agentv1.RepoSource, dir string, cache bool, logLine LogFunc) error {
	o.ensureGitInLXC(ctx, vmid)
	if cache && pct(ctx, "exec", vmid, "--", "test", "-d", dir+"/.git").Run() == nil {
		if err := o.updateRepoInLXC(ctx, vmid, repo, dir, logLine); err == nil {
			return nil
		}
		logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "workspace update failed, falling back to fresh clone")
	}
	_ = pct(ctx, "exec", vmid, "--", "rm", "-rf", dir).Run()
	return o.cloneRepoInLXC(ctx, vmid, repo, dir, logLine)
}

func (o *ProxmoxLXC) ensureGitInLXC(ctx context.Context, vmid string) {
	gitScript := fmt.Sprintf(`command -v git >/dev/null 2>&1 || { %s
}`, pkgInstallScript("git", "ca-certificates"))
	_ = pct(ctx, "exec", vmid, "--", "sh", "-c", gitScript).Run()
}

// gitBaseArgs keeps a token or deploy key in a 0600 file in the LXC, never argv,
// with the same protocol allowlist and SSH behaviour as hostGitAuth.
func (o *ProxmoxLXC) gitBaseArgs(ctx context.Context, vmid string, repo *agentv1.RepoSource) ([]string, func(), error) {
	args := append([]string{"exec", vmid, "--", "git"}, gitProtocolArgs()...)
	var files []string
	cleanup := func() {
		if len(files) > 0 {
			_ = pct(context.Background(), append([]string{"exec", vmid, "--", "rm", "-f"}, files...)...).Run()
		}
	}
	if token := repo.GetInstallationToken(); token != "" {
		const tokenPath = "/run/ownship-git-token"
		if err := o.writeEnvFile(ctx, vmid, tokenPath, token); err != nil {
			return nil, nil, err
		}
		files = append(files, tokenPath)
		args = append(args, "-c",
			`credential.helper=!f() { echo username=x-access-token; echo "password=$(cat `+tokenPath+`)"; }; f`)
	}
	if key := repo.GetSshPrivateKey(); key != "" {
		o.ensureSSHInLXC(ctx, vmid)
		const keyPath = "/run/ownship-deploy-key"
		// Persistent, so a host key trusted on first use stays pinned.
		const knownHosts = "/root/.ownship-known-hosts"
		if err := o.writeEnvFile(ctx, vmid, keyPath, key+"\n"); err != nil {
			cleanup()
			return nil, nil, err
		}
		files = append(files, keyPath)
		pinned := repo.GetSshKnownHosts() != ""
		if pinned {
			if err := o.writeEnvFile(ctx, vmid, knownHosts, repo.GetSshKnownHosts()+"\n"); err != nil {
				cleanup()
				return nil, nil, err
			}
		}
		sshArgs, err := sshCommandArg(keyPath, knownHosts, pinned)
		if err != nil {
			cleanup()
			return nil, nil, err
		}
		args = append(args, sshArgs...)
	}
	return args, cleanup, nil
}

// ensureSSHInLXC: the package is openssh-client on apt, openssh-clients on dnf/yum.
func (o *ProxmoxLXC) ensureSSHInLXC(ctx context.Context, vmid string) {
	script := `command -v ssh >/dev/null 2>&1 && exit 0
export LC_ALL=C LANG=C DEBIAN_FRONTEND=noninteractive
if command -v apt-get >/dev/null 2>&1; then apt-get update -y -q && apt-get install -y -q openssh-client
elif command -v dnf >/dev/null 2>&1; then dnf install -y openssh-clients
elif command -v yum >/dev/null 2>&1; then yum install -y openssh-clients
fi`
	_ = pct(ctx, "exec", vmid, "--", "sh", "-c", script).Run()
}

func (o *ProxmoxLXC) cloneRepoInLXC(ctx context.Context, vmid string, repo *agentv1.RepoSource, dir string, logLine LogFunc) error {
	args, cleanup, err := o.gitBaseArgs(ctx, vmid, repo)
	if err != nil {
		return err
	}
	defer cleanup()
	clone := append(args, "clone", "--depth", "1")
	if ref := repo.GetRef(); ref != "" {
		clone = append(clone, "--branch", ref)
	}
	clone = append(clone, "--end-of-options", repo.GetCloneUrl(), dir)
	if err := runStreaming(ctx, "", "pct", clone, logLine); err != nil {
		return fmt.Errorf("git clone in lxc: %w", err)
	}
	return nil
}

// updateRepoInLXC refreshes a checkout without recloning, like the host gitUpdate.
func (o *ProxmoxLXC) updateRepoInLXC(ctx context.Context, vmid string, repo *agentv1.RepoSource, dir string, logLine LogFunc) error {
	ref := repo.GetRef()
	if ref == "" {
		ref = "HEAD"
	}
	args, cleanup, err := o.gitBaseArgs(ctx, vmid, repo)
	if err != nil {
		return err
	}
	defer cleanup()
	fetch := append(args, "-C", dir, "fetch", "--depth", "1", "--end-of-options", "origin", ref)
	if err := runStreaming(ctx, "", "pct", fetch, logLine); err != nil {
		return err
	}
	if err := runStreaming(ctx, "", "pct",
		[]string{"exec", vmid, "--", "git", "-C", dir, "reset", "--hard", "FETCH_HEAD"}, logLine); err != nil {
		return err
	}
	// Remove untracked files so the build context matches the commit.
	return runStreaming(ctx, "", "pct",
		[]string{"exec", vmid, "--", "git", "-C", dir, "clean", "-fd"}, logLine)
}

// applyRootPassword pipes the password to chpasswd to keep it off argv. Best-effort:
// without chpasswd only console access is lost.
func (o *ProxmoxLXC) applyRootPassword(ctx context.Context, vmid, password string, logLine LogFunc) {
	if password == "" || strings.ContainsAny(password, ":\n") {
		return
	}
	cmd := pct(ctx, "exec", vmid, "--", "chpasswd")
	cmd.Stdin = strings.NewReader("root:" + password + "\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "could not set the machine root password: "+strings.TrimSpace(string(out)))
	}
}

func (o *ProxmoxLXC) writeEnvFile(ctx context.Context, vmid, path, content string) error {
	cmd := pct(ctx, "exec", vmid, "--", "sh", "-c", "umask 077; cat > "+path)
	cmd.Stdin = strings.NewReader(content)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("write env file in lxc: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// Backups stream through pct exec's stdio; paths come only from safeVolumeHostPath.

func (o *ProxmoxLXC) VolumeTarCmd(ctx context.Context, workloadID, name string) (*exec.Cmd, error) {
	vmid, ok := o.state.get(workloadID)
	if !ok {
		return nil, fmt.Errorf("no LXC for workload %s", workloadID)
	}
	host, err := safeVolumeHostPath(name)
	if err != nil {
		return nil, err
	}
	return pct(ctx, "exec", vmid, "--", "tar", "czf", "-", "-C", host, "."), nil
}

func (o *ProxmoxLXC) VolumeUntarCmd(ctx context.Context, workloadID, name string) (*exec.Cmd, error) {
	vmid, ok := o.state.get(workloadID)
	if !ok {
		return nil, fmt.Errorf("no LXC for workload %s", workloadID)
	}
	host, err := safeVolumeHostPath(name)
	if err != nil {
		return nil, err
	}
	return pct(ctx, "exec", vmid, "--", "sh", "-c",
		"mkdir -p "+host+" && find "+host+" -mindepth 1 -delete && tar xzf - -C "+host), nil
}

func (o *ProxmoxLXC) VolumeWriteFileCmd(ctx context.Context, workloadID, name, fileName string) (*exec.Cmd, error) {
	vmid, ok := o.state.get(workloadID)
	if !ok {
		return nil, fmt.Errorf("no LXC for workload %s", workloadID)
	}
	host, err := safeVolumeHostPath(name)
	if err != nil {
		return nil, err
	}
	if err := validVolumeFileName(fileName); err != nil {
		return nil, err
	}
	return pct(ctx, "exec", vmid, "--", "sh", "-c", "mkdir -p "+host+" && cat > "+host+"/"+fileName), nil
}

func (o *ProxmoxLXC) AppExecCmd(ctx context.Context, workloadID, script string) (*exec.Cmd, error) {
	vmid, ok := o.state.get(workloadID)
	if !ok {
		return nil, fmt.Errorf("no LXC for workload %s", workloadID)
	}
	return o.lxcEngine(ctx, workloadID, vmid).Cmd(ctx, "exec", "-i", lxcAppContainer, "sh", "-c", script), nil
}

// Volume browsing runs as root in the workload's LXC, the same trust as the web terminal.

func (o *ProxmoxLXC) VolumeListCmd(ctx context.Context, workloadID, name, relPath string) (*exec.Cmd, error) {
	vmid, ok := o.state.get(workloadID)
	if !ok {
		return nil, fmt.Errorf("no LXC for workload %s", workloadID)
	}
	host, err := safeVolumeHostPath(name)
	if err != nil {
		return nil, err
	}
	target := host
	if relPath != "" {
		target += "/" + relPath
	}
	return pct(ctx, "exec", vmid, "--", "find", target, "-maxdepth", "1", "-mindepth", "1",
		"-exec", "stat", "-c", "%n|%s|%Y|%F", "{}", "+"), nil
}

func (o *ProxmoxLXC) VolumeReadCmd(ctx context.Context, workloadID, name, relPath string) (*exec.Cmd, error) {
	vmid, ok := o.state.get(workloadID)
	if !ok {
		return nil, fmt.Errorf("no LXC for workload %s", workloadID)
	}
	host, err := safeVolumeHostPath(name)
	if err != nil {
		return nil, err
	}
	return pct(ctx, "exec", vmid, "--", "cat", host+"/"+relPath), nil
}

// LinkAddr returns the LXC's current IP, which consumers re-resolve on their next deploy.
func (o *ProxmoxLXC) LinkAddr(ctx context.Context, _, targetWorkloadID string, _ EngineKind, port uint32) (string, error) {
	vmid, ok := o.state.get(targetWorkloadID)
	if !ok {
		return "", fmt.Errorf("no LXC for link target %s", targetWorkloadID)
	}
	ip, err := o.lxcIP(ctx, vmid)
	if err != nil {
		return "", fmt.Errorf("link target %s has no IP: %w", targetWorkloadID, err)
	}
	return fmt.Sprintf("http://%s:%d", ip, port), nil
}

func (o *ProxmoxLXC) DialAddr(ctx context.Context, workloadID string, port uint32) (string, error) {
	vmid, ok := o.state.get(workloadID)
	if !ok {
		return "", fmt.Errorf("no LXC for workload %s", workloadID)
	}
	ip, err := o.lxcIP(ctx, vmid)
	if err != nil {
		return "", fmt.Errorf("workload %s has no IP: %w", workloadID, err)
	}
	return net.JoinHostPort(ip, strconv.FormatUint(uint64(port), 10)), nil
}

func (o *ProxmoxLXC) waitForIP(ctx context.Context, vmid string) (string, error) {
	deadline := time.Now().Add(90 * time.Second)
	backoff := 250 * time.Millisecond
	for {
		if ip, err := o.lxcIP(ctx, vmid); err == nil && ip != "" {
			return ip, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("timed out waiting for LXC %s IP", vmid)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(backoff):
		}
		backoff = growBackoff(backoff, 2*time.Second)
	}
}

// lxcIP reads eth0 specifically: `hostname -I` may return an internal Docker bridge.
func (o *ProxmoxLXC) lxcIP(ctx context.Context, vmid string) (string, error) {
	out, err := pct(ctx, "exec", vmid, "--", "ip", "-4", "-o", "addr", "show", "eth0").Output()
	if err != nil {
		return "", err
	}
	m := reEth0Inet.FindSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("no IP yet")
	}
	return string(m[1]), nil
}

var reEth0Inet = regexp.MustCompile(`inet\s+(\d+\.\d+\.\d+\.\d+)/`)

func nextVmid(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "pvesh", "get", "/cluster/nextid").Output()
	if err != nil {
		return "", fmt.Errorf("allocate vmid: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func pct(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "pct", args...)
}

func shortID(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
