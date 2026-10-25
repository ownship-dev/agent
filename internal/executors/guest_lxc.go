package executors

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

// lxcGuest is today's behaviour: every call is the pct command it always was.
type lxcGuest struct{}

func (lxcGuest) Kind() GuestKind          { return GuestLXC }
func (lxcGuest) Label(vmid string) string { return "LXC " + vmid }

func (lxcGuest) ExecPrefix(_ context.Context, vmid string, _ bool) (GuestPrefix, error) {
	return GuestPrefix{Argv: []string{"pct", "exec", vmid, "--"}}, nil
}

func (lxcGuest) Running(ctx context.Context, vmid string) bool {
	out, _ := pct(ctx, "status", vmid).Output()
	return strings.Contains(string(out), "running")
}

func (lxcGuest) Stopped(ctx context.Context, vmid string) bool {
	out, err := pct(ctx, "status", vmid).Output()
	return err == nil && strings.Contains(string(out), "status: stopped")
}

func (lxcGuest) Start(ctx context.Context, vmid string) error {
	if out, err := pct(ctx, "start", vmid).CombinedOutput(); err != nil {
		return fmt.Errorf("start lxc: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// Ready: pct exec works as soon as the LXC runs.
func (lxcGuest) Ready(context.Context, string) error { return nil }

func (lxcGuest) Shutdown(ctx context.Context, vmid string) error {
	return pct(ctx, "shutdown", vmid).Run()
}

func (lxcGuest) Stop(ctx context.Context, vmid string) error {
	return pct(ctx, "stop", vmid).Run()
}

func (lxcGuest) Reboot(ctx context.Context, vmid string) error {
	if out, err := pct(ctx, "reboot", vmid).CombinedOutput(); err != nil {
		return fmt.Errorf("restart lxc: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// Resize is live on an LXC.
func (lxcGuest) Resize(ctx context.Context, vmid string, res *agentv1.Resources) (bool, error) {
	setArgs := []string{"set", vmid}
	if v := res.GetVcpu(); v > 0 {
		setArgs = append(setArgs, "--cores", strconv.FormatUint(uint64(v), 10))
	}
	if m := res.GetMemoryMb(); m > 0 {
		setArgs = append(setArgs, "--memory", strconv.FormatUint(uint64(m), 10))
	}
	if len(setArgs) > 2 {
		if out, err := pct(ctx, setArgs...).CombinedOutput(); err != nil {
			return false, fmt.Errorf("size lxc: %s", strings.TrimSpace(string(out)))
		}
	}
	return false, nil
}

// SetNetwork changes net0/nameserver; the caller restarts the LXC.
func (lxcGuest) SetNetwork(ctx context.Context, vmid string, n netSpec) error {
	net0, nameservers, err := net0Arg(n.Bridge, n.Network)
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
	return nil
}

// Create clones a golden template when CloneFrom is set, else creates from BaseVolid.
func (lxcGuest) Create(ctx context.Context, req createReq) error {
	if req.CloneFrom != "" {
		args := []string{"clone", req.CloneFrom, req.Vmid, "--hostname", req.Hostname}
		if req.TargetNode != "" {
			args = append(args, "--target", req.TargetNode)
		}
		req.LogLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "cloning LXC "+req.Vmid+" from template "+req.CloneFrom)
		if out, err := pct(ctx, args...).CombinedOutput(); err != nil {
			return fmt.Errorf("clone template: %s", strings.TrimSpace(string(out)))
		}
		// pct clone takes no --tags; the marker is best-effort.
		_ = pct(ctx, "set", req.Vmid, "--tags", ownshipTag).Run()
		return nil
	}
	net0, nameservers, err := net0Arg(req.Net.Bridge, req.Net.Network)
	if err != nil {
		return err
	}
	args := []string{"create", req.Vmid, req.BaseVolid,
		"--hostname", req.Hostname,
		"--cores", req.Cores,
		"--memory", req.MemoryMB,
		"--rootfs", fmt.Sprintf("%s:%d", req.Storage, req.RootfsGB),
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
	req.LogLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "creating LXC "+req.Vmid+" from "+req.BaseVolid)
	if out, err := pct(ctx, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("create lxc: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func (lxcGuest) Destroy(ctx context.Context, vmid string) error {
	_ = pct(ctx, "stop", vmid).Run()
	if out, err := pct(ctx, "destroy", vmid, "--purge").CombinedOutput(); err != nil {
		return fmt.Errorf("destroy lxc: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// IP reads eth0 specifically: `hostname -I` may return an internal Docker bridge.
func (lxcGuest) IP(ctx context.Context, vmid string) (string, error) {
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

func (lxcGuest) Gone(ctx context.Context, vmid string) bool { return lxcGone(ctx, vmid) }

func (lxcGuest) Config(ctx context.Context, vmid string) (string, error) {
	out, err := pct(ctx, "config", vmid).Output()
	return string(out), err
}

// ApplyGPU sets the LXC's devN entries to every host GPU node or none, restarting the LXC
// when they change.
func (lxcGuest) ApplyGPU(ctx context.Context, o *ProxmoxLXC, vmid string, want bool, g gpuDevices, logLine LogFunc) error {
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
	_, err = o.waitForIP(ctx, guestRef{lxcGuest{}, vmid})
	return err
}

func pct(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "pct", args...)
}
