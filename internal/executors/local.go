package executors

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ownship/agent/internal/compose"
	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
	"github.com/ownship/agent/internal/linkbridge"
)

// LocalDocker runs workloads on the agent host's own engines (no Proxmox).
type LocalDocker struct {
	// Serializes Run/Destroy per workload, so a racing command can't remove a container just started.
	runLocks *keyedLocks
	// nil only in tests that never touch an engine.
	state *engineState
	// Forwards private links across engines; nil until EnableLinkBridge.
	bridge   *linkbridge.Bridge
	subnetMu sync.Mutex
	subnets  map[string]*net.IPNet
	bcache   bridgeCache
}

func NewLocalDocker(engineStatePath string, engines []*Engine) *LocalDocker {
	state := loadEngineState(engineStatePath, engines, probeContainer)
	state.detect = func(k EngineKind) *Engine {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return DetectEngine(ctx, k)
	}
	return &LocalDocker{runLocks: newKeyedLocks(), state: state}
}

// runEngineCmd is swapped in tests to fake the CLIs.
var runEngineCmd = func(cmd *exec.Cmd) ([]byte, error) { return cmd.Output() }

func probeContainer(ctx context.Context, e *Engine, workloadID string) bool {
	_, err := runEngineCmd(e.Cmd(ctx, "container", "inspect", "--", "ownship-"+workloadID))
	return err == nil
}

// engineErr prefers the CLI's stderr over the exec error.
func engineErr(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		return strings.TrimSpace(string(ee.Stderr))
	}
	return err.Error()
}

func (o *LocalDocker) EngineFor(k EngineKind) (*Engine, bool) { return o.state.Engine(k) }

// Engines re-detects on every Hello, so a newly installed Podman is reported without a restart.
func (o *LocalDocker) Engines(ctx context.Context) []*agentv1.EngineInfo {
	_, infos := DetectEngines(ctx)
	return infos
}

func (o *LocalDocker) eng(ctx context.Context, workloadID string) *Engine {
	if o.state == nil {
		return NewDockerEngine()
	}
	return o.state.For(ctx, workloadID)
}

// Every workload joins this bridge, so private links resolve by name in any start order.
const linkNetwork = "ownship"

func ensureLinkNetwork(ctx context.Context, eng *Engine) {
	// "Already exists" is the usual case.
	_ = eng.Cmd(ctx, "network", "create", linkNetwork).Run()
}

func (o *LocalDocker) Run(ctx context.Context, spec RunSpec) (Target, error) {
	defer o.runLocks.lock(spec.WorkloadID)()
	eng, ok := o.state.Engine(spec.Engine)
	if !ok {
		return Target{}, fmt.Errorf("%s is not available on this host", o.state.byKind(spec.Engine).Name())
	}
	var target Target
	var err error
	if rec, ok := o.state.Recorded(spec.WorkloadID); ok && rec != eng.Kind {
		target, err = o.switchTo(ctx, o.state.byKind(rec), eng, spec)
		if err == nil {
			_ = o.state.SetPendingOld(spec.WorkloadID, rec)
		}
	} else {
		target, err = o.runOn(ctx, eng, spec, "")
	}
	if err != nil {
		return Target{}, err
	}
	_ = o.state.Set(spec.WorkloadID, eng.Kind)
	_ = o.state.SetAsleep(spec.WorkloadID, false)
	o.bcache.forget(spec.WorkloadID)
	return target, nil
}

// switchTo moves a workload to another engine: the old container is stopped and its volumes
// copied, and any failure restores it. FinalizeSwitch retires it.
func (o *LocalDocker) switchTo(ctx context.Context, old, eng *Engine, spec RunSpec) (Target, error) {
	id, name := spec.WorkloadID, "ownship-"+spec.WorkloadID
	spec.LogLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "moving this workload from "+old.Name()+" to "+eng.Name())
	port := previousHostPort(ctx, old, name+authSidecarSuffix, name)
	return runSwitch(switchOps{
		ensureImage: func() error {
			if spec.Compose != nil || spec.PullImage || eng.Cmd(ctx, "image", "inspect", "--", spec.Image).Run() == nil {
				return nil
			}
			if old.Cmd(ctx, "image", "inspect", "--", spec.Image).Run() != nil {
				return nil
			}
			return copyImage(ctx, old, eng, spec.Image)
		},
		stopOld: func() error { return o.stopForSwitch(ctx, old, id) },
		copyVolumes: func() error {
			return migrateVolumes(ctx, engineVolumeCopier{}, old, eng, workloadVolumes(ctx, old, id), spec.LogLine)
		},
		runNew:    func() (Target, error) { return o.runOn(ctx, eng, spec, port) },
		removeNew: func() { o.removeOn(ctx, eng, id, false) },
		startOld:  func() { _ = o.startOn(ctx, old, id) },
	})
}

func (o *LocalDocker) stopForSwitch(ctx context.Context, old *Engine, workloadID string) error {
	err := o.stopOn(ctx, old, workloadID)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "no such container") {
		return nil
	}
	return err
}

// FinalizeSwitch retires the old engine once the new one is healthy.
func (o *LocalDocker) FinalizeSwitch(ctx context.Context, workloadID string) error {
	k, ok := o.state.PendingOld(workloadID)
	if !ok {
		return nil
	}
	o.removeOn(ctx, o.state.byKind(k), workloadID, false)
	return o.state.ClearPendingOld(workloadID)
}

// AbortSwitch drops the new copy and restarts the old container.
func (o *LocalDocker) AbortSwitch(ctx context.Context, workloadID string) error {
	k, ok := o.state.PendingOld(workloadID)
	if !ok {
		return nil
	}
	old := o.state.byKind(k)
	o.removeOn(ctx, o.state.For(ctx, workloadID), workloadID, false)
	if err := o.startOn(ctx, old, workloadID); err != nil {
		return err
	}
	if err := o.state.Set(workloadID, k); err != nil {
		return err
	}
	return o.state.ClearPendingOld(workloadID)
}

// workloadVolumes includes compose volumes, which share the prefix.
func workloadVolumes(ctx context.Context, eng *Engine, workloadID string) []string {
	out, err := runEngineCmd(eng.Cmd(ctx, "volume", "ls", "-q", "--filter", "name=ownship-"+workloadID+"-"))
	if err != nil {
		return nil
	}
	var vols []string
	for _, v := range strings.Fields(string(out)) {
		if strings.HasPrefix(v, "ownship-"+workloadID+"-") {
			vols = append(vols, v)
		}
	}
	return vols
}

// runOn runs the workload on eng; portHint keeps the host port from another engine.
func (o *LocalDocker) runOn(ctx context.Context, eng *Engine, spec RunSpec, portHint string) (Target, error) {
	if spec.Compose != nil {
		return o.runCompose(ctx, eng, spec, portHint)
	}
	name := "ownship-" + spec.WorkloadID
	// Keep the host port across redeploys: the Cloudflare route and the waker point at it.
	hostPort := previousHostPort(ctx, eng, name+authSidecarSuffix, name)
	if hostPort == "" {
		hostPort = portHint
	}
	// Services left behind by a project that switched from compose to a single image.
	o.removeComposeLeftovers(ctx, eng, spec.WorkloadID)
	// Replace the previous container and auth sidecar; the sidecar returns only if configured.
	_ = eng.Cmd(ctx, "rm", "-f", name, name+authSidecarSuffix).Run()
	if hostPort == "" || !portFree(hostPort) {
		p, err := freePort()
		if err != nil {
			return Target{}, fmt.Errorf("allocate host port: %w", err)
		}
		hostPort = p
	}
	ensureLinkNetwork(ctx, eng)

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
		imageUser, err := imageUserLocal(ctx, eng, spec.Image)
		if err != nil {
			// Fail safe: force non-root even if inspect failed.
			spec.LogLine(agentv1.LogStream_LOG_STREAM_SYSTEM,
				"image inspect failed, forcing non-root as a fail-safe: "+err.Error())
			imageUser = ""
		}
		userArgs = dockerUserArgs(imageUser)
	}

	// 0600 env file keeps secrets off argv.
	scratch, cleanupScratch, err := scratchDir(ctx, eng)
	if err != nil {
		return Target{}, err
	}
	defer cleanupScratch()
	envPath := filepath.Join(scratch, "app.env")
	if err := writeScratch(ctx, eng, envPath, envFileContent(spec.Env, spec.Port)); err != nil {
		return Target{}, err
	}

	// Restart after a crash or reboot; a slept app stays asleep.
	args := []string{"run", "-d", "--name", name, "--restart", eng.RestartPolicy(),
		"--network", linkNetwork, "--env-file", envPath}
	args = append(args, userArgs...)
	// --name stays ownship-<id> for tracking; --hostname is the user's cosmetic name.
	args = append(args, "--hostname", lxcHostname(spec.Hostname, spec.WorkloadID))
	if mb := spec.Resources.GetMemoryMb(); mb > 0 {
		args = append(args, "-m", fmt.Sprintf("%dm", mb))
	}
	if vcpu := spec.Resources.GetVcpu(); vcpu > 0 {
		args = append(args, "--cpus", strconv.FormatUint(uint64(vcpu), 10))
	}
	if spec.Resources.GetGpu() {
		gpuArgs, notes, err := eng.GPUArgs(detectGPUs(ctx, "/"), eng.HasNvidia(ctx))
		for _, n := range notes {
			spec.LogLine(agentv1.LogStream_LOG_STREAM_SYSTEM, n)
		}
		if err != nil {
			return Target{}, err
		}
		args = append(args, gpuArgs...)
	}
	// Docker won't chown a named volume to match --user, so chmod 0777 up front.
	for _, v := range spec.Volumes {
		vol := volumeName(spec.WorkloadID, v.GetName())
		if nonRootEnforced {
			if err := ensureWritableVolume(ctx, eng, vol); err != nil {
				return Target{}, err
			}
		}
		args = append(args, "-v", vol+":"+v.GetMountPath())
	}
	// Behind Basic Auth only the sidecar is published. An explicit host port survives restarts;
	// ":0" would be re-picked each start.
	if spec.BasicAuth == nil {
		args = append(args, "-p", fmt.Sprintf("127.0.0.1:%s:%d", hostPort, spec.Port))
	}
	// -- stops an image ref starting with "-" being parsed as a flag.
	args = append(args, "--", spec.Image)
	args = append(args, spec.Args...)

	out, err := eng.Cmd(ctx, args...).CombinedOutput()
	if err != nil {
		return Target{}, fmt.Errorf("%s run: %s", eng.Name(), strings.TrimSpace(string(out)))
	}
	containerID := strings.TrimSpace(string(out))

	published, publishedContainerPort := name, spec.Port
	if spec.BasicAuth != nil {
		if err := o.runAuthProxy(ctx, eng, name, hostPort, spec); err != nil {
			return Target{}, err
		}
		published, publishedContainerPort = name+authSidecarSuffix, 80
		spec.LogLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "basic auth proxy fronting the app")
	}
	boundPort, err := publishedPort(ctx, eng, published, publishedContainerPort)
	if err != nil {
		return Target{}, err
	}
	spec.LogLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "container started on host port "+boundPort)
	// Detached ctx so pruning outlives the deploy.
	go pruneImages(context.Background(), eng, spec.WorkloadID, 3)
	return Target{URL: "http://localhost:" + boundPort, ContainerID: containerID}, nil
}

// runCompose runs a sanitized compose project. Only the primary is published, as ownship-<id>.
func (o *LocalDocker) runCompose(ctx context.Context, eng *Engine, spec RunSpec, portHint string) (Target, error) {
	if spec.BasicAuth != nil {
		return Target{}, errors.New("basic auth is not available for compose projects")
	}
	if err := eng.ComposeCmd(ctx, "version").Run(); err != nil {
		if eng.Kind == KindDocker {
			return Target{}, errors.New("docker compose is not installed on this host (install the docker-compose-plugin package)")
		}
		return Target{}, fmt.Errorf("docker-compose can't reach %s (install docker-compose v2 and enable podman.socket)", eng.Name())
	}
	name := "ownship-" + spec.WorkloadID
	hostPort := previousHostPort(ctx, eng, name+authSidecarSuffix, name)
	if hostPort == "" {
		hostPort = portHint
	}
	if hostPort == "" || !portFree(hostPort) {
		p, err := freePort()
		if err != nil {
			return Target{}, fmt.Errorf("allocate host port: %w", err)
		}
		hostPort = p
	}
	// Don't let compose replace a single-image container of the same name.
	if out, _ := eng.Cmd(ctx, "inspect", "--format",
		`{{index .Config.Labels "com.docker.compose.project"}}`, name).Output(); strings.TrimSpace(string(out)) != composeProject(spec.WorkloadID) {
		_ = eng.Cmd(ctx, "rm", "-f", name, name+authSidecarSuffix).Run()
	}
	ensureLinkNetwork(ctx, eng)

	// Kept outside the checkout: a planted symlink could redirect the agent's write.
	work, cleanupWork, err := scratchDir(ctx, eng)
	if err != nil {
		return Target{}, err
	}
	defer cleanupWork()

	gpu, err := composeGPU(spec, detectGPUs(ctx, "/"), func() bool { return eng.HasNvidia(ctx) })
	if err != nil {
		return Target{}, err
	}
	root := ""
	if spec.Compose.HostDir != "" {
		if root, err = filepath.EvalSymlinks(spec.Compose.HostDir); err != nil {
			return Target{}, err
		}
	}
	src, projectDir, resolveBuild, err := composeSource(hostFS{}, root, spec.Compose)
	if err != nil {
		return Target{}, err
	}
	if projectDir == "" {
		projectDir = work
	}
	// Rootless compose reads build contexts as its own user, so it gets a copy.
	if root != "" && eng.Rootless {
		staged, cleanupStaged, err := stageForRootless(ctx, eng, root)
		if err != nil {
			return Target{}, err
		}
		defer cleanupStaged()
		projectDir, resolveBuild = rebasePath(root, staged, projectDir), rebaseResolver(root, staged, resolveBuild)
	}

	envPath := filepath.Join(work, "app.env")
	if err := writeScratch(ctx, eng, envPath, envFileContent(spec.Env, spec.Port)); err != nil {
		return Target{}, err
	}

	file, err := prepareCompose(src, compose.Options{
		Env:              composeEnv(spec.Env, spec.Port),
		Restart:          eng.RestartPolicy(),
		PrimaryService:   spec.Compose.PrimaryService,
		PrimaryContainer: name,
		PrimaryPublish:   fmt.Sprintf("127.0.0.1:%s:%d", hostPort, spec.Port),
		EnvFile:          envPath,
		LinkNetwork:      linkNetwork,
		AllowBuild:       spec.Compose.HostDir != "",
		WorkloadID:       spec.WorkloadID,
		Volume: func(v string) map[string]any {
			return map[string]any{"name": volumeName(spec.WorkloadID, v)}
		},
		ResolveBuild: resolveBuild,
		GPU:          gpu,
	}, spec.LogLine)
	if err != nil {
		return Target{}, err
	}
	composePath := filepath.Join(work, preparedComposeName)
	if err := writeScratch(ctx, eng, composePath, string(file)); err != nil {
		return Target{}, err
	}

	spec.LogLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "starting compose project")
	up := eng.ComposeCmd(ctx, composeUpArgs(spec.WorkloadID, composePath, projectDir)[1:]...)
	up.Dir = work
	if err := runStreamingCmd(up, spec.LogLine); err != nil {
		return Target{}, fmt.Errorf("docker compose up: %w", err)
	}
	out, err := eng.Cmd(ctx, "inspect", "--format", "{{.Id}}", name).Output()
	if err != nil {
		return Target{}, fmt.Errorf("primary service %s did not start", spec.Compose.PrimaryService)
	}
	boundPort, err := publishedPort(ctx, eng, name, spec.Port)
	if err != nil {
		return Target{}, err
	}
	spec.LogLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "compose project up; primary on host port "+boundPort)
	return Target{URL: "http://localhost:" + boundPort, ContainerID: strings.TrimSpace(string(out))}, nil
}

func composeContainers(ctx context.Context, eng *Engine, workloadID string) []string {
	out, err := runEngineCmd(eng.Cmd(ctx, "ps", "-aq",
		"--filter", "label=com.docker.compose.project="+composeProject(workloadID)))
	if err != nil {
		return nil
	}
	return strings.Fields(string(out))
}

// removeComposeLeftovers removes containers and networks; volumes are handled per workload.
func (o *LocalDocker) removeComposeLeftovers(ctx context.Context, eng *Engine, workloadID string) {
	if ids := composeContainers(ctx, eng, workloadID); len(ids) > 0 {
		_, _ = runEngineCmd(eng.Cmd(ctx, append([]string{"rm", "-f"}, ids...)...))
	}
	out, err := runEngineCmd(eng.Cmd(ctx, "network", "ls", "-q",
		"--filter", "label=com.docker.compose.project="+composeProject(workloadID)))
	if err == nil {
		for _, id := range strings.Fields(string(out)) {
			_, _ = runEngineCmd(eng.Cmd(ctx, "network", "rm", id))
		}
	}
}

// runAuthProxy starts the Caddy sidecar; its config (bcrypt hash only) goes via env, not argv.
func (o *LocalDocker) runAuthProxy(ctx context.Context, eng *Engine, name, hostPort string, spec RunSpec) error {
	caddyfile, err := renderCaddyfile(spec.BasicAuth, fmt.Sprintf("ownship-%s:%d", spec.WorkloadID, spec.Port))
	if err != nil {
		return err
	}
	cmd := eng.Cmd(ctx, "run", "-d", "--name", name+authSidecarSuffix,
		"--restart", eng.RestartPolicy(), "--network", linkNetwork, "-p", "127.0.0.1:"+hostPort+":80",
		"-e", "CADDY_CONFIG",
		"--entrypoint", "sh", "--", authProxyImage, "-c", caddyEntrypointScript)
	cmd.Env = append(cmd.Env, "CADDY_CONFIG="+caddyfile)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("auth proxy: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func volumeName(workloadID, name string) string {
	return "ownship-" + workloadID + "-" + name
}

// imageUserLocal returns the image's Config.User, pulling it if needed; "" (root) on failure.
func imageUserLocal(ctx context.Context, eng *Engine, image string) (string, error) {
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

// ensureWritableVolume chmods 0777, since Docker won't chown a volume to match --user.
func ensureWritableVolume(ctx context.Context, eng *Engine, name string) error {
	if out, err := eng.Cmd(ctx, "volume", "create", "--", name).CombinedOutput(); err != nil {
		return fmt.Errorf("create volume %s: %s", name, strings.TrimSpace(string(out)))
	}
	if out, err := eng.Cmd(ctx, "run", "--rm",
		"-v", name+":/data", "--", "busybox", "chmod", "0777", "/data").CombinedOutput(); err != nil {
		return fmt.Errorf("chmod volume %s: %s", name, strings.TrimSpace(string(out)))
	}
	return nil
}

// DialAddr reads back the loopback port Run picked.
func (o *LocalDocker) DialAddr(ctx context.Context, workloadID string, port uint32) (string, error) {
	out, err := o.eng(ctx, workloadID).Cmd(ctx, "port", "ownship-"+workloadID, fmt.Sprintf("%d/tcp", port)).Output()
	if err != nil {
		return "", fmt.Errorf("no published port for %s", workloadID)
	}
	addr := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if addr == "" {
		return "", fmt.Errorf("no published port for %s", workloadID)
	}
	return addr, nil
}

// Backups use throwaway busybox containers on the volume, with no staging file on the host.

func (o *LocalDocker) VolumeTarCmd(ctx context.Context, workloadID, name string) (*exec.Cmd, error) {
	return o.eng(ctx, workloadID).Cmd(ctx, "run", "--rm", "-i",
		"-v", volumeName(workloadID, name)+":/data:ro", "--", "busybox",
		"tar", "czf", "-", "-C", "/data", "."), nil
}

func (o *LocalDocker) VolumeUntarCmd(ctx context.Context, workloadID, name string) (*exec.Cmd, error) {
	// Wipe first so the restore is exact, not a merge.
	return o.eng(ctx, workloadID).Cmd(ctx, "run", "--rm", "-i",
		"-v", volumeName(workloadID, name)+":/data", "--", "busybox",
		"sh", "-c", "find /data -mindepth 1 -delete && tar xzf - -C /data"), nil
}

func (o *LocalDocker) VolumeWriteFileCmd(ctx context.Context, workloadID, name, fileName string) (*exec.Cmd, error) {
	if err := validVolumeFileName(fileName); err != nil {
		return nil, err
	}
	return o.eng(ctx, workloadID).Cmd(ctx, "run", "--rm", "-i",
		"-v", volumeName(workloadID, name)+":/data", "--", "busybox",
		"sh", "-c", "cat > /data/"+fileName), nil
}

func (o *LocalDocker) AppExecCmd(ctx context.Context, workloadID, script string) (*exec.Cmd, error) {
	return o.eng(ctx, workloadID).Cmd(ctx, "exec", "-i", "ownship-"+workloadID, "sh", "-c", script), nil
}

// Volume browsing runs in a throwaway container, so symlinks resolve inside it, not on the host.

func (o *LocalDocker) VolumeListCmd(ctx context.Context, workloadID, name, relPath string) (*exec.Cmd, error) {
	target := "/data"
	if relPath != "" {
		target += "/" + relPath
	}
	return o.eng(ctx, workloadID).Cmd(ctx, "run", "--rm",
		"-v", volumeName(workloadID, name)+":/data:ro", "--", "busybox",
		"find", target, "-maxdepth", "1", "-mindepth", "1",
		"-exec", "stat", "-c", "%n|%s|%Y|%F", "{}", "+"), nil
}

func (o *LocalDocker) VolumeReadCmd(ctx context.Context, workloadID, name, relPath string) (*exec.Cmd, error) {
	return o.eng(ctx, workloadID).Cmd(ctx, "run", "--rm",
		"-v", volumeName(workloadID, name)+":/data:ro", "--", "busybox",
		"cat", "/data/"+relPath), nil
}

// pruneImages keeps the newest `keep` deploy images.
func pruneImages(ctx context.Context, eng *Engine, projectID string, keep int) {
	ref := "ownship/" + projectID + ":*"
	out, err := eng.Cmd(ctx, "images",
		"--filter=reference="+ref, "--format", "{{.Repository}}:{{.Tag}}").Output()
	if err != nil {
		return
	}
	tags := strings.Fields(string(out))
	if len(tags) > keep {
		// In-use images fail to remove and are skipped.
		args := append([]string{"rmi"}, tags[keep:]...)
		_ = eng.Cmd(ctx, args...).Run()
	}
}

// FollowLogs tails new output; compose projects tail every service, prefixed by name.
func (o *LocalDocker) FollowLogs(ctx context.Context, workloadID string, onLine LogFunc) error {
	eng := o.eng(ctx, workloadID)
	ids := composeContainers(ctx, eng, workloadID)
	if len(ids) == 0 {
		return runStreamingCmd(eng.Cmd(ctx, "logs", "-f", "--tail", "0", "ownship-"+workloadID), onLine)
	}
	var wg sync.WaitGroup
	for _, id := range ids {
		out, _ := eng.Cmd(ctx, "inspect", "--format",
			`{{index .Config.Labels "com.docker.compose.service"}}`, id).Output()
		prefix := "[" + strings.TrimSpace(string(out)) + "] "
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

func (o *LocalDocker) Stop(ctx context.Context, workloadID string) error {
	if err := o.stopOn(ctx, o.eng(ctx, workloadID), workloadID); err != nil {
		return err
	}
	// Otherwise Podman's restart service wakes it after a reboot; Resume re-applies this.
	if o.state != nil {
		_ = o.state.SetAsleep(workloadID, true)
	}
	return nil
}

func (o *LocalDocker) Start(ctx context.Context, workloadID string) error {
	if err := o.startOn(ctx, o.eng(ctx, workloadID), workloadID); err != nil {
		return err
	}
	if o.state != nil {
		_ = o.state.SetAsleep(workloadID, false)
	}
	return nil
}

func (o *LocalDocker) stopOn(ctx context.Context, eng *Engine, workloadID string) error {
	_, _ = runEngineCmd(eng.Cmd(ctx, "stop", "ownship-"+workloadID+authSidecarSuffix))
	if ids := composeContainers(ctx, eng, workloadID); len(ids) > 0 {
		_, _ = runEngineCmd(eng.Cmd(ctx, append([]string{"stop"}, ids...)...))
	}
	if _, err := runEngineCmd(eng.Cmd(ctx, "stop", "ownship-"+workloadID)); err != nil {
		return errors.New(engineErr(err))
	}
	return nil
}

func (o *LocalDocker) startOn(ctx context.Context, eng *Engine, workloadID string) error {
	_, _ = runEngineCmd(eng.Cmd(ctx, "start", "ownship-"+workloadID+authSidecarSuffix))
	// Dependencies first, so the primary finds them up.
	if ids := composeContainers(ctx, eng, workloadID); len(ids) > 0 {
		_, _ = runEngineCmd(eng.Cmd(ctx, append([]string{"start"}, ids...)...))
	}
	if _, err := runEngineCmd(eng.Cmd(ctx, "start", "ownship-"+workloadID)); err != nil {
		return errors.New(engineErr(err))
	}
	return nil
}

func (o *LocalDocker) removeOn(ctx context.Context, eng *Engine, workloadID string, keepVolumes bool) {
	name := "ownship-" + workloadID
	_, _ = runEngineCmd(eng.Cmd(ctx, "rm", "-f", name+authSidecarSuffix))
	_, _ = runEngineCmd(eng.Cmd(ctx, "rm", "-f", name))
	o.removeComposeLeftovers(ctx, eng, workloadID)
	if !keepVolumes {
		for _, vol := range workloadVolumes(ctx, eng, workloadID) {
			_, _ = runEngineCmd(eng.Cmd(ctx, "volume", "rm", vol))
		}
	}
}

// NetworkUpdate: local workloads share one bridge with no static IPs, so it's a no-op.
func (o *LocalDocker) NetworkUpdate(ctx context.Context, workloadID string, network *agentv1.NetworkConfig) error {
	return fmt.Errorf("network reconfiguration is not supported for local Docker workloads")
}

// StatsAll uses one stats and one inspect call, whatever the workload count.
func (o *LocalDocker) StatsAll(ctx context.Context) (map[string]StatsSample, error) {
	samples := map[string]StatsSample{}
	var firstErr error
	answered := false
	for _, eng := range o.state.Engines() {
		got, err := statsOn(ctx, eng)
		if err != nil {
			firstErr = err
			continue
		}
		answered = true
		for id, s := range got {
			samples[id] = s
		}
	}
	if !answered && firstErr != nil {
		return nil, firstErr
	}
	return samples, nil
}

func statsOn(ctx context.Context, eng *Engine) (map[string]StatsSample, error) {
	out, err := eng.Cmd(ctx, "stats", "--no-stream",
		"--format", "{{.Name}};{{.CPUPerc}};{{.MemUsage}};{{.NetIO}}").Output()
	if err != nil {
		return nil, err
	}
	samples := map[string]StatsSample{}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		name, rest, ok := strings.Cut(line, ";")
		if !ok || !strings.HasPrefix(name, "ownship-") || strings.HasSuffix(name, authSidecarSuffix) {
			continue
		}
		id := strings.TrimPrefix(name, "ownship-")
		samples[id] = parseDockerStats(rest)
		names = append(names, name)
	}
	if len(names) == 0 {
		return samples, nil
	}
	// A failed inspect leaves restart counts at 0 rather than dropping samples.
	args := append([]string{"inspect", "--format", "{{.Name}}|{{.RestartCount}}"}, names...)
	if rc, err := eng.Cmd(ctx, args...).Output(); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(rc)), "\n") {
			name, cnt, ok := strings.Cut(line, "|")
			if !ok {
				continue
			}
			id := strings.TrimPrefix(strings.TrimPrefix(name, "/"), "ownship-")
			if s, ok := samples[id]; ok {
				s.RestartCount = parseUint(strings.TrimSpace(cnt))
				samples[id] = s
			}
		}
	}
	return samples, nil
}

func (o *LocalDocker) ExecArgs(ctx context.Context, workloadID string, inner []string, tty bool) ([]string, error) {
	args := []string{"exec", "-i"}
	if tty {
		args = append(args, "-t")
	}
	args = append(args, "ownship-"+workloadID)
	return o.eng(ctx, workloadID).ExecArgv(append(args, inner...)...), nil
}

// MachineExecArgs is refused: the machine here is the agent host, not part of the workload.
func (o *LocalDocker) MachineExecArgs(context.Context, string, []string, bool) ([]string, error) {
	return nil, errors.New("this homelab runs plain Docker — the container is the whole workload, so there is no machine shell")
}

func (o *LocalDocker) Destroy(ctx context.Context, workloadID string, keepVolumes bool) error {
	defer o.runLocks.lock(workloadID)()

	name := "ownship-" + workloadID
	eng := o.eng(ctx, workloadID)
	_, _ = runEngineCmd(eng.Cmd(ctx, "rm", "-f", name+authSidecarSuffix))
	if _, err := runEngineCmd(eng.Cmd(ctx, "rm", "-f", name)); err != nil {
		return fmt.Errorf("remove container: %s", engineErr(err))
	}
	o.removeComposeLeftovers(ctx, eng, workloadID)
	if !keepVolumes {
		out, err := runEngineCmd(eng.Cmd(ctx, "volume", "ls", "-q",
			"--filter", "name=ownship-"+workloadID+"-"))
		if err == nil {
			for _, vol := range strings.Fields(string(out)) {
				_, _ = runEngineCmd(eng.Cmd(ctx, "volume", "rm", vol))
			}
		}
	}
	if o.state != nil {
		_ = o.state.Remove(workloadID)
	}
	if o.bridge != nil {
		o.bridge.DropWorkload(workloadID)
	}
	o.bcache.forget(workloadID)
	return nil
}

func (o *LocalDocker) HostBuilds() bool { return true }

// List returns project ids from ownship-<id> names. Sidecars and compose services (whose names
// contain "ownship-") are excluded, or the reconcile GC would destroy them as unknown.
func (o *LocalDocker) List(ctx context.Context) (Inventory, error) {
	inv := Inventory{Complete: true}
	var firstErr error
	for _, eng := range o.state.Engines() {
		out, err := runEngineCmd(eng.Cmd(ctx, "ps", "-a",
			"--filter", "name=ownship-", "--format", "{{.Names}}"))
		if err != nil {
			inv.Complete = false
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %s", eng.Name(), engineErr(err))
			}
			continue
		}
		inv.IDs = append(inv.IDs, workloadIDsFromNames(strings.Fields(string(out)))...)
	}
	if len(inv.IDs) == 0 && firstErr != nil {
		return Inventory{}, firstErr
	}
	// No engine answering, or one a recorded workload needs: absence proves nothing yet.
	if len(o.state.Engines()) == 0 || o.state.missingRecorded() {
		inv.Complete = false
	}
	sort.Strings(inv.IDs)
	inv.IDs = slices.Compact(inv.IDs)
	return inv, nil
}

func workloadIDsFromNames(names []string) []string {
	var ids []string
	for _, name := range names {
		if !strings.HasPrefix(name, "ownship-") || strings.HasSuffix(name, authSidecarSuffix) {
			continue
		}
		ids = append(ids, strings.TrimPrefix(name, "ownship-"))
	}
	return ids
}

func publishedPort(ctx context.Context, eng *Engine, name string, port uint32) (string, error) {
	out, err := eng.Cmd(ctx, "port", name, fmt.Sprintf("%d/tcp", port)).Output()
	if err != nil {
		return "", fmt.Errorf("resolve published port: %w", err)
	}
	line := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
	idx := strings.LastIndex(line, ":")
	if idx < 0 {
		return "", fmt.Errorf("unexpected docker port output: %q", line)
	}
	return line[idx+1:], nil
}
