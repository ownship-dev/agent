package executors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
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
	// VM workloads, base templates and retired guests; defaults to vm-state.json beside StatePath.
	VMStatePath string
	// Where the owner puts VM cloud images; empty means DefaultVMImageDir.
	VMImageDir string
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
	vms     *vmState
	vm      *vmGuest
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
	// Serializes template imports per image id.
	baseLocks *keyedLocks

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
	if cfg.VMStatePath == "" {
		cfg.VMStatePath = filepath.Join(filepath.Dir(cfg.StatePath), "vm-state.json")
	}
	vms, err := loadVMState(cfg.VMStatePath)
	if err != nil {
		return nil, fmt.Errorf("load vm state: %w", err)
	}
	o := &ProxmoxLXC{cfg: cfg, state: state, vms: vms, createLocks: newKeyedLocks(), baseLocks: newKeyedLocks()}
	o.vm = newVMGuest(o)
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
	ref, ok := o.guestFor(workloadID)
	return ok && ref.engine(ctx, e).Cmd(ctx, "container", "inspect", "--", lxcAppContainer).Run() == nil
}

func (o *ProxmoxLXC) guestEngine(ctx context.Context, workloadID string, ref guestRef) *Engine {
	return ref.engine(ctx, o.workloadEngine(ctx, workloadID))
}

func (o *ProxmoxLXC) workloadEngine(ctx context.Context, workloadID string) *Engine {
	if o.engines == nil {
		return NewDockerEngine()
	}
	return o.engines.For(ctx, workloadID)
}

// guestFor resolves a workload to its tracked guest. A workload in both state files is
// ambiguous, so it resolves to nothing rather than to either guest.
func (o *ProxmoxLXC) guestFor(workloadID string) (guestRef, bool) {
	vmid, inLXC := o.state.get(workloadID)
	w, inVM := o.vmWorkload(workloadID)
	switch {
	case inLXC && inVM:
		return guestRef{}, false
	case inVM:
		return guestRef{o.guestOf(GuestVM), w.Vmid}, true
	case inLXC:
		return guestRef{lxcGuest{}, vmid}, true
	}
	return guestRef{}, false
}

func (o *ProxmoxLXC) vmWorkload(workloadID string) (vmWorkload, bool) {
	if o.vms == nil {
		return vmWorkload{}, false
	}
	return o.vms.get(workloadID)
}

// guestConflict refuses a workload both state files claim; only a person can say which is real.
func (o *ProxmoxLXC) guestConflict(workloadID string) error {
	_, inLXC := o.state.get(workloadID)
	if _, inVM := o.vmWorkload(workloadID); inLXC && inVM {
		return fmt.Errorf("workload %s is tracked as both an LXC and a VM; remove the wrong entry from the agent state", workloadID)
	}
	return nil
}

// guestOf is the strategy for a kind.
func (o *ProxmoxLXC) guestOf(kind GuestKind) guest {
	if kind == GuestVM && o.vm != nil {
		return o.vm
	}
	if kind == GuestVM {
		return newVMGuest(o)
	}
	return lxcGuest{}
}

// trackedGuests is every workload's guest; ambiguous ones are left out.
func (o *ProxmoxLXC) trackedGuests() map[string]guestRef {
	out := map[string]guestRef{}
	for id := range o.state.snapshot() {
		if ref, ok := o.guestFor(id); ok {
			out[id] = ref
		}
	}
	if o.vms != nil {
		for id := range o.vms.snapshot() {
			if ref, ok := o.guestFor(id); ok {
				out[id] = ref
			}
		}
	}
	return out
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
	spec.GuestKind = normalizeGuestKind(spec.GuestKind)
	if spec.GuestKind == GuestVM {
		if err := o.vmRefusal(ctx, spec); err != nil {
			return Target{}, err
		}
	}
	target, err := o.run(ctx, kind, spec)
	if err != nil {
		return Target{}, err
	}
	_ = o.engines.Set(spec.WorkloadID, kind)
	_ = o.engines.SetAsleep(spec.WorkloadID, false)
	return target, nil
}

// vmRefusal turns away what a VM can't do before anything is created.
func (o *ProxmoxLXC) vmRefusal(ctx context.Context, spec RunSpec) error {
	switch {
	case spec.BaseTemplate != "" || spec.TemplateVmid != "":
		return errors.New("LXC templates don't apply to a VM; it boots from a VM image")
	case spec.Resources.GetGpu():
		return errors.New("GPU passthrough is not available for VMs yet")
	case spec.Resources.GetMemoryMb() > 0 && spec.Resources.GetMemoryMb() < vmMinMemoryMB:
		return fmt.Errorf("a VM needs at least %d MB of memory", vmMinMemoryMB)
	}
	if local := o.localNodeName(ctx); spec.TargetNode != "" && spec.TargetNode != local {
		return fmt.Errorf("VMs are created on this agent's node (%s), not on %s", local, spec.TargetNode)
	}
	if reason := o.vmHostBlockedReason(ctx); reason != "" {
		return errors.New(reason)
	}
	return nil
}

func (o *ProxmoxLXC) run(ctx context.Context, kind EngineKind, spec RunSpec) (Target, error) {
	ref, err := o.ensureGuest(ctx, spec)
	if err != nil {
		return Target{}, err
	}

	// Re-applied every deploy, since a reused LXC would otherwise keep the old password.
	o.applyRootPassword(ctx, ref, spec.LxcPassword, spec.LogLine)
	// Same for GPU passthrough.
	hostGPU := detectGPUs(ctx, "/")
	if err := ref.g.ApplyGPU(ctx, o, ref.vmid, spec.Resources.GetGpu(), hostGPU, spec.LogLine); err != nil {
		return Target{}, err
	}

	// Golden templates usually ship Docker; base-OS builds install it.
	eng := ref.engine(ctx, o.engines.byKind(kind))
	if err := o.ensureEngine(ctx, eng, ref, spec.LogLine); err != nil {
		return Target{}, err
	}
	// Volumes are guest dirs both engines bind, so a switch only retires the old engine's containers.
	if rec, ok := o.engines.Recorded(spec.WorkloadID); ok && normalizeKind(rec) != kind {
		old := ref.engine(ctx, o.engines.byKind(rec))
		_ = old.Raw(ctx, "sh", "-c", lxcRunPrepScript(old)).Run()
		spec.LogLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "moved from "+old.Name()+" to "+eng.Name()+" inside "+ref.label())
	}

	// Short-lived registry credentials for the build and the pull, removed afterwards.
	if spec.RegistryAuth != nil {
		_ = ref.cmd(ctx, "mkdir", "-p", "-m", "0700", "/root/.docker").Run()
		if err := o.writeEnvFile(ctx, ref, "/root/.docker/config.json", dockerConfigJSON(spec.RegistryAuth)); err != nil {
			return Target{}, fmt.Errorf("write registry auth: %w", err)
		}
		defer func() {
			_ = ref.cmd(context.Background(), "rm", "-f", "/root/.docker/config.json").Run()
		}()
		spec.LogLine(agentv1.LogStream_LOG_STREAM_SYSTEM_OK, "authenticated to registry "+spec.RegistryAuth.GetHost())
	}

	if spec.Compose != nil {
		return o.runComposeInLXC(ctx, eng, ref, spec, hostGPU)
	}

	// An image with no repo (managed DB, rollback) runs as-is.
	if spec.Repo.GetCloneUrl() != "" {
		if err := o.buildInLXC(ctx, eng, ref, spec.Image, spec.Repo, spec.Build, spec.Cache, spec.LogLine); err != nil {
			return Target{}, err
		}
	}

	// 0600 env file inside the LXC keeps secrets off argv.
	const envPath = "/run/ownship-app.env"
	if err := o.writeEnvFile(ctx, ref, envPath, envFileContent(spec.Env, spec.Port)); err != nil {
		return Target{}, err
	}

	// Re-pull moved tags, but keep the current image if the registry is down.
	if spec.PullImage {
		if out, err := eng.Cmd(ctx, "pull", "--", spec.Image).CombinedOutput(); err != nil {
			spec.LogLine(agentv1.LogStream_LOG_STREAM_SYSTEM_WARN,
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
			spec.LogLine(agentv1.LogStream_LOG_STREAM_SYSTEM_WARN,
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
		_ = ref.cmd(ctx, "mkdir", "-p", host).Run()
		_ = ref.cmd(ctx, "chmod", volMode, host).Run()
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
		_ = ref.cmd(ctx, "mkdir", "-p", "-m", "0700", lxcCaddyfileDir).Run()
		if err := o.writeEnvFile(ctx, ref, lxcCaddyfilePath, caddyfile); err != nil {
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

	ip, err := ref.g.IP(ctx, ref.vmid)
	if err != nil {
		return Target{}, err
	}
	// Detached ctx so pruning outlives the deploy.
	go o.pruneImages(context.Background(), eng, spec.WorkloadID, 3)
	spec.LogLine(agentv1.LogStream_LOG_STREAM_SYSTEM_OK, fmt.Sprintf("running in %s at %s:%d", ref.label(), ip, spec.Port))
	return Target{URL: fmt.Sprintf("http://%s:%d", ip, spec.Port), LxcID: ref.vmid, GuestKind: ref.g.Kind()}, nil
}

// runComposeInLXC runs a sanitized compose project in the LXC, with named volumes on the
// same dirs single-image apps use so backups keep working.
func (o *ProxmoxLXC) runComposeInLXC(ctx context.Context, eng *Engine, ref guestRef, spec RunSpec, hostGPU gpuDevices) (Target, error) {
	if spec.BasicAuth != nil {
		return Target{}, errors.New("basic auth is not available for compose projects")
	}
	if err := o.ensureComposeInLXC(ctx, eng, ref, spec.LogLine); err != nil {
		return Target{}, err
	}
	const repoDir = "/root/app"
	hasRepo := spec.Repo.GetCloneUrl() != ""
	if hasRepo {
		if err := gitSync(ctx, lxcGit{o, ref}, spec.Repo, repoDir, spec.Cache, spec.LogLine); err != nil {
			return Target{}, err
		}
	}
	// Kept outside the checkout, with every repo path pinned inside it.
	const preparedDir = "/root/ownship-compose"
	root := ""
	if hasRepo {
		root = repoDir
	}
	src, projectDir, resolveBuild, err := composeSource(lxcFS{ctx, ref}, root, spec.Compose)
	if err != nil {
		return Target{}, err
	}
	if projectDir == "" {
		projectDir = preparedDir
	}

	const envPath = "/run/ownship-app.env"
	if err := o.writeEnvFile(ctx, ref, envPath, envFileContent(spec.Env, spec.Port)); err != nil {
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
		_ = ref.cmd(ctx, "mkdir", "-p", d).Run()
	}
	_ = ref.cmd(ctx, "mkdir", "-p", "-m", "0700", preparedDir).Run()
	_ = eng.Cmd(ctx, "network", "create", linkNetwork).Run()
	// Don't let compose replace a single-image container of the same name.
	if out, _ := eng.Cmd(ctx, "inspect", "--format",
		`{{index .Config.Labels "com.docker.compose.project"}}`, lxcAppContainer).Output(); strings.TrimSpace(string(out)) != composeProject(spec.WorkloadID) {
		_ = eng.Cmd(ctx, "rm", "-f", lxcAppContainer, lxcAppContainer+authSidecarSuffix).Run()
	}
	composePath := preparedDir + "/" + preparedComposeName
	if err := o.writeEnvFile(ctx, ref, composePath, string(file)); err != nil {
		return Target{}, err
	}
	spec.LogLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "starting compose project in "+ref.label())
	up := eng.ComposeCmd(ctx, composeUpArgs(spec.WorkloadID, composePath, projectDir)[1:]...)
	if err := runStreamingCmd(up, spec.LogLine); err != nil {
		return Target{}, fmt.Errorf("docker compose up in lxc: %w", err)
	}
	ip, err := ref.g.IP(ctx, ref.vmid)
	if err != nil {
		return Target{}, err
	}
	spec.LogLine(agentv1.LogStream_LOG_STREAM_SYSTEM_OK, fmt.Sprintf("compose project up in %s at %s:%d", ref.label(), ip, spec.Port))
	return Target{URL: fmt.Sprintf("http://%s:%d", ip, spec.Port), LxcID: ref.vmid}, nil
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

func (o *ProxmoxLXC) ensureComposeInLXC(ctx context.Context, eng *Engine, ref guestRef, logLine LogFunc) error {
	if eng.ComposeCmd(ctx, "version").Run() == nil {
		return nil
	}
	logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "installing docker compose for "+eng.Name()+" in "+ref.label())
	if err := runStreamingCmd(ref.cmd(ctx, "sh", "-c", composeInstallScript(eng)), logLine); err != nil {
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

// List reports tracked workloads whose guest still exists with the recorded type. Only
// ensureGuest and Destroy forget a vmid, once they've seen it gone.
func (o *ProxmoxLXC) List(ctx context.Context) (Inventory, error) {
	tracked := o.trackedGuests()
	all, err := clusterGuests(ctx)
	if err != nil {
		ids := make([]string, 0, len(tracked))
		for id := range tracked {
			ids = append(ids, id)
		}
		return Inventory{IDs: ids}, err
	}
	inv := Inventory{Complete: true}
	trackedVmids := o.vms.asideVmids()
	for id, ref := range tracked {
		trackedVmids[ref.vmid] = true
		if g, ok := all[ref.vmid]; ok && g.is(ref.g.Kind()) {
			inv.IDs = append(inv.IDs, id)
		}
	}
	// Before Adopt, an untracked ownship guest may be a mapping a reinstall lost. After it, a
	// stray one (a failed create, another machine on the cluster) must not mute detection.
	if !o.adopted.Load() {
		for vmid, g := range all {
			if g.Tagged && !trackedVmids[vmid] {
				inv.Complete = false
				break
			}
		}
	}
	sort.Strings(inv.IDs)
	return inv, nil
}

// Adopt tracks workload->guest pairs the control plane recorded, for a state file that was
// lost. Only an existing ownship-tagged guest of the recorded type that no other workload
// holds is taken.
func (o *ProxmoxLXC) Adopt(ctx context.Context, pairs map[string]string) ([]string, error) {
	return o.AdoptKinds(ctx, pairs, nil)
}

// AdoptKinds is Adopt with each pair's guest kind (absent is an LXC).
func (o *ProxmoxLXC) AdoptKinds(ctx context.Context, pairs map[string]string, kinds map[string]GuestKind) ([]string, error) {
	all, err := clusterGuests(ctx)
	if err != nil {
		return nil, err
	}
	held := o.vms.asideVmids()
	for _, ref := range o.trackedGuests() {
		held[ref.vmid] = true
	}
	// Two rows naming one vmid can't both be right; picking one would hand it the other's guest.
	claims := map[string]int{}
	for _, vmid := range pairs {
		claims[vmid]++
	}
	var adopted []string
	for id, vmid := range pairs {
		kind := normalizeGuestKind(kinds[id])
		g, live := all[vmid]
		if _, ok := o.guestFor(id); ok || held[vmid] || claims[vmid] > 1 || !live || !g.Tagged || !g.is(kind) {
			continue
		}
		if err := o.track(id, guestRef{o.guestOf(kind), vmid}); err != nil {
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
	ref, ok := o.guestFor(workloadID)
	if !ok {
		return fmt.Errorf("no LXC or VM for workload %s", workloadID)
	}
	eng := o.guestEngine(ctx, workloadID, ref)
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
	ref, ok := o.guestFor(workloadID)
	if !ok {
		return nil
	}
	_ = o.guestEngine(ctx, workloadID, ref).Cmd(ctx, "stop", lxcAppContainer).Run()
	// Otherwise Podman's restart service starts it on the next boot; Resume re-applies this.
	_ = o.engines.SetAsleep(workloadID, true)
	return ref.g.Shutdown(ctx, ref.vmid)
}

func (o *ProxmoxLXC) Start(ctx context.Context, workloadID string) error {
	ref, ok := o.guestFor(workloadID)
	if !ok {
		return fmt.Errorf("no LXC or VM for workload %s", workloadID)
	}
	if err := ref.g.Start(ctx, ref.vmid); err != nil {
		return err
	}
	if _, err := o.waitForIP(ctx, ref); err != nil {
		return err
	}
	_ = o.engines.SetAsleep(workloadID, false)
	return o.guestEngine(ctx, workloadID, ref).Cmd(ctx, "start", lxcAppContainer).Run()
}

// NetworkUpdate changes net0/nameserver in place and restarts the LXC.
func (o *ProxmoxLXC) NetworkUpdate(ctx context.Context, workloadID string, network *agentv1.NetworkConfig) error {
	ref, ok := o.guestFor(workloadID)
	if !ok {
		return fmt.Errorf("no LXC or VM for workload %s", workloadID)
	}
	if err := ref.g.SetNetwork(ctx, ref.vmid, netSpec{Bridge: o.cfg.Bridge, Network: network}); err != nil {
		return err
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
	refs := o.trackedGuests()
	out := make(map[string]StatsSample, len(refs))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for wl, ref := range refs {
		wg.Add(1)
		sem <- struct{}{}
		go func(wl string, ref guestRef) {
			defer wg.Done()
			defer func() { <-sem }()
			s, err := o.statsOne(ctx, o.guestEngine(ctx, wl, ref))
			if err != nil {
				return
			}
			mu.Lock()
			out[wl] = s
			mu.Unlock()
		}(wl, ref)
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
	ref, ok := o.guestFor(workloadID)
	if !ok {
		return nil, fmt.Errorf("no LXC for workload %s", workloadID)
	}
	args := []string{"exec", "-i"}
	if tty {
		args = append(args, "-t")
	}
	args = append(args, lxcAppContainer)
	p, err := ref.g.ExecPrefix(ctx, ref.vmid, tty)
	if err != nil {
		return nil, err
	}
	return o.workloadEngine(ctx, workloadID).InGuest(p).ExecArgv(append(args, inner...)...), nil
}

// LxcExecArgs opens a shell in the LXC rather than the app container.
func (o *ProxmoxLXC) LxcExecArgs(ctx context.Context, workloadID string, inner []string, tty bool) ([]string, error) {
	ref, ok := o.guestFor(workloadID)
	if !ok {
		return nil, fmt.Errorf("no LXC for workload %s", workloadID)
	}
	return ref.argv(ctx, tty, inner...), nil
}

// Destroy removes the workload's LXC.
func (o *ProxmoxLXC) Destroy(ctx context.Context, workloadID string, keepVolumes bool) error {
	if !keepVolumes {
		if err := o.destroyAside(ctx, workloadID); err != nil {
			return err
		}
	}
	ref, ok := o.guestFor(workloadID)
	if !ok {
		return nil
	}
	// Deleted outside Ownship: only the mapping is left.
	if ref.g.Gone(ctx, ref.vmid) {
		return o.untrack(workloadID, ref)
	}
	if keepVolumes {
		// Volumes live on the guest's disk, so keeping them means keeping the guest, stopped.
		_ = o.guestEngine(ctx, workloadID, ref).Cmd(ctx, "rm", "-f", lxcAppContainer).Run()
		_ = ref.g.Stop(ctx, ref.vmid)
		return nil
	}
	if err := ref.g.Destroy(ctx, ref.vmid); err != nil {
		return err
	}
	_ = o.engines.Remove(workloadID)
	return o.untrack(workloadID, ref)
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

// ensureGuest returns the workload's guest, creating one of the requested kind on first deploy.
func (o *ProxmoxLXC) ensureGuest(ctx context.Context, spec RunSpec) (guestRef, error) {
	defer o.createLocks.lock(spec.WorkloadID)()

	kind := normalizeGuestKind(spec.GuestKind)
	if err := o.guestConflict(spec.WorkloadID); err != nil {
		return guestRef{}, err
	}
	if ref, ok := o.guestFor(spec.WorkloadID); ok {
		// A prepared convert target becomes the workload's guest; the old one is kept aside.
		if ref.g.Kind() != kind {
			if promoted, ok, err := o.promotePending(ctx, spec.WorkloadID, ref, kind); err != nil || ok {
				if err != nil {
					return guestRef{}, err
				}
				spec.LogLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "converted: now running in "+promoted.label()+", "+ref.label()+" kept until the convert commits")
				ref = promoted
			}
		}
		// Never silently swap the guest under existing data; that is a convert.
		if ref.g.Kind() != kind {
			return guestRef{}, fmt.Errorf("this workload runs in %s; moving it to %s is a conversion, not a deploy",
				ref.label(), guestKindNoun(kind))
		}
		if !ref.g.Gone(ctx, ref.vmid) {
			// Redeploys need the same resize, or new cores/memory limits are ignored.
			if err := o.sizeAndStart(ctx, ref, spec.WorkloadID, spec.Resources); err != nil {
				return guestRef{}, err
			}
			return ref, nil
		}
		spec.LogLine(agentv1.LogStream_LOG_STREAM_SYSTEM_WARN, ref.label()+" no longer exists in Proxmox; creating a new one")
		if err := o.untrack(spec.WorkloadID, ref); err != nil {
			return guestRef{}, err
		}
	}
	if kind == GuestVM {
		return o.createVM(ctx, spec)
	}
	return o.createLXC(ctx, spec.WorkloadID, spec.BaseTemplate, spec.TemplateVmid, spec.BaseOS, spec.Resources,
		spec.TargetNode, spec.Hostname, spec.Network, spec.LogLine)
}

func guestKindNoun(k GuestKind) string {
	if k == GuestVM {
		return "a VM"
	}
	return "an LXC"
}

// createVM clones the image's template, then configures and boots it outside the vmid lock,
// since importing a template and first boot are both slow.
func (o *ProxmoxLXC) createVM(ctx context.Context, spec RunSpec) (guestRef, error) {
	if o.vm == nil {
		o.vm = newVMGuest(o)
	}
	img, err := findVMImage(ctx, o.vmImageDir(), spec.VmImage)
	if err != nil {
		return guestRef{}, err
	}
	base, err := o.ensureVMBase(ctx, img, spec.LogLine)
	if err != nil {
		return guestRef{}, err
	}
	authorized, _, err := ensureVMKey(o.vm.paths)
	if err != nil {
		return guestRef{}, fmt.Errorf("VM SSH key: %w", err)
	}
	req := createReq{WorkloadID: spec.WorkloadID, Hostname: lxcHostname(spec.Hostname, spec.WorkloadID),
		Cores: "1", MemoryMB: strconv.Itoa(vmDefaultMemoryMB),
		Net:     netSpec{Bridge: o.cfg.Bridge, Network: spec.Network},
		Storage: o.cfg.Storage, RootfsGB: o.cfg.RootfsGB,
		VMImage: spec.VmImage, BaseVmid: base, AuthorizedKey: authorized, LogLine: spec.LogLine}
	if v := spec.Resources.GetVcpu(); v > 0 {
		req.Cores = strconv.FormatUint(uint64(v), 10)
	}
	if m := spec.Resources.GetMemoryMb(); m > 0 {
		req.MemoryMB = strconv.FormatUint(uint64(m), 10)
	}

	o.vmidMu.Lock()
	vmid, err := nextVmid(ctx)
	if err != nil {
		o.vmidMu.Unlock()
		return guestRef{}, err
	}
	req.Vmid = vmid
	createErr := o.vm.Create(ctx, req)
	o.vmidMu.Unlock() // vmid is claimed once the clone ran
	if createErr != nil {
		return guestRef{}, createErr
	}
	ref := guestRef{o.vm, vmid}
	// A VM that never booted would be reused broken on every redeploy; remove what we made.
	fail := func(e error) (guestRef, error) {
		o.vm.discard(vmid)
		_ = o.untrackIn(ctx, spec.WorkloadID, ref)
		return guestRef{}, e
	}
	if err := o.vm.configure(ctx, req); err != nil {
		return fail(err)
	}
	// Tracked before boot so Ready can record the pinned host key against it.
	if err := o.trackIn(ctx, spec.WorkloadID, ref); err != nil {
		return fail(err)
	}
	if err := o.startAndTrack(ctx, ref, spec.WorkloadID); err != nil {
		return fail(err)
	}
	return ref, nil
}

// createLXC creates the project's LXC from templateVmid, else baseTemplate, baseOS or a fallback.
func (o *ProxmoxLXC) createLXC(ctx context.Context, workloadID, baseTemplate, templateVmid, baseOS string, res *agentv1.Resources, targetNode, requestedHostname string, network *agentv1.NetworkConfig, logLine LogFunc) (guestRef, error) {
	// These reach pct argv: reject anything that could pass as a flag.
	if !validPctArg(templateVmid, reVmid) {
		return guestRef{}, fmt.Errorf("invalid template VMID %q", templateVmid)
	}
	if !validPctArg(targetNode, reNode) {
		return guestRef{}, fmt.Errorf("invalid target node %q", targetNode)
	}
	if !validPctArg(baseTemplate, reVolid) {
		return guestRef{}, fmt.Errorf("invalid base template %q", baseTemplate)
	}
	if !validPctArg(baseOS, reBaseOS) {
		return guestRef{}, fmt.Errorf("invalid base OS %q", baseOS)
	}

	g := lxcGuest{}
	o.vmidMu.Lock()
	newVmid, err := nextVmid(ctx)
	if err != nil {
		o.vmidMu.Unlock()
		return guestRef{}, err
	}
	ref := guestRef{g, newVmid}
	req := createReq{Vmid: newVmid, WorkloadID: workloadID, Hostname: lxcHostname(requestedHostname, workloadID),
		TargetNode: targetNode, LogLine: logLine}

	if templateVmid != "" {
		req.CloneFrom = templateVmid
		cloneErr := g.Create(ctx, req)
		o.vmidMu.Unlock() // vmid is claimed once clone/create runs
		if cloneErr != nil {
			return guestRef{}, cloneErr
		}
		if err := o.sizeAndStart(ctx, ref, workloadID, res); err != nil {
			return guestRef{}, err
		}
		return ref, nil
	}

	// pct create has no --target, so refuse rather than create on the wrong node.
	if local := o.localNodeName(ctx); targetNode != "" && targetNode != local {
		o.vmidMu.Unlock()
		return guestRef{}, fmt.Errorf(
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
			return guestRef{}, err
		}
	}
	req.Cores = "1"
	if v := res.GetVcpu(); v > 0 {
		req.Cores = strconv.FormatUint(uint64(v), 10)
	}
	req.MemoryMB = "512"
	if m := res.GetMemoryMb(); m > 0 {
		req.MemoryMB = strconv.FormatUint(uint64(m), 10)
	}
	req.BaseVolid = baseTemplate
	req.Net = netSpec{Bridge: o.cfg.Bridge, Network: network}
	req.Storage, req.RootfsGB = o.cfg.Storage, o.cfg.RootfsGB
	createErr := g.Create(ctx, req)
	o.vmidMu.Unlock() // vmid is claimed once clone/create runs
	if createErr != nil {
		return guestRef{}, createErr
	}
	if err := o.startAndTrack(ctx, ref, workloadID); err != nil {
		return guestRef{}, err
	}
	return ref, nil
}

// sizeAndStart resizes the guest, starts it and waits for an IP.
func (o *ProxmoxLXC) sizeAndStart(ctx context.Context, ref guestRef, workloadID string, res *agentv1.Resources) error {
	restart, err := ref.g.Resize(ctx, ref.vmid, res)
	if err != nil {
		return err
	}
	if restart && ref.g.Running(ctx, ref.vmid) {
		if err := ref.g.Reboot(ctx, ref.vmid); err != nil {
			return err
		}
	}
	return o.startAndTrack(ctx, ref, workloadID)
}

// startAndTrack starts the guest, records the mapping and waits for an IP.
func (o *ProxmoxLXC) startAndTrack(ctx context.Context, ref guestRef, workloadID string) error {
	// A redeploy reuses a running guest, and start fails on one; it still has to be reachable.
	if !ref.g.Running(ctx, ref.vmid) {
		if err := ref.g.Start(ctx, ref.vmid); err != nil {
			return err
		}
	} else if err := ref.g.Ready(ctx, ref.vmid); err != nil {
		return err
	}
	if err := o.trackIn(ctx, workloadID, ref); err != nil {
		return err
	}
	if _, err := o.waitForIP(ctx, ref); err != nil {
		return err
	}
	return nil
}

// track records the workload's guest in the state file of its kind.
func (o *ProxmoxLXC) track(workloadID string, ref guestRef) error {
	if ref.g.Kind() == GuestVM {
		w, _ := o.vms.get(workloadID)
		if w.Vmid != ref.vmid {
			w = vmWorkload{Vmid: ref.vmid}
		}
		return o.vms.set(workloadID, w)
	}
	return o.state.set(workloadID, ref.vmid)
}

func (o *ProxmoxLXC) untrack(workloadID string, ref guestRef) error {
	if ref.g.Kind() == GuestVM {
		return o.vms.remove(workloadID)
	}
	return o.state.remove(workloadID)
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
func (o *ProxmoxLXC) ensureDocker(ctx context.Context, ref guestRef, logLine LogFunc) error {
	if ref.cmd(ctx, "sh", "-c", "command -v docker >/dev/null 2>&1").Run() == nil {
		return nil
	}
	logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "installing Docker in "+ref.label())
	curlScript := fmt.Sprintf(`command -v curl >/dev/null 2>&1 || { %s
}`, pkgInstallScript("curl"))
	if err := runStreamingCmd(ref.cmd(ctx, "sh", "-c", curlScript), logLine); err != nil {
		return fmt.Errorf("install curl in %s: %w", ref.word(), err)
	}
	script := `set -e
export LC_ALL=C LANG=C DEBIAN_FRONTEND=noninteractive
curl -fsSL https://get.docker.com | sh
(systemctl enable --now docker 2>/dev/null || service docker start 2>/dev/null || true)`
	if err := runStreamingCmd(ref.cmd(ctx, "sh", "-c", script), logLine); err != nil {
		return fmt.Errorf("install docker in %s: %w", ref.word(), err)
	}
	return nil
}

func (o *ProxmoxLXC) ensureEngine(ctx context.Context, eng *Engine, ref guestRef, logLine LogFunc) error {
	if eng.Kind == KindDocker {
		return o.ensureDocker(ctx, ref, logLine)
	}
	if ref.cmd(ctx, "sh", "-c", "command -v podman >/dev/null 2>&1").Run() == nil {
		return nil
	}
	logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "installing Podman in "+ref.label())
	if err := runStreamingCmd(ref.cmd(ctx, "sh", "-c", engineInstallScript(eng.Kind)), logLine); err != nil {
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
func (o *ProxmoxLXC) buildInLXC(ctx context.Context, eng *Engine, ref guestRef, image string, repo *agentv1.RepoSource, build *agentv1.BuildConfig, cache bool, logLine LogFunc) error {
	const dir = "/root/app"
	if err := gitSync(ctx, lxcGit{o, ref}, repo, dir, cache, logLine); err != nil {
		return err
	}

	dockerfile := chooseDockerfile(build.GetType(), func() bool {
		return ref.cmd(ctx, "test", "-f", dir+"/Dockerfile").Run() == nil
	})
	if !dockerfile {
		install := "command -v nixpacks >/dev/null 2>&1 || curl -fsSL https://nixpacks.com/install.sh | bash"
		if err := runStreamingCmd(ref.cmd(ctx, "sh", "-c", install), logLine); err != nil {
			return fmt.Errorf("install nixpacks in lxc: %w", err)
		}
	}
	for _, step := range buildSteps(eng, image, dockerfile) {
		if err := runStreamingCmd(lxcStepCmd(ctx, ref, dir, step), logLine); err != nil {
			return err
		}
	}
	return nil
}

// lxcStepCmd runs a host build step inside the guest from dir; dir and argv stay positional,
// never part of the script.
func lxcStepCmd(ctx context.Context, ref guestRef, dir string, argv []string) *exec.Cmd {
	return ref.cmd(ctx, append([]string{"sh", "-c", `cd "$1" && shift && exec "$@"`, "sh", dir}, argv...)...)
}

// lxcFS reads a checkout inside the guest, resolving symlinks there rather than on the host.
type lxcFS struct {
	ctx context.Context
	ref guestRef
}

func (f lxcFS) realpath(p string) (string, error) {
	out, err := f.ref.cmd(f.ctx, "realpath", "-e", "--", p).Output()
	return strings.TrimSpace(string(out)), err
}

func (f lxcFS) readHead(p string, n int) ([]byte, error) {
	return f.ref.cmd(f.ctx, "head", "-c", strconv.Itoa(n), "--", p).Output()
}

// lxcGit runs git inside a workload's guest. Env can't cross the exec prefix, so a token or
// deploy key goes in a 0600 file there instead, with the same protocol allowlist as the host.
type lxcGit struct {
	o   *ProxmoxLXC
	ref guestRef
}

func (g lxcGit) git(ctx context.Context, args []string, _ []string) *exec.Cmd {
	return g.ref.cmd(ctx, append([]string{"git"}, args...)...)
}

func (g lxcGit) hasCheckout(ctx context.Context, dest string) bool {
	return g.ref.cmd(ctx, "test", "-d", dest+"/.git").Run() == nil
}

func (g lxcGit) removeAll(ctx context.Context, dest string) error {
	_ = g.ref.cmd(ctx, "rm", "-rf", dest).Run()
	return nil
}

func (g lxcGit) auth(ctx context.Context, repo *agentv1.RepoSource, _ string) ([]string, []string, func(), error) {
	g.o.ensureGitInLXC(ctx, g.ref)
	args := gitProtocolArgs()
	var files []string
	cleanup := func() {
		if len(files) > 0 {
			_ = g.ref.cmd(context.Background(), append([]string{"rm", "-f"}, files...)...).Run()
		}
	}
	if token := repo.GetInstallationToken(); token != "" {
		const tokenPath = "/run/ownship-git-token"
		if err := g.o.writeEnvFile(ctx, g.ref, tokenPath, token); err != nil {
			return nil, nil, nil, err
		}
		files = append(files, tokenPath)
		args = append(args, "-c",
			`credential.helper=!f() { echo username=x-access-token; echo "password=$(cat `+tokenPath+`)"; }; f`)
	}
	if key := repo.GetSshPrivateKey(); key != "" {
		g.o.ensureSSHInLXC(ctx, g.ref)
		const keyPath = "/run/ownship-deploy-key"
		// Persistent, so a host key trusted on first use stays pinned.
		const knownHosts = "/root/.ownship-known-hosts"
		if err := g.o.writeEnvFile(ctx, g.ref, keyPath, key+"\n"); err != nil {
			cleanup()
			return nil, nil, nil, err
		}
		files = append(files, keyPath)
		pinned := repo.GetSshKnownHosts() != ""
		if pinned {
			if err := g.o.writeEnvFile(ctx, g.ref, knownHosts, repo.GetSshKnownHosts()+"\n"); err != nil {
				cleanup()
				return nil, nil, nil, err
			}
		}
		sshArgs, err := sshCommandArg(keyPath, knownHosts, pinned)
		if err != nil {
			cleanup()
			return nil, nil, nil, err
		}
		args = append(args, sshArgs...)
	}
	return args, nil, cleanup, nil
}

func (o *ProxmoxLXC) ensureGitInLXC(ctx context.Context, ref guestRef) {
	gitScript := fmt.Sprintf(`command -v git >/dev/null 2>&1 || { %s
}`, pkgInstallScript("git", "ca-certificates"))
	_ = ref.cmd(ctx, "sh", "-c", gitScript).Run()
}

// ensureSSHInLXC: the package is openssh-client on apt, openssh-clients on dnf/yum.
func (o *ProxmoxLXC) ensureSSHInLXC(ctx context.Context, ref guestRef) {
	script := `command -v ssh >/dev/null 2>&1 && exit 0
export LC_ALL=C LANG=C DEBIAN_FRONTEND=noninteractive
if command -v apt-get >/dev/null 2>&1; then apt-get update -y -q && apt-get install -y -q openssh-client
elif command -v dnf >/dev/null 2>&1; then dnf install -y openssh-clients
elif command -v yum >/dev/null 2>&1; then yum install -y openssh-clients
fi`
	_ = ref.cmd(ctx, "sh", "-c", script).Run()
}

// applyRootPassword pipes the password to chpasswd to keep it off argv. Best-effort:
// without chpasswd only console access is lost.
func (o *ProxmoxLXC) applyRootPassword(ctx context.Context, ref guestRef, password string, logLine LogFunc) {
	if password == "" || strings.ContainsAny(password, ":\n") {
		return
	}
	cmd := ref.cmd(ctx, "chpasswd")
	cmd.Stdin = strings.NewReader("root:" + password + "\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		logLine(agentv1.LogStream_LOG_STREAM_SYSTEM_WARN, "could not set the LXC root password: "+strings.TrimSpace(string(out)))
	}
}

func (o *ProxmoxLXC) writeEnvFile(ctx context.Context, ref guestRef, path, content string) error {
	cmd := ref.cmd(ctx, "sh", "-c", "umask 077; cat > "+path)
	cmd.Stdin = strings.NewReader(content)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("write env file in lxc: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// Backups stream through pct exec's stdio; paths come only from safeVolumeHostPath.

func (o *ProxmoxLXC) VolumeTarCmd(ctx context.Context, workloadID, name string) (*exec.Cmd, error) {
	ref, ok := o.guestFor(workloadID)
	if !ok {
		return nil, fmt.Errorf("no LXC for workload %s", workloadID)
	}
	host, err := safeVolumeHostPath(name)
	if err != nil {
		return nil, err
	}
	return ref.cmd(ctx, "tar", "czf", "-", "-C", host, "."), nil
}

func (o *ProxmoxLXC) VolumeUntarCmd(ctx context.Context, workloadID, name string) (*exec.Cmd, error) {
	ref, ok := o.guestFor(workloadID)
	if !ok {
		return nil, fmt.Errorf("no LXC for workload %s", workloadID)
	}
	host, err := safeVolumeHostPath(name)
	if err != nil {
		return nil, err
	}
	return ref.cmd(ctx, "sh", "-c",
		"mkdir -p "+host+" && find "+host+" -mindepth 1 -delete && tar xzf - -C "+host), nil
}

func (o *ProxmoxLXC) VolumeWriteFileCmd(ctx context.Context, workloadID, name, fileName string) (*exec.Cmd, error) {
	ref, ok := o.guestFor(workloadID)
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
	return ref.cmd(ctx, "sh", "-c", "mkdir -p "+host+" && cat > "+host+"/"+fileName), nil
}

func (o *ProxmoxLXC) AppExecCmd(ctx context.Context, workloadID, script string) (*exec.Cmd, error) {
	ref, ok := o.guestFor(workloadID)
	if !ok {
		return nil, fmt.Errorf("no LXC for workload %s", workloadID)
	}
	return o.guestEngine(ctx, workloadID, ref).Cmd(ctx, "exec", "-i", lxcAppContainer, "sh", "-c", script), nil
}

// Volume browsing runs as root in the workload's LXC, the same trust as the web terminal.

func (o *ProxmoxLXC) VolumeListCmd(ctx context.Context, workloadID, name, relPath string) (*exec.Cmd, error) {
	ref, ok := o.guestFor(workloadID)
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
	return ref.cmd(ctx, "find", target, "-maxdepth", "1", "-mindepth", "1",
		"-exec", "stat", "-c", "%n|%s|%Y|%F", "{}", "+"), nil
}

func (o *ProxmoxLXC) VolumeReadCmd(ctx context.Context, workloadID, name, relPath string) (*exec.Cmd, error) {
	ref, ok := o.guestFor(workloadID)
	if !ok {
		return nil, fmt.Errorf("no LXC for workload %s", workloadID)
	}
	host, err := safeVolumeHostPath(name)
	if err != nil {
		return nil, err
	}
	return ref.cmd(ctx, "cat", host+"/"+relPath), nil
}

// LinkAddr returns the LXC's current IP, which consumers re-resolve on their next deploy.
func (o *ProxmoxLXC) LinkAddr(ctx context.Context, _, targetWorkloadID string, _ EngineKind, port uint32) (string, error) {
	ref, ok := o.guestFor(targetWorkloadID)
	if !ok {
		return "", fmt.Errorf("no LXC for link target %s", targetWorkloadID)
	}
	ip, err := ref.g.IP(ctx, ref.vmid)
	if err != nil {
		return "", fmt.Errorf("link target %s has no IP: %w", targetWorkloadID, err)
	}
	return fmt.Sprintf("http://%s:%d", ip, port), nil
}

func (o *ProxmoxLXC) DialAddr(ctx context.Context, workloadID string, port uint32) (string, error) {
	ref, ok := o.guestFor(workloadID)
	if !ok {
		return "", fmt.Errorf("no LXC for workload %s", workloadID)
	}
	ip, err := ref.g.IP(ctx, ref.vmid)
	if err != nil {
		return "", fmt.Errorf("workload %s has no IP: %w", workloadID, err)
	}
	return net.JoinHostPort(ip, strconv.FormatUint(uint64(port), 10)), nil
}

func (o *ProxmoxLXC) waitForIP(ctx context.Context, ref guestRef) (string, error) {
	deadline := time.Now().Add(90 * time.Second)
	backoff := 250 * time.Millisecond
	for {
		if ip, err := ref.g.IP(ctx, ref.vmid); err == nil && ip != "" {
			return ip, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("timed out waiting for %s IP", ref.label())
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(backoff):
		}
		backoff = growBackoff(backoff, 2*time.Second)
	}
}

var reEth0Inet = regexp.MustCompile(`inet\s+(\d+\.\d+\.\d+\.\d+)/`)

var nextVmid = func(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "pvesh", "get", "/cluster/nextid").Output()
	if err != nil {
		return "", fmt.Errorf("allocate vmid: %w", err)
	}
	vmid := strings.TrimSpace(string(out))
	if !reVmid.MatchString(vmid) {
		return "", fmt.Errorf("allocate vmid: unexpected reply %q", vmid)
	}
	return vmid, nil
}

func shortID(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
