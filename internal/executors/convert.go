package executors

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

// Convert: Prepare builds the new guest and copies volumes, the next deploy promotes it, and
// Commit or Rollback settles which survives; both stay in vm-state.json until then.

type pendingCtxKey struct{}

var reConvertID = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// withPending makes a create record its guest as the workload's convert target instead of
// as the workload's guest.
func withPending(ctx context.Context) context.Context {
	return context.WithValue(ctx, pendingCtxKey{}, true)
}

func isPending(ctx context.Context) bool {
	v, _ := ctx.Value(pendingCtxKey{}).(bool)
	return v
}

func asideOf(ref guestRef) retiredGuest {
	return retiredGuest{Vmid: ref.vmid, Kind: ref.word()}
}

func asideKind(g retiredGuest) GuestKind {
	if g.Kind == "vm" {
		return GuestVM
	}
	return GuestLXC
}

func (o *ProxmoxLXC) asideRef(g retiredGuest) guestRef {
	return guestRef{o.guestOf(asideKind(g)), g.Vmid}
}

func (o *ProxmoxLXC) trackIn(ctx context.Context, workloadID string, ref guestRef) error {
	if isPending(ctx) {
		return o.vms.setPending(workloadID, asideOf(ref))
	}
	return o.track(workloadID, ref)
}

func (o *ProxmoxLXC) untrackIn(ctx context.Context, workloadID string, ref guestRef) error {
	if isPending(ctx) {
		return o.vms.removePending(workloadID)
	}
	return o.untrack(workloadID, ref)
}

// promotePending swaps a prepared guest of the wanted kind in for the current one, which is
// kept as retired. Called with the workload's create lock held.
func (o *ProxmoxLXC) promotePending(ctx context.Context, workloadID string, cur guestRef, kind GuestKind) (guestRef, bool, error) {
	p, ok := o.vms.pending(workloadID)
	if !ok || asideKind(p) != kind {
		return guestRef{}, false, nil
	}
	next := o.asideRef(p)
	if next.g.Gone(ctx, next.vmid) {
		return guestRef{}, false, o.vms.removePending(workloadID)
	}
	if err := o.vms.setRetired(workloadID, asideOf(cur)); err != nil {
		return guestRef{}, false, err
	}
	if err := o.untrack(workloadID, cur); err != nil {
		return guestRef{}, false, err
	}
	if err := o.track(workloadID, next); err != nil {
		return guestRef{}, false, err
	}
	return next, true, o.vms.removePending(workloadID)
}

// destroyHeld removes an aside guest, but only while its vmid is still an Ownship guest of the
// recorded type; a recycled vmid is left alone.
func (o *ProxmoxLXC) destroyHeld(ctx context.Context, g retiredGuest) error {
	all, err := clusterGuests(ctx)
	if err != nil {
		return err
	}
	info, ok := all[g.Vmid]
	if !ok || !info.is(asideKind(g)) {
		return nil
	}
	if !info.Tagged || info.Base {
		return fmt.Errorf("vmid %s is no longer an Ownship guest; leaving it alone", g.Vmid)
	}
	return o.asideRef(g).g.Destroy(ctx, g.Vmid)
}

// destroyAside removes both convert guests of a workload being deleted.
func (o *ProxmoxLXC) destroyAside(ctx context.Context, workloadID string) error {
	if p, ok := o.vms.pending(workloadID); ok {
		if err := o.destroyHeld(ctx, p); err != nil {
			return err
		}
		if err := o.vms.removePending(workloadID); err != nil {
			return err
		}
	}
	if r, ok := o.vms.retired(workloadID); ok {
		if err := o.destroyHeld(ctx, r); err != nil {
			return err
		}
		return o.vms.removeRetired(workloadID)
	}
	return nil
}

// ConvertRequest is what Prepare needs to create the new guest.
type ConvertRequest struct {
	WorkloadID   string
	To           GuestKind
	VMImage      string
	BaseOS       string
	BaseTemplate string
	TemplateVmid string
	Resources    *agentv1.Resources
	Network      *agentv1.NetworkConfig
	TargetNode   string
	Hostname     string
	Volumes      []string
	LogLine      LogFunc
}

// ConvertPrepare creates (or reuses) the target guest and copies the volumes into it. The old
// guest is shut down first, so a static address is free and the copy is consistent.
func (o *ProxmoxLXC) ConvertPrepare(ctx context.Context, req ConvertRequest) (string, error) {
	defer o.createLocks.lock(req.WorkloadID)()
	if req.LogLine == nil {
		req.LogLine = func(agentv1.LogStream, string) {}
	}
	to := normalizeGuestKind(req.To)
	if err := o.guestConflict(req.WorkloadID); err != nil {
		return "", err
	}
	cur, ok := o.guestFor(req.WorkloadID)
	if !ok {
		return "", fmt.Errorf("no LXC or VM for workload %s", req.WorkloadID)
	}
	if cur.g.Kind() == to {
		// A retry after the deploy already promoted the new guest.
		if _, ok := o.vms.retired(req.WorkloadID); ok {
			return cur.vmid, nil
		}
		return "", fmt.Errorf("this workload already runs in %s", cur.label())
	}
	hosts := make([]string, 0, len(req.Volumes))
	for _, v := range req.Volumes {
		h, err := safeVolumeHostPath(v)
		if err != nil {
			return "", err
		}
		hosts = append(hosts, h)
	}

	stage, err := o.convertStageDir(req.WorkloadID)
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(stage) }()

	_ = o.guestEngine(ctx, req.WorkloadID, cur).Cmd(ctx, "stop", lxcAppContainer).Run()
	if len(hosts) > 0 && !cur.g.Running(ctx, cur.vmid) {
		if err := cur.g.Start(ctx, cur.vmid); err != nil {
			return "", fmt.Errorf("start %s to copy its volumes: %w", cur.label(), err)
		}
	}
	staged := make([]string, len(hosts))
	for i, h := range hosts {
		// A volume the app never wrote to has no directory yet.
		if cur.cmd(ctx, "test", "-d", h).Run() != nil {
			continue
		}
		file := filepath.Join(stage, "vol-"+strconv.Itoa(i)+".tgz")
		if err := convertTarOut(cur.cmd(ctx, "tar", "czf", "-", "-C", h, "."), file); err != nil {
			return "", fmt.Errorf("copy volume %q out of %s: %w", req.Volumes[i], cur.label(), err)
		}
		staged[i] = file
	}
	if err := cur.g.Shutdown(ctx, cur.vmid); err != nil {
		return "", fmt.Errorf("shut down %s: %w", cur.label(), err)
	}

	next, err := o.convertTarget(ctx, req, to)
	if err != nil {
		return "", err
	}
	for i, file := range staged {
		if file == "" {
			continue
		}
		h := hosts[i]
		in := next.cmd(ctx, "sh", "-c", "mkdir -p "+h+" && find "+h+" -mindepth 1 -delete && tar xzf - -C "+h)
		if err := convertTarIn(in, file); err != nil {
			return "", fmt.Errorf("copy volume %q into %s: %w", req.Volumes[i], next.label(), err)
		}
	}
	req.LogLine(agentv1.LogStream_LOG_STREAM_SYSTEM_OK, "prepared "+next.label()+" with "+strconv.Itoa(len(hosts))+" volume(s) from "+cur.label())
	return next.vmid, nil
}

// convertTarget reuses a prepared guest that still exists, else creates one as pending.
func (o *ProxmoxLXC) convertTarget(ctx context.Context, req ConvertRequest, to GuestKind) (guestRef, error) {
	pctx := withPending(ctx)
	if p, ok := o.vms.pending(req.WorkloadID); ok {
		ref := o.asideRef(p)
		if asideKind(p) == to && !ref.g.Gone(ctx, ref.vmid) {
			return ref, o.startAndTrack(pctx, ref, req.WorkloadID)
		}
		if err := o.destroyHeld(ctx, p); err != nil {
			return guestRef{}, err
		}
		if err := o.vms.removePending(req.WorkloadID); err != nil {
			return guestRef{}, err
		}
	}
	if to == GuestVM {
		return o.createVM(pctx, RunSpec{WorkloadID: req.WorkloadID, GuestKind: GuestVM, VmImage: req.VMImage,
			Resources: req.Resources, Network: req.Network, Hostname: req.Hostname, LogLine: req.LogLine})
	}
	return o.createLXC(pctx, req.WorkloadID, req.BaseTemplate, req.TemplateVmid, req.BaseOS, req.Resources,
		req.TargetNode, req.Hostname, req.Network, req.LogLine)
}

// convertStageDir sits beside the state files, on the host's disk rather than a tmpfs.
func (o *ProxmoxLXC) convertStageDir(workloadID string) (string, error) {
	if !reConvertID.MatchString(workloadID) {
		return "", fmt.Errorf("invalid workload id %q", workloadID)
	}
	dir := filepath.Join(filepath.Dir(o.vms.path), "convert", workloadID)
	if err := os.RemoveAll(dir); err != nil {
		return "", err
	}
	return dir, os.MkdirAll(dir, 0o700)
}

func convertTarOut(cmd *exec.Cmd, file string) error {
	f, err := os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	cmd.Stdout = f
	if err := runWithStderr(cmd); err != nil {
		return err
	}
	return f.Sync()
}

func convertTarIn(cmd *exec.Cmd, file string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	cmd.Stdin = f
	return runWithStderr(cmd)
}

func runWithStderr(cmd *exec.Cmd) error {
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%v %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// ConvertCommit destroys the guest the workload left behind.
func (o *ProxmoxLXC) ConvertCommit(ctx context.Context, workloadID string) error {
	defer o.createLocks.lock(workloadID)()
	r, ok := o.vms.retired(workloadID)
	if !ok {
		return nil
	}
	if cur, ok := o.guestFor(workloadID); ok && cur.vmid == r.Vmid {
		return fmt.Errorf("%s is still the workload's guest; nothing was promoted", cur.label())
	}
	if err := o.destroyHeld(ctx, r); err != nil {
		return err
	}
	return o.vms.removeRetired(workloadID)
}

// ConvertRollback destroys the new guest and brings the old one back with its app.
func (o *ProxmoxLXC) ConvertRollback(ctx context.Context, workloadID string) error {
	defer o.createLocks.lock(workloadID)()
	if p, ok := o.vms.pending(workloadID); ok {
		if err := o.destroyHeld(ctx, p); err != nil {
			return err
		}
		if err := o.vms.removePending(workloadID); err != nil {
			return err
		}
	}
	if r, ok := o.vms.retired(workloadID); ok {
		old := o.asideRef(r)
		if cur, ok := o.guestFor(workloadID); ok && cur.vmid != r.Vmid {
			if err := o.destroyHeld(ctx, asideOf(cur)); err != nil {
				return err
			}
			if err := o.untrack(workloadID, cur); err != nil {
				return err
			}
		}
		if err := o.track(workloadID, old); err != nil {
			return err
		}
		if err := o.vms.removeRetired(workloadID); err != nil {
			return err
		}
	}
	ref, ok := o.guestFor(workloadID)
	if !ok || ref.g.Gone(ctx, ref.vmid) {
		return fmt.Errorf("the original guest of workload %s no longer exists", workloadID)
	}
	if !ref.g.Running(ctx, ref.vmid) {
		if err := ref.g.Start(ctx, ref.vmid); err != nil {
			return err
		}
	}
	if _, err := o.waitForIP(ctx, ref); err != nil {
		return err
	}
	return o.guestEngine(ctx, workloadID, ref).Cmd(ctx, "start", lxcAppContainer).Run()
}

// Converter is the orchestrator side of a convert; ProxmoxLXC only.
type Converter interface {
	ConvertPrepare(ctx context.Context, req ConvertRequest) (string, error)
	ConvertCommit(ctx context.Context, workloadID string) error
	ConvertRollback(ctx context.Context, workloadID string) error
}

var errNoConvert = fmt.Errorf("this machine can't convert workloads between an LXC and a VM")

// Each step answers with a ConvertResult, failed or not, so the migration can move on.
func convertResult(migrationID, workloadID, step, vmid string, err error) *agentv1.Event {
	r := &agentv1.ConvertResult{MigrationId: migrationID, WorkloadId: workloadID, Step: step, Ok: err == nil, Vmid: vmid}
	if err != nil {
		r.Error = err.Error()
	}
	return &agentv1.Event{Payload: &agentv1.Event_ConvertResult{ConvertResult: r}}
}

func (d *Deployer) ConvertPrepare(ctx context.Context, cmd *agentv1.ConvertPrepare, emit Emit) error {
	vmid, err := "", errNoConvert
	if c, ok := d.orch.(Converter); ok {
		vmid, err = c.ConvertPrepare(ctx, ConvertRequest{
			WorkloadID: cmd.GetWorkloadId(), To: cmd.GetToKind(), VMImage: cmd.GetVmImage(),
			BaseOS: cmd.GetBaseOs(), BaseTemplate: cmd.GetBaseTemplate(), TemplateVmid: cmd.GetTemplateVmid(),
			Resources: cmd.GetResources(), Network: cmd.GetNetwork(), TargetNode: cmd.GetTargetNode(),
			Hostname: cmd.GetHostname(), Volumes: cmd.GetVolumeNames(),
		})
	}
	return emit(convertResult(cmd.GetMigrationId(), cmd.GetWorkloadId(), "prepare", vmid, err))
}

func (d *Deployer) ConvertCommit(ctx context.Context, cmd *agentv1.ConvertCommit, emit Emit) error {
	err := errNoConvert
	if c, ok := d.orch.(Converter); ok {
		err = c.ConvertCommit(ctx, cmd.GetWorkloadId())
	}
	return emit(convertResult(cmd.GetMigrationId(), cmd.GetWorkloadId(), "commit", "", err))
}

func (d *Deployer) ConvertRollback(ctx context.Context, cmd *agentv1.ConvertRollback, emit Emit) error {
	err := errNoConvert
	if c, ok := d.orch.(Converter); ok {
		err = c.ConvertRollback(ctx, cmd.GetWorkloadId())
	}
	if err == nil {
		_ = emit(StateEvent(d.List(ctx)))
	}
	return emit(convertResult(cmd.GetMigrationId(), cmd.GetWorkloadId(), "rollback", "", err))
}
