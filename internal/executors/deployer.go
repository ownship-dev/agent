package executors

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ownship/agent/internal/cloudflare"
	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
	"github.com/ownship/agent/internal/lanproxy"
	"github.com/ownship/agent/internal/registry"
	"github.com/ownship/agent/internal/telemetry"
	"github.com/ownship/agent/internal/tsingress"
)

// Deployer builds the repo and hands the image to an Orchestrator to run.
type Deployer struct {
	orch          Orchestrator
	cf            *cloudflare.Router
	rtl           *RuntimeLogs
	services      *serviceState
	workspacesDir string
	waker         *Waker
	// Private ingress; nil disables it.
	lan *lanproxy.Proxy
	ts  *tsingress.Ingress
	// Where a relayed backup buddy's S3 calls go; empty disables relayed targets.
	controlPlaneURL string
}

func (d *Deployer) SetControlPlaneURL(u string) {
	d.controlPlaneURL = u
}

// ServiceURL is the workload's address as of its last deploy.
func (d *Deployer) ServiceURL(workloadID string) (string, bool) {
	return d.services.get(workloadID)
}

func NewDeployer(orch Orchestrator, rtl *RuntimeLogs, waker *Waker, lan *lanproxy.Proxy, ts *tsingress.Ingress, workspacesDir, serviceStatePath string) *Deployer {
	return &Deployer{
		orch:          orch,
		cf:            cloudflare.NewRouter(),
		rtl:           rtl,
		services:      loadServiceState(serviceStatePath),
		workspacesDir: workspacesDir,
		waker:         waker,
		lan:           lan,
		ts:            ts,
	}
}

// GPUs reports this machine's GPUs. On Proxmox the NVIDIA runtime that matters is inside
// each LXC, so it's reported as needing setup there.
func (d *Deployer) GPUs(ctx context.Context) []*agentv1.Gpu {
	g := detectGPUs(ctx, "/")
	if !g.any() {
		return nil
	}
	node, _ := os.Hostname()
	if d.orch.HostBuilds() {
		return gpuInventory(g, node, NewDockerEngine().HasNvidia(ctx))
	}
	out := gpuInventory(g, node, false)
	version := d.PveVersion(ctx)
	for _, gpu := range out {
		switch {
		case !supportsGPUPassthrough(version):
			gpu.Usable = false
			gpu.Hint = gpuPassthroughRefusal(version).Error()
		case gpu.Kind == "nvidia":
			gpu.Hint = "each LXC needs nvidia-container-toolkit and driver libraries matching the host"
		}
	}
	return out
}

// PveVersion is "" on a Docker host.
func (d *Deployer) PveVersion(ctx context.Context) string {
	if cd, ok := d.orch.(ClusterDiscovery); ok {
		return cd.PveVersion(ctx)
	}
	return ""
}

type engineReporter interface {
	Engines(ctx context.Context) []*agentv1.EngineInfo
}

func (d *Deployer) Engines(ctx context.Context) []*agentv1.EngineInfo {
	if r, ok := d.orch.(engineReporter); ok {
		return r.Engines(ctx)
	}
	return nil
}

// LanCAPEM is "" when LAN ingress is unavailable.
func (d *Deployer) LanCAPEM() string {
	if d.lan == nil {
		return ""
	}
	return d.lan.CAPEM()
}

// workloadID gives previews and managed services their own container or LXC.
func workloadID(projectID, previewID, serviceID string) string {
	if serviceID != "" {
		return serviceID
	}
	if previewID != "" {
		return previewID
	}
	return projectID
}

func (d *Deployer) Deploy(ctx context.Context, cmd *agentv1.Deploy, emit Emit) error {
	id := cmd.GetDeploymentId()
	wl := workloadID(cmd.GetProjectId(), cmd.GetPreviewId(), cmd.GetServiceId())
	var seq atomic.Uint64
	logLine := func(stream agentv1.LogStream, text string) {
		_ = emit(logChunk(id, seq.Add(1), stream, text))
	}

	if err := d.authenticateRegistry(ctx, cmd, logLine); err != nil {
		return err
	}
	if auth := cmd.GetRegistryAuth(); auth != nil && d.orch.HostBuilds() {
		defer dockerLogoutHost(ctx, d.hostEngine(cmd), auth.GetHost())
	}
	var image, composeDir string
	repoBuild := true
	if cmd.GetCompose() != nil {
		// The checkout must outlive Run, since compose builds from it. Proxmox clones inside the LXC.
		if d.orch.HostBuilds() && cmd.GetRepo().GetCloneUrl() != "" {
			dir, cleanup, err := d.checkout(ctx, cmd, wl, logLine, emit)
			if err != nil {
				return err
			}
			defer cleanup()
			composeDir = dir
		}
	} else {
		var err error
		image, repoBuild, err = d.resolveImage(ctx, cmd, wl, id, logLine, emit)
		if err != nil {
			return err
		}
	}

	if err := emitLifecycle(emit, id, agentv1.Phase_PHASE_STARTING, "starting workload"); err != nil {
		return err
	}
	d.rtl.Stop(wl)

	env := d.resolveLinks(ctx, cmd, wl, logLine)
	spec := d.buildRunSpec(cmd, wl, image, env, repoBuild, logLine)
	if c := cmd.GetCompose(); c != nil {
		spec.Compose = &ComposeRun{Path: c.GetPath(), Inline: c.GetInline(), PrimaryService: c.GetPrimaryService(), HostDir: composeDir}
	}
	var target Target
	err := traced(ctx, "deploy.run", func(c context.Context) error {
		var runErr error
		target, runErr = d.orch.Run(c, spec)
		return runErr
	})
	if err != nil {
		// Run has already torn down the old workload, so a failure here is rollback-eligible.
		var coded *CodedError
		if errors.As(err, &coded) {
			return err
		}
		return &CodedError{Code: CodeRunFailed, Err: err}
	}
	// Remembered so a later RouteUpdate can find it.
	d.services.set(wl, target.URL)

	if err := d.restoreVolumes(ctx, cmd, wl, logLine); err != nil {
		d.abortEngineSwitch(ctx, wl, logLine)
		return err
	}
	if err := d.runPreDeployHook(ctx, cmd, wl, id, logLine, emit); err != nil {
		d.abortEngineSwitch(ctx, wl, logLine)
		return err
	}
	if err := d.runHealthGate(ctx, cmd, wl, id, target, logLine, emit); err != nil {
		d.abortEngineSwitch(ctx, wl, logLine)
		return err
	}
	d.finalizeEngineSwitch(ctx, wl, logLine)

	url, lanCA, err := d.configureRouting(ctx, cmd, wl, id, target, logLine, emit)
	if err != nil {
		return err
	}

	d.runPostDeployHook(ctx, cmd, wl, id, logLine, emit)

	if err := emitLifecycle(emit, id, agentv1.Phase_PHASE_LIVE, "workload running"); err != nil {
		return err
	}
	d.rtl.Start(wl)

	return emit(&agentv1.Event{
		Payload: &agentv1.Event_Deployed{
			Deployed: &agentv1.Deployed{
				DeploymentId: id,
				Url:          url,
				ServiceId:    cmd.GetServiceId(),
				LanCaPem:     lanCA,
				Actual: &agentv1.ActualState{
					ContainerId: target.ContainerID,
					LxcId:       target.LxcID,
					Ip:          hostFromURL(target.URL),
					ImageDigest: image,
				},
			},
		},
	})
}

// engineSwitcher finalizes an engine switch after the health gate, or aborts it on failure.
type engineSwitcher interface {
	FinalizeSwitch(ctx context.Context, workloadID string) error
	AbortSwitch(ctx context.Context, workloadID string) error
}

// abortEngineSwitch restores the old engine's copy; the auto-rollback then retries the move.
func (d *Deployer) abortEngineSwitch(ctx context.Context, wl string, logLine LogFunc) {
	if s, ok := d.orch.(engineSwitcher); ok {
		if err := s.AbortSwitch(ctx, wl); err != nil {
			logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "could not restore the previous engine's copy: "+err.Error())
		}
	}
}

// finalizeEngineSwitch runs only after the health gate; a cleanup failure is only logged.
func (d *Deployer) finalizeEngineSwitch(ctx context.Context, wl string, logLine LogFunc) {
	if s, ok := d.orch.(engineSwitcher); ok {
		if err := s.FinalizeSwitch(ctx, wl); err != nil {
			logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "could not remove the previous engine's copy: "+err.Error())
		}
	}
}

type engineResolver interface {
	EngineFor(k EngineKind) (*Engine, bool)
}

// hostEngine is the engine for host builds and registry logins. Run refuses one the host
// lacks, so the Docker fallback never runs a workload.
func (d *Deployer) hostEngine(cmd *agentv1.Deploy) *Engine {
	if r, ok := d.orch.(engineResolver); ok {
		if e, ok := r.EngineFor(cmd.GetContainerEngine()); ok {
			return e
		}
	}
	return NewDockerEngine()
}

// authenticateRegistry logs in up front, so both private base images and the app image pull.
func (d *Deployer) authenticateRegistry(ctx context.Context, cmd *agentv1.Deploy, logLine LogFunc) error {
	auth := cmd.GetRegistryAuth()
	if auth == nil || !d.orch.HostBuilds() {
		return nil
	}
	if err := dockerLoginHost(ctx, d.hostEngine(cmd), auth); err != nil {
		return err
	}
	logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "authenticated to registry "+auth.GetHost())
	return nil
}

// resolveImage picks the image to run, building it on host-building orchestrators. Each build
// gets its own tag so rollbacks can re-run it.
func (d *Deployer) resolveImage(ctx context.Context, cmd *agentv1.Deploy, wl, id string, logLine LogFunc, emit Emit) (string, bool, error) {
	image := cmd.GetImageRef()
	repoBuild := image == ""
	if !repoBuild {
		logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "using image "+image)
		return image, repoBuild, nil
	}
	image = fmt.Sprintf("ownship/%s:%s", wl, shortID(id))
	if d.orch.HostBuilds() {
		if err := d.build(ctx, cmd, wl, image, logLine, emit); err != nil {
			return "", repoBuild, err
		}
	}
	return image, repoBuild, nil
}

// resolveLinks injects each private link's address as env. A target not yet deployed is
// skipped and resolves on the next deploy.
func (d *Deployer) resolveLinks(ctx context.Context, cmd *agentv1.Deploy, wl string, logLine LogFunc) []*agentv1.EnvEntry {
	env := cmd.GetEnv()
	for _, link := range cmd.GetLinks() {
		addr, err := d.orch.LinkAddr(ctx, wl, link.GetTargetWorkloadId(), cmd.GetContainerEngine(), link.GetTargetPort())
		if err != nil {
			logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "link "+link.GetEnvKey()+" skipped: "+err.Error())
			continue
		}
		env = append(env, &agentv1.EnvEntry{Key: link.GetEnvKey(), Value: addr})
	}
	return env
}

func (d *Deployer) buildRunSpec(cmd *agentv1.Deploy, wl, image string, env []*agentv1.EnvEntry, repoBuild bool, logLine LogFunc) RunSpec {
	spec := RunSpec{
		WorkloadID:   wl,
		Image:        image,
		Resources:    cmd.GetResources(),
		Port:         cmd.GetRuntime().GetPort(),
		Env:          env,
		Volumes:      cmd.GetVolumes(),
		Args:         cmd.GetArgs(),
		TargetNode:   cmd.GetTargetNode(),
		LogLine:      logLine,
		BaseTemplate: cmd.GetBaseTemplate(),
		TemplateVmid: cmd.GetTemplateVmid(),
		BaseOS:       cmd.GetBaseOs(),
		RegistryAuth: cmd.GetRegistryAuth(),
		BasicAuth:    cmd.GetBasicAuth(),
		// Registry images re-pull so a moved tag lands; local build tags exist only here.
		PullImage:        !repoBuild && !strings.HasPrefix(image, "ownship/"),
		Hostname:         cmd.GetRuntime().GetHostname(),
		Network:          cmd.GetNetwork(),
		AllowRoot:        cmd.GetAllowRoot(),
		IsManagedService: cmd.GetServiceId() != "",
		LxcPassword:      cmd.GetLxcPassword(),
		Engine:           normalizeKind(cmd.GetContainerEngine()),
	}
	// On Proxmox, Run clones and builds inside the LXC.
	if repoBuild && !d.orch.HostBuilds() {
		spec.Repo = cmd.GetRepo()
		spec.Build = cmd.GetBuild()
		spec.Cache = cmd.GetCache().GetEnabled()
	}
	return spec
}

// restoreVolumes restores migrated volume data before the health gate.
func (d *Deployer) restoreVolumes(ctx context.Context, cmd *agentv1.Deploy, wl string, logLine LogFunc) error {
	if len(cmd.GetRestore()) == 0 {
		return nil
	}
	logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "restoring volume data from backups")
	if err := d.orch.Stop(ctx, wl); err != nil {
		return fmt.Errorf("stop for restore: %w", err)
	}
	if err := d.restoreVolumesFromHints(ctx, wl, cmd.GetRestore(), logLine); err != nil {
		return err
	}
	if err := d.orch.Start(ctx, wl); err != nil {
		return fmt.Errorf("start after restore: %w", err)
	}
	return nil
}

// runPreDeployHook runs before the health gate; a failure aborts the deploy, so auto-rollback
// applies and no traffic reaches it.
func (d *Deployer) runPreDeployHook(ctx context.Context, cmd *agentv1.Deploy, wl, id string, logLine LogFunc, emit Emit) error {
	hook := cmd.GetHooks().GetPreDeploy()
	if hook == "" {
		return nil
	}
	if err := emitLifecycle(emit, id, agentv1.Phase_PHASE_PRE_DEPLOY_HOOK, "running pre-deploy hook"); err != nil {
		return err
	}
	logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "pre-deploy hook: "+hook)
	if err := d.runHook(ctx, wl, hook, cmd.GetHooks().GetTimeoutSeconds(), logLine); err != nil {
		d.stopWorkload(wl, logLine, "pre-deploy hook failed — stopped workload")
		return &CodedError{Code: CodePreDeployHookFailed, Err: fmt.Errorf("pre-deploy hook: %w", err)}
	}
	return nil
}

// runHealthGate blocks routing until the app answers; an unhealthy workload is stopped.
func (d *Deployer) runHealthGate(ctx context.Context, cmd *agentv1.Deploy, wl, id string, target Target, logLine LogFunc, emit Emit) error {
	hc := cmd.GetHealth()
	if !hc.GetEnabled() {
		return nil
	}
	if err := emitLifecycle(emit, id, agentv1.Phase_PHASE_HEALTHCHECK, "waiting for health check"); err != nil {
		return err
	}
	if err := traced(ctx, "deploy.healthcheck", func(c context.Context) error {
		return waitHealthy(c, target.URL, hc, cmd.GetBasicAuth(), logLine)
	}); err != nil {
		d.stopWorkload(wl, logLine, "stopped unhealthy workload")
		return err
	}
	return nil
}

// configureRouting sets up wake-on-request and Cloudflare. Routing to the waker means the
// route never changes on wake.
func (d *Deployer) configureRouting(ctx context.Context, cmd *agentv1.Deploy, wl, id string, target Target, logLine LogFunc, emit Emit) (string, string, error) {
	url := target.URL
	cf := cmd.GetCloudflare()
	hostnames := routableHostnames(cf)
	lanHosts := cmd.GetLan().GetHostnames()
	routeTarget := target.URL
	if cmd.GetWakeOnRequest() && len(hostnames)+len(lanHosts) > 0 {
		if err := d.waker.Register(wl, append(append([]string{}, hostnames...), lanHosts...), target.URL, cmd.GetHealth()); err != nil {
			logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "wake-on-request disabled: "+err.Error())
		} else {
			routeTarget = d.waker.URL()
			logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "wake-on-request active via "+routeTarget)
		}
	} else {
		d.waker.Unregister(wl)
	}
	if len(hostnames) > 0 && cf.GetApiToken() != "" {
		if err := emitLifecycle(emit, id, agentv1.Phase_PHASE_ROUTING, "configuring Cloudflare routes"); err != nil {
			return "", "", err
		}
		targets := make([]cloudflare.Target, 0, len(hostnames))
		for _, h := range hostnames {
			logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "routing "+h+" -> "+routeTarget)
			targets = append(targets, targetFor(cf, h))
		}
		// One tunnel config write for all hostnames; DNS upserts run concurrently.
		if err := traced(ctx, "deploy.routing", func(c context.Context) error {
			return d.cf.RouteAll(c, targets, routeTarget)
		}); err != nil {
			return "", "", fmt.Errorf("cloudflare route: %w", err)
		}
		if cf.GetHostname() != "" {
			url = "https://" + cf.GetHostname()
		}
	}
	privateURL, lanCA, err := d.configurePrivateIngress(ctx, cmd, wl, id, target.URL, routeTarget, logLine, emit)
	if err != nil {
		return "", "", err
	}
	if privateURL != "" {
		url = privateURL
	}
	return url, lanCA, nil
}

// configurePrivateIngress serves on the LAN or tailnet and removes whichever is no longer wanted.
func (d *Deployer) configurePrivateIngress(ctx context.Context, cmd *agentv1.Deploy, wl, id, direct, routeTarget string, logLine LogFunc, emit Emit) (string, string, error) {
	url, newCA := "", ""
	lan := cmd.GetLan()
	if d.lan != nil {
		if len(lan.GetHostnames()) == 0 {
			if len(d.lan.Hosts(wl)) > 0 {
				_ = d.lan.Drop(wl)
				logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "removed LAN routes")
			}
		} else {
			if err := emitLifecycle(emit, id, agentv1.Phase_PHASE_ROUTING, "configuring LAN routes"); err != nil {
				return "", "", err
			}
			ca, err := d.lan.Set(wl, lan.GetHostnames(), routeTarget, int(lan.GetHttpsPort()), lan.GetCaDomain())
			if err != nil {
				return "", "", fmt.Errorf("lan route: %w", err)
			}
			if ca != "" {
				newCA = ca
				logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "created a LAN certificate authority for "+lan.GetCaDomain()+"; download it again from the machine to trust it")
			}
			for _, h := range lan.GetHostnames() {
				logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "LAN https://"+h+" -> "+routeTarget)
			}
			url = "https://" + lan.GetHostnames()[0]
			if p := lan.GetHttpsPort(); p != 0 && p != lanproxy.DefaultPort {
				url = fmt.Sprintf("%s:%d", url, p)
			}
		}
	} else if len(lan.GetHostnames()) > 0 {
		return "", "", errors.New("LAN ingress is unavailable on this agent (its CA could not be loaded)")
	}

	ts := cmd.GetTailscale()
	if ts.GetHostname() == "" {
		if d.ts != nil && d.ts.Has(wl) {
			_ = d.ts.Drop(ctx, wl)
			logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "removed tailnet node")
		}
		return url, newCA, nil
	}
	if d.ts == nil {
		return "", "", errors.New("Tailscale ingress is unavailable on this agent")
	}
	if err := emitLifecycle(emit, id, agentv1.Phase_PHASE_ROUTING, "joining the tailnet"); err != nil {
		return "", "", err
	}
	// The waker routes by public hostname, so a tailnet node always targets the app.
	tsURL, err := d.ts.Ensure(ctx, wl, ts.GetHostname(), ts.GetAuthKey(), direct)
	if err != nil {
		return "", "", err
	}
	logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "tailnet "+tsURL+" -> "+direct)
	return tsURL, newCA, nil
}

// runPostDeployHook runs after routing; a failure only warns, as the deploy is already live.
func (d *Deployer) runPostDeployHook(ctx context.Context, cmd *agentv1.Deploy, wl, id string, logLine LogFunc, emit Emit) {
	hook := cmd.GetHooks().GetPostDeploy()
	if hook == "" {
		return
	}
	_ = emitLifecycle(emit, id, agentv1.Phase_PHASE_POST_DEPLOY_HOOK, "running post-deploy hook")
	logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "post-deploy hook: "+hook)
	if err := d.runHook(ctx, wl, hook, cmd.GetHooks().GetTimeoutSeconds(), logLine); err != nil {
		logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "post-deploy hook failed (deploy stays live): "+err.Error())
	}
}

// stopWorkload uses a fresh context, since the deploy's may already be done.
func (d *Deployer) stopWorkload(wl string, logLine LogFunc, note string) {
	stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = d.orch.Stop(stopCtx, wl)
	logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, note)
}

// hostFromURL uses SplitHostPort, which handles IPv6 literals.
func hostFromURL(raw string) string {
	s := strings.TrimPrefix(raw, "http://")
	host, _, err := net.SplitHostPort(s)
	if err != nil {
		return strings.Trim(s, "[]") // no port at all
	}
	return host
}

func routableHostnames(cf *agentv1.CloudflareTarget) []string {
	var out []string
	if h := cf.GetHostname(); h != "" {
		out = append(out, h)
	}
	return append(out, cf.GetExtraHostnames()...)
}

func targetFor(cf *agentv1.CloudflareTarget, hostname string) cloudflare.Target {
	return cloudflare.Target{
		Hostname:  hostname,
		TunnelID:  cf.GetTunnelId(),
		APIToken:  cf.GetApiToken(),
		AccountID: cf.GetAccountId(),
		ZoneID:    cf.GetZoneId(),
	}
}

// RouteUpdate changes custom domains on a running workload without a redeploy.
func (d *Deployer) RouteUpdate(ctx context.Context, cmd *agentv1.RouteUpdate, emit Emit) error {
	if lan := cmd.GetLan(); lan != nil {
		return d.lanRouteUpdate(cmd, lan)
	}
	cf := cmd.GetCloudflare()
	for _, h := range cmd.GetRemove() {
		if err := d.cf.Unroute(ctx, targetFor(cf, h)); err != nil {
			return fmt.Errorf("unroute %s: %w", h, err)
		}
	}
	if len(cmd.GetAdd()) == 0 {
		return nil
	}
	service, ok := d.services.get(cmd.GetProjectId())
	if !ok {
		return fmt.Errorf("project %s is not running; domain will attach on next deploy", cmd.GetProjectId())
	}
	for _, h := range cmd.GetAdd() {
		if err := d.cf.Route(ctx, targetFor(cf, h), service); err != nil {
			return fmt.Errorf("route %s: %w", h, err)
		}
	}
	return nil
}

func (d *Deployer) lanRouteUpdate(cmd *agentv1.RouteUpdate, lan *agentv1.LanTarget) error {
	if d.lan == nil {
		return errors.New("LAN ingress is unavailable on this agent")
	}
	if err := d.lan.Remove(cmd.GetRemove()); err != nil {
		return err
	}
	if len(cmd.GetAdd()) == 0 {
		return nil
	}
	service, ok := d.services.get(cmd.GetProjectId())
	if !ok {
		return fmt.Errorf("project %s is not running; LAN routes will attach on next deploy", cmd.GetProjectId())
	}
	_, err := d.lan.Add(cmd.GetProjectId(), cmd.GetAdd(), service, int(lan.GetHttpsPort()), lan.GetCaDomain())
	return err
}

// NetworkUpdate is Proxmox-only; local Docker rejects it.
func (d *Deployer) NetworkUpdate(ctx context.Context, cmd *agentv1.NetworkUpdate, emit Emit) error {
	return d.orch.NetworkUpdate(ctx, cmd.GetWorkloadId(), cmd.GetNetwork())
}

// checkout syncs the repo into a cached workspace (incremental) or a throwaway dir (hermetic).
func (d *Deployer) checkout(ctx context.Context, cmd *agentv1.Deploy, wl string, logLine LogFunc, emit Emit) (string, func(), error) {
	workDir, cleanup := "", func() {}
	if cmd.GetCache().GetEnabled() && d.workspacesDir != "" {
		workDir = filepath.Join(d.workspacesDir, wl)
		if err := os.MkdirAll(d.workspacesDir, 0o700); err != nil {
			return "", cleanup, fmt.Errorf("create workspaces dir: %w", err)
		}
	} else {
		tmp, err := os.MkdirTemp("", "ownship-"+wl+"-")
		if err != nil {
			return "", cleanup, fmt.Errorf("create workdir: %w", err)
		}
		workDir, cleanup = tmp, func() { _ = os.RemoveAll(tmp) }
	}
	if err := emitLifecycle(emit, cmd.GetDeploymentId(), agentv1.Phase_PHASE_CLONING, "syncing repository"); err != nil {
		cleanup()
		return "", func() {}, err
	}
	if err := traced(ctx, "deploy.clone", func(c context.Context) error {
		return gitSync(c, localGit{}, cmd.GetRepo(), workDir, true, logLine)
	}); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("git sync: %w", err)
	}
	return workDir, cleanup, nil
}

func (d *Deployer) build(ctx context.Context, cmd *agentv1.Deploy, wl, image string, logLine LogFunc, emit Emit) error {
	id := cmd.GetDeploymentId()
	workDir, cleanup, err := d.checkout(ctx, cmd, wl, logLine, emit)
	if err != nil {
		return err
	}
	defer cleanup()

	if err := emitLifecycle(emit, id, agentv1.Phase_PHASE_BUILDING, "building image"); err != nil {
		return err
	}
	if err := traced(ctx, "deploy.build", func(c context.Context) error {
		return buildImage(c, d.hostEngine(cmd), workDir, image, cmd.GetBuild(), logLine)
	}); err != nil {
		return fmt.Errorf("build: %w", err)
	}
	return nil
}

func (d *Deployer) Destroy(ctx context.Context, cmd *agentv1.Destroy, emit Emit) error {
	wl := workloadID(cmd.GetProjectId(), cmd.GetPreviewId(), cmd.GetServiceId())
	d.rtl.Stop(wl)
	d.waker.Unregister(wl)
	d.services.remove(wl)
	if d.lan != nil {
		_ = d.lan.Drop(wl)
	}
	if d.ts != nil {
		_ = d.ts.Drop(ctx, wl)
	}
	// The workspace is only a cache.
	if d.workspacesDir != "" {
		_ = os.RemoveAll(filepath.Join(d.workspacesDir, wl))
	}
	return d.orch.Destroy(ctx, wl, cmd.GetKeepVolumes())
}

func (d *Deployer) List(ctx context.Context) (Inventory, error) {
	return d.orch.List(ctx)
}

// Adopt is a no-op on Docker, whose inventory is the container names themselves. Anything
// adopted is reported at once, so a workload marked missing comes back without a minute's wait.
func (d *Deployer) Adopt(ctx context.Context, cmd *agentv1.Adopt, emit Emit) error {
	a, ok := d.orch.(Adopter)
	if !ok {
		return nil
	}
	pairs := make(map[string]string, len(cmd.GetWorkloads()))
	for _, w := range cmd.GetWorkloads() {
		if w.GetWorkloadId() != "" && w.GetLxcId() != "" {
			pairs[w.GetWorkloadId()] = w.GetLxcId()
		}
	}
	adopted, err := a.Adopt(ctx, pairs)
	if len(adopted) > 0 {
		_ = emit(StateEvent(d.List(ctx)))
	}
	return err
}

// StateEvent reports existing workloads; a failed listing is never marked complete.
func StateEvent(inv Inventory, err error) *agentv1.Event {
	if err != nil {
		inv.Complete = false
	}
	projects := make([]*agentv1.ProjectState, 0, len(inv.IDs))
	for _, id := range inv.IDs {
		projects = append(projects, &agentv1.ProjectState{ProjectId: id})
	}
	return &agentv1.Event{Payload: &agentv1.Event_State{
		State: &agentv1.State{Projects: projects, Complete: inv.Complete},
	}}
}

// Cluster discovery is Proxmox-only; on Docker these return empty inventory.
func (d *Deployer) Nodes(ctx context.Context) []*agentv1.NodeInfo {
	if cd, ok := d.orch.(ClusterDiscovery); ok {
		return cd.Nodes(ctx)
	}
	return nil
}

func (d *Deployer) Templates(ctx context.Context) []string {
	if cd, ok := d.orch.(ClusterDiscovery); ok {
		return cd.Templates(ctx)
	}
	return nil
}

func (d *Deployer) Bridges(ctx context.Context) []string {
	if cd, ok := d.orch.(ClusterDiscovery); ok {
		return cd.Bridges(ctx)
	}
	return nil
}

func (d *Deployer) InvalidateCaches() {
	if cd, ok := d.orch.(ClusterDiscovery); ok {
		cd.InvalidateCaches()
	}
}

func (d *Deployer) Sleep(ctx context.Context, cmd *agentv1.Sleep, emit Emit) error {
	d.rtl.Stop(cmd.GetProjectId())
	if err := d.orch.Stop(ctx, cmd.GetProjectId()); err != nil {
		return err
	}
	// With wake-on-request the waker now answers its traffic; it was already the route target.
	d.waker.MarkAsleep(cmd.GetProjectId())
	return emit(sleepState(cmd.GetProjectId(), true))
}

func (d *Deployer) Wake(ctx context.Context, cmd *agentv1.Wake, emit Emit) error {
	if err := d.orch.Start(ctx, cmd.GetProjectId()); err != nil {
		return err
	}
	d.waker.MarkAwake(cmd.GetProjectId())
	d.rtl.Start(cmd.GetProjectId())
	return emit(sleepState(cmd.GetProjectId(), false))
}

// runHook runs a user command in the workload, like cron, streaming into the deploy log.
func (d *Deployer) runHook(ctx context.Context, wl, command string, timeoutSeconds uint32, logLine LogFunc) error {
	timeout := time.Duration(timeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	hookCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	argv, err := d.orch.ExecArgs(hookCtx, wl, []string{"sh", "-c", command}, false)
	if err != nil {
		return err
	}
	if code := runStreamingCode(hookCtx, argv[0], argv[1:], logLine); code != 0 {
		return fmt.Errorf("exit code %d", code)
	}
	return nil
}

// RunCommand runs a one-off or cron command, streaming output and reporting the exit code.
func (d *Deployer) RunCommand(ctx context.Context, cmd *agentv1.RunCommand, emit Emit) error {
	timeout := time.Duration(cmd.GetTimeoutSeconds()) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	argv, err := d.orch.ExecArgs(runCtx, cmd.GetProjectId(), []string{"sh", "-c", cmd.GetCommand()}, false)
	if err != nil {
		return err
	}
	var seq atomic.Uint64
	onLine := func(stream agentv1.LogStream, text string) {
		_ = emit(&agentv1.Event{
			Payload: &agentv1.Event_RuntimeLog{
				RuntimeLog: &agentv1.RuntimeLogChunk{
					ProjectId: cmd.GetProjectId(),
					RunId:     cmd.GetRunId(),
					Seq:       seq.Add(1),
					Stream:    stream,
					Data:      text,
					Ts:        time.Now().UnixMilli(),
				},
			},
		})
	}
	code := runStreamingCode(runCtx, argv[0], argv[1:], onLine)
	return emit(&agentv1.Event{
		Payload: &agentv1.Event_CommandResult{
			CommandResult: &agentv1.CommandResult{RunId: cmd.GetRunId(), ExitCode: int32(code)},
		},
	})
}

func (d *Deployer) DiskUsage(ctx context.Context, emit Emit) error {
	report, err := d.orch.DiskUsage(ctx)
	if err != nil {
		return err
	}
	return emit(&agentv1.Event{Payload: &agentv1.Event_DiskUsage{DiskUsage: report}})
}

func (d *Deployer) Prune(ctx context.Context, emit Emit) error {
	if err := d.orch.Prune(ctx); err != nil {
		return err
	}
	// Report now so the dashboard shows the reclaimed space.
	return d.DiskUsage(ctx, emit)
}

// CheckImageUpdate queries the registry from the agent, keeping the control plane clear of
// rate limits.
func (d *Deployer) CheckImageUpdate(ctx context.Context, cmd *agentv1.CheckImageUpdate, emit Emit) error {
	current, err := d.orch.ImageDigest(ctx, cmd.GetProjectId(), cmd.GetImage())
	if err != nil {
		return err
	}
	latest, err := registry.LatestDigest(ctx, cmd.GetImage(),
		cmd.GetAuth().GetUsername(), cmd.GetAuth().GetPassword())
	if err != nil {
		return err
	}
	return emit(&agentv1.Event{
		Payload: &agentv1.Event_ImageUpdate{
			ImageUpdate: &agentv1.ImageUpdateStatus{
				ProjectId:     cmd.GetProjectId(),
				CurrentDigest: current,
				LatestDigest:  latest,
			},
		},
	})
}

func (d *Deployer) Sample(ctx context.Context) []*agentv1.ProjectMetrics {
	stats, err := d.orch.StatsAll(ctx)
	if err != nil {
		return nil
	}
	ts := time.Now().UnixMilli()
	out := make([]*agentv1.ProjectMetrics, 0, len(stats))
	for wl, s := range stats {
		out = append(out, &agentv1.ProjectMetrics{
			ProjectId:     wl,
			Ts:            ts,
			CpuPct:        s.CPUPct,
			MemBytes:      s.MemBytes,
			MemLimitBytes: s.MemLimitBytes,
			NetRxBytes:    s.NetRxBytes,
			NetTxBytes:    s.NetTxBytes,
			RestartCount:  s.RestartCount,
		})
	}
	return out
}

func sleepState(projectID string, sleeping bool) *agentv1.Event {
	return &agentv1.Event{
		Payload: &agentv1.Event_SleepState{
			SleepState: &agentv1.SleepStateChanged{ProjectId: projectID, Sleeping: sleeping},
		},
	}
}

// traced wraps a deploy phase in a span, so a slow deploy shows where the time went.
func traced(ctx context.Context, name string, fn func(context.Context) error) error {
	var err error
	c, end := telemetry.Span(ctx, name)
	defer end(&err)
	err = fn(c)
	return err
}
