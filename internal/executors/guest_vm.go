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
	"strconv"
	"strings"
	"sync"
	"time"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

// qmOutput runs qm; tests replace it, since no Proxmox host is available to them.
var qmOutput = func(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "qm", args...).CombinedOutput()
}

// qmCmd is for callers that stream or pipe qm.
var qmCmd = func(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "qm", args...)
}

func qmErr(what string, out []byte, err error) error {
	if msg := strings.TrimSpace(string(out)); msg != "" {
		return fmt.Errorf("%s: %s", what, msg)
	}
	return fmt.Errorf("%s: %w", what, err)
}

// errNoGuestAgent names the image requirement, since a silent agent is the usual first failure.
func errNoGuestAgent(vmid string) error {
	return fmt.Errorf("VM %s booted but its qemu-guest-agent does not answer; the image needs qemu-guest-agent installed and enabled (see the suggested images doc)", vmid)
}

// vmAgentPing is the guest agent answering over virtio-serial, not the network.
func vmAgentPing(ctx context.Context, vmid string) error {
	if out, err := qmOutput(ctx, "agent", vmid, "ping"); err != nil {
		return qmErr("guest agent ping", out, err)
	}
	return nil
}

// vmGuestExec runs argv through the guest agent and returns its stdout. Only for bootstrap
// and recovery: it buffers output and can't stream stdin.
func vmGuestExec(ctx context.Context, vmid string, argv ...string) (string, error) {
	out, err := qmOutput(ctx, append([]string{"guest", "exec", vmid, "--timeout", "60", "--"}, argv...)...)
	if err != nil {
		return "", qmErr("guest exec", out, err)
	}
	var res struct {
		ExitCode int    `json:"exitcode"`
		Exited   int    `json:"exited"`
		OutData  string `json:"out-data"`
		ErrData  string `json:"err-data"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return "", fmt.Errorf("guest exec: unparseable reply")
	}
	if res.Exited != 1 {
		return "", fmt.Errorf("guest exec: %s did not finish", argv[0])
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("guest exec: %s exited %d: %s", argv[0], res.ExitCode, strings.TrimSpace(res.ErrData))
	}
	return res.OutData, nil
}

// Interfaces whose addresses are never the VM's LAN address.
var vmIgnoredIfaces = []string{"lo", "docker", "br-", "veth", "podman", "cni", "virbr", "tailscale"}

// vmGuestIPv4 asks the guest agent for the first LAN IPv4, validated before it reaches argv.
func vmGuestIPv4(ctx context.Context, vmid string) (string, error) {
	out, err := qmOutput(ctx, "agent", vmid, "network-get-interfaces")
	if err != nil {
		return "", qmErr("guest agent network-get-interfaces", out, err)
	}
	var ifaces []struct {
		Name string `json:"name"`
		IPs  []struct {
			Addr string `json:"ip-address"`
			Type string `json:"ip-address-type"`
		} `json:"ip-addresses"`
	}
	if err := json.Unmarshal(out, &ifaces); err != nil {
		return "", fmt.Errorf("guest agent network-get-interfaces: unparseable reply")
	}
	for _, it := range ifaces {
		if ignoredIface(it.Name) {
			continue
		}
		for _, a := range it.IPs {
			ip := net.ParseIP(a.Addr)
			if a.Type != "ipv4" || ip == nil || ip.To4() == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			if validIPv4(ip.String()) {
				return ip.String(), nil
			}
		}
	}
	return "", fmt.Errorf("no IP yet")
}

func ignoredIface(name string) bool {
	for _, p := range vmIgnoredIfaces {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// vmPinHostKey reads the guest's ed25519 host key through the guest agent and pins it under
// the vmid's alias. No trust on first use over the network.
func vmPinHostKey(ctx context.Context, p sshPaths, vmid string) (string, error) {
	raw, err := vmGuestExec(ctx, vmid, "cat", "/etc/ssh/ssh_host_ed25519_key.pub")
	if err != nil {
		return "", fmt.Errorf("read the VM's SSH host key: %w", err)
	}
	keyLine, fpr, err := parseHostKey(raw)
	if err != nil {
		return "", err
	}
	if err := pinHostKey(p.KnownHosts, hostKeyAlias(vmid), keyLine); err != nil {
		return "", err
	}
	return fpr, nil
}

// vmInstallKey replaces the ownship user's authorized_keys with the agent's key, which also
// drops a key the agent lost in a reinstall. The key is the agent's own, never user input.
func vmInstallKey(ctx context.Context, vmid, authorized string) error {
	const script = `set -e
home=$(getent passwd ` + vmUser + ` | cut -d: -f6)
[ -n "$home" ] || { echo "user ` + vmUser + ` is missing; the image needs cloud-init" >&2; exit 1; }
umask 077
mkdir -p "$home/.ssh"
printf '%s
' "$1" > "$home/.ssh/authorized_keys.ownship"
mv "$home/.ssh/authorized_keys.ownship" "$home/.ssh/authorized_keys"
chown -R ` + vmUser + `: "$home/.ssh"`
	_, err := vmGuestExec(ctx, vmid, "sh", "-c", script, "sh", authorized)
	return err
}

// Readiness waits; tests shorten them.
var (
	vmReadyTimeout   = 3 * time.Minute
	vmRekeyAfter     = 45 * time.Second
	vmShutdownWait   = "60"
	vmIPCacheTTL     = 30 * time.Second
	vmPollMaxBackoff = 3 * time.Second
)

// vmSSHProbe checks that SSH with the pinned key reaches sudo in the guest.
var vmSSHProbe = func(ctx context.Context, argv []string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, argv[0], argv[1:]...).Run()
}

var reMAC = regexp.MustCompile(`^[0-9A-Fa-f]{2}(:[0-9A-Fa-f]{2}){5}$`)

// vmGuest runs a workload in a QEMU VM: a full clone of a template imported from a local
// image, reached over pinned-host-key SSH with the guest agent as bootstrap channel.
type vmGuest struct {
	o     *ProxmoxLXC
	paths sshPaths

	ipMu sync.Mutex
	ips  map[string]vmIP
}

type vmIP struct {
	ip string
	at time.Time
}

func newVMGuest(o *ProxmoxLXC) *vmGuest {
	dir := filepath.Dir(o.cfg.VMStatePath)
	if o.cfg.VMStatePath == "" {
		dir = filepath.Dir(o.cfg.StatePath)
	}
	return &vmGuest{o: o, paths: newSSHPaths(dir), ips: map[string]vmIP{}}
}

func (*vmGuest) Kind() GuestKind          { return GuestVM }
func (*vmGuest) Label(vmid string) string { return "VM " + vmid }

func (g *vmGuest) ExecPrefix(ctx context.Context, vmid string, tty bool) (GuestPrefix, error) {
	if !reVmid.MatchString(vmid) {
		return GuestPrefix{}, fmt.Errorf("invalid vmid %q", vmid)
	}
	ip, err := g.IP(ctx, vmid)
	if err != nil {
		return GuestPrefix{}, fmt.Errorf("VM %s has no address yet: %w", vmid, err)
	}
	return sshPrefix(g.paths, vmid, ip, tty), nil
}

// owned is the only gate before a stop or destroy: a qemu guest with the ownship tag. A vmid
// recycled by an LXC, a template, or someone else's VM is never touched.
func (g *vmGuest) owned(ctx context.Context, vmid string) error {
	all, err := clusterGuests(ctx)
	if err != nil {
		return err
	}
	info, ok := all[vmid]
	if !ok || !info.is(GuestVM) || !info.Tagged || info.Base {
		return fmt.Errorf("vmid %s is not an Ownship VM; leaving it alone", vmid)
	}
	return nil
}

func (g *vmGuest) Running(ctx context.Context, vmid string) bool {
	out, _ := qmOutput(ctx, "status", vmid)
	return strings.Contains(string(out), "status: running")
}

func (g *vmGuest) Stopped(ctx context.Context, vmid string) bool {
	out, err := qmOutput(ctx, "status", vmid)
	return err == nil && strings.Contains(string(out), "status: stopped")
}

// Start boots the VM and waits until the guest agent answers and SSH works.
func (g *vmGuest) Start(ctx context.Context, vmid string) error {
	g.forgetIP(vmid)
	if !g.Running(ctx, vmid) {
		if out, err := qmOutput(ctx, "start", vmid); err != nil {
			return qmErr("start vm", out, err)
		}
	}
	return g.Ready(ctx, vmid)
}

// Ready waits for the guest agent, pins the host key through it, and confirms SSH. A key the
// guest no longer accepts (agent reinstalled) is replaced through the guest agent.
func (g *vmGuest) Ready(ctx context.Context, vmid string) error {
	authorized, _, err := ensureVMKey(g.paths)
	if err != nil {
		return fmt.Errorf("VM SSH key: %w", err)
	}
	start := time.Now()
	deadline := start.Add(vmReadyTimeout)
	backoff := 250 * time.Millisecond
	agentUp, rekeyed := false, false
	var last error
	for {
		switch {
		case !agentUp:
			if last = vmAgentPing(ctx, vmid); last == nil {
				agentUp = true
				continue
			}
		case !g.pinned(vmid):
			var fpr string
			if fpr, last = vmPinHostKey(ctx, g.paths, vmid); last == nil {
				_ = g.o.vms.setHostKey(vmid, fpr)
				continue
			}
		default:
			p, err := g.ExecPrefix(ctx, vmid, false)
			if err == nil {
				if last = vmSSHProbe(ctx, p.Wrap("true")); last == nil {
					return nil
				}
			} else {
				last = err
			}
			// First boot runs cloud-init after the agent starts; only re-key once it had time.
			if !rekeyed && time.Since(start) > vmRekeyAfter {
				if err := vmInstallKey(ctx, vmid, authorized); err == nil {
					rekeyed = true
					_, _ = vmPinHostKey(ctx, g.paths, vmid)
					continue
				}
			}
		}
		if time.Now().After(deadline) {
			if !agentUp {
				return errNoGuestAgent(vmid)
			}
			return fmt.Errorf("VM %s is up but SSH as %s does not work (%v); the image needs cloud-init, openssh-server and sudo", vmid, vmUser, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = growBackoff(backoff, vmPollMaxBackoff)
	}
}

func (g *vmGuest) pinned(vmid string) bool {
	data, err := os.ReadFile(g.paths.KnownHosts)
	if err != nil {
		return false
	}
	alias := hostKeyAlias(vmid) + " "
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, alias) {
			return true
		}
	}
	return false
}

func (g *vmGuest) Shutdown(ctx context.Context, vmid string) error {
	if err := g.owned(ctx, vmid); err != nil {
		return err
	}
	g.forgetIP(vmid)
	if out, err := qmOutput(ctx, "shutdown", vmid, "--timeout", vmShutdownWait, "--forceStop", "1"); err != nil {
		return qmErr("shut down vm", out, err)
	}
	return nil
}

func (g *vmGuest) Stop(ctx context.Context, vmid string) error {
	if err := g.owned(ctx, vmid); err != nil {
		return err
	}
	g.forgetIP(vmid)
	if out, err := qmOutput(ctx, "stop", vmid); err != nil {
		return qmErr("stop vm", out, err)
	}
	return nil
}

// Reboot shuts down and starts again, which also applies pending config such as memory.
func (g *vmGuest) Reboot(ctx context.Context, vmid string) error {
	if err := g.owned(ctx, vmid); err != nil {
		return err
	}
	g.forgetIP(vmid)
	if out, err := qmOutput(ctx, "reboot", vmid); err != nil {
		return qmErr("restart vm", out, err)
	}
	return g.Ready(ctx, vmid)
}

// Resize applies cores and memory (a VM needs a restart for them) and grows the disk.
func (g *vmGuest) Resize(ctx context.Context, vmid string, res *agentv1.Resources) (bool, error) {
	cfg, err := g.Config(ctx, vmid)
	if err != nil {
		return false, err
	}
	cur := parseQMConfig(cfg)
	args := []string{"set", vmid}
	if v := res.GetVcpu(); v > 0 && cur["cores"] != strconv.FormatUint(uint64(v), 10) {
		args = append(args, "--cores", strconv.FormatUint(uint64(v), 10))
	}
	if m := res.GetMemoryMb(); m > 0 && cur["memory"] != strconv.FormatUint(uint64(m), 10) {
		args = append(args, "--memory", strconv.FormatUint(uint64(m), 10))
	}
	changed := len(args) > 2
	if changed {
		if out, err := qmOutput(ctx, args...); err != nil {
			return false, qmErr("size vm", out, err)
		}
	}
	if err := g.growDisk(ctx, vmid, cur["scsi0"]); err != nil {
		return false, err
	}
	return changed, nil
}

// growDisk grows scsi0 to the configured size; Proxmox can't shrink, so a smaller size is left.
func (g *vmGuest) growDisk(ctx context.Context, vmid, scsi0 string) error {
	want := g.o.cfg.RootfsGB
	if want <= 0 {
		return nil
	}
	if have, ok := diskSizeGB(scsi0); ok && have >= want {
		return nil
	}
	if out, err := qmOutput(ctx, "resize", vmid, "scsi0", fmt.Sprintf("%dG", want)); err != nil {
		return qmErr("grow vm disk", out, err)
	}
	return nil
}

// SetNetwork sets net0 (keeping the MAC, so a DHCP lease survives) and cloud-init's
// ipconfig0; the caller restarts the VM, and cloud-init applies it on boot.
func (g *vmGuest) SetNetwork(ctx context.Context, vmid string, n netSpec) error {
	net0, ipconfig0, nameservers, err := vmNetArgs(n.Bridge, n.Network)
	if err != nil {
		return err
	}
	cfg, err := g.Config(ctx, vmid)
	if err != nil {
		return err
	}
	if mac := netMAC(parseQMConfig(cfg)["net0"]); mac != "" {
		net0 = strings.Replace(net0, "virtio,", "virtio="+mac+",", 1)
	}
	args := []string{"set", vmid, "--net0", net0, "--ipconfig0", ipconfig0}
	if len(nameservers) > 0 {
		args = append(args, "--nameserver", strings.Join(nameservers, " "))
	} else {
		args = append(args, "--delete", "nameserver")
	}
	if out, err := qmOutput(ctx, args...); err != nil {
		return qmErr("qm set network", out, err)
	}
	if out, err := qmOutput(ctx, "cloudinit", "update", vmid); err != nil {
		return qmErr("qm cloudinit update", out, err)
	}
	g.forgetIP(vmid)
	return nil
}

// Create full-clones the template (it survives template cleanup and deletes cleanly). It only
// claims the vmid; configure sets the rest once the vmid lock is released.
func (g *vmGuest) Create(ctx context.Context, req createReq) error {
	if !reVmid.MatchString(req.Vmid) || !reVmid.MatchString(req.BaseVmid) {
		return fmt.Errorf("invalid vmid")
	}
	if !reHostname.MatchString(req.Hostname) || !validPctArg(req.Storage, reNode) || req.Storage == "" {
		return fmt.Errorf("invalid hostname or storage")
	}
	req.LogLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "cloning VM "+req.Vmid+" from template "+req.BaseVmid+" ("+req.VMImage+")")
	if out, err := qmOutput(ctx, "clone", req.BaseVmid, req.Vmid, "--name", req.Hostname, "--full", "1", "--storage", req.Storage); err != nil {
		return qmErr("clone vm", out, err)
	}
	return nil
}

// configure sets every option through Proxmox's native cloud-init fields, so no user text
// ever reaches cloud-init user-data. The SSH key is the agent's own.
func (g *vmGuest) configure(ctx context.Context, req createReq) error {
	if !strings.HasPrefix(req.AuthorizedKey, "ssh-ed25519 ") || strings.ContainsAny(req.AuthorizedKey, "\n\r") {
		return fmt.Errorf("invalid agent key")
	}
	net0, ipconfig0, nameservers, err := vmNetArgs(req.Net.Bridge, req.Net.Network)
	if err != nil {
		return err
	}
	if !reVmid.MatchString(req.Cores) || !reVmid.MatchString(req.MemoryMB) {
		return fmt.Errorf("invalid cores or memory")
	}
	keyFile, err := os.CreateTemp("", "ownship-vm-key-*.pub")
	if err != nil {
		return err
	}
	defer os.Remove(keyFile.Name())
	if _, err := keyFile.WriteString(req.AuthorizedKey + "\n"); err != nil {
		_ = keyFile.Close()
		return err
	}
	_ = keyFile.Close()
	args := []string{"set", req.Vmid,
		"--tags", ownshipTag,
		"--cores", req.Cores,
		"--memory", req.MemoryMB,
		"--balloon", "0",
		"--cpu", "host",
		"--agent", "enabled=1",
		"--onboot", "1",
		"--net0", net0,
		"--ipconfig0", ipconfig0,
		"--ciuser", vmUser,
		"--sshkeys", keyFile.Name(),
	}
	if len(nameservers) > 0 {
		args = append(args, "--nameserver", strings.Join(nameservers, " "))
	}
	if ref, err := g.o.ensureVendorSnippet(ctx); err == nil {
		args = append(args, "--cicustom", "vendor="+ref)
	} else {
		req.LogLine(agentv1.LogStream_LOG_STREAM_SYSTEM_WARN, "booting without the guest-agent snippet ("+err.Error()+"); the image must enable qemu-guest-agent itself")
	}
	if out, err := qmOutput(ctx, args...); err != nil {
		return qmErr("configure vm", out, err)
	}
	cfg, err := g.Config(ctx, req.Vmid)
	if err != nil {
		return err
	}
	return g.growDisk(ctx, req.Vmid, parseQMConfig(cfg)["scsi0"])
}

// Destroy removes the VM only when it is provably an Ownship VM; otherwise only the caller's
// mapping goes.
func (g *vmGuest) Destroy(ctx context.Context, vmid string) error {
	if err := g.owned(ctx, vmid); err != nil {
		return nil
	}
	g.unregisterRHEL(ctx, vmid)
	_, _ = qmOutput(ctx, "stop", vmid)
	if out, err := qmOutput(ctx, "destroy", vmid, "--purge", "--destroy-unreferenced-disks", "1"); err != nil {
		return qmErr("destroy vm", out, err)
	}
	g.forgetIP(vmid)
	_ = unpinHostKey(g.paths.KnownHosts, hostKeyAlias(vmid))
	return nil
}

// discard removes a clone this agent just made, before configure could tag it as owned.
func (g *vmGuest) discard(vmid string) {
	ctx := context.Background()
	_, _ = qmOutput(ctx, "stop", vmid)
	_, _ = qmOutput(ctx, "destroy", vmid, "--purge", "--destroy-unreferenced-disks", "1")
	g.forgetIP(vmid)
	_ = unpinHostKey(g.paths.KnownHosts, hostKeyAlias(vmid))
}

// IP comes from the guest agent, cached briefly since every exec needs it.
func (g *vmGuest) IP(ctx context.Context, vmid string) (string, error) {
	g.ipMu.Lock()
	c, ok := g.ips[vmid]
	g.ipMu.Unlock()
	if ok && time.Since(c.at) < vmIPCacheTTL {
		return c.ip, nil
	}
	ip, err := vmGuestIPv4(ctx, vmid)
	if err != nil {
		return "", err
	}
	g.ipMu.Lock()
	g.ips[vmid] = vmIP{ip: ip, at: time.Now()}
	g.ipMu.Unlock()
	return ip, nil
}

func (g *vmGuest) forgetIP(vmid string) {
	g.ipMu.Lock()
	delete(g.ips, vmid)
	g.ipMu.Unlock()
}

func (g *vmGuest) Gone(ctx context.Context, vmid string) bool { return guestGone(ctx, GuestVM, vmid) }

func (g *vmGuest) Config(ctx context.Context, vmid string) (string, error) {
	out, err := qmOutput(ctx, "config", vmid)
	if err != nil {
		return "", qmErr("read vm config", out, err)
	}
	return string(out), nil
}

// ApplyGPU: a VM would need exclusive PCI passthrough, which v1 does not offer.
func (*vmGuest) ApplyGPU(_ context.Context, _ *ProxmoxLXC, _ string, want bool, _ gpuDevices, _ LogFunc) error {
	if want {
		return errors.New("GPU passthrough is only offered on LXC")
	}
	return nil
}

// parseQMConfig reads `qm config` lines ("key: value").
func parseQMConfig(cfg string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(cfg, "\n") {
		if k, v, ok := strings.Cut(line, ": "); ok && !strings.HasPrefix(k, "#") {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}

// diskSizeGB reads size= from a disk line ("local-lvm:vm-201-disk-0,size=2G").
func diskSizeGB(disk string) (int, bool) {
	for _, part := range strings.Split(disk, ",") {
		v, ok := strings.CutPrefix(part, "size=")
		if !ok {
			continue
		}
		mult := 1.0
		switch {
		case strings.HasSuffix(v, "T"):
			mult, v = 1024, strings.TrimSuffix(v, "T")
		case strings.HasSuffix(v, "G"):
			v = strings.TrimSuffix(v, "G")
		case strings.HasSuffix(v, "M"):
			mult, v = 1.0/1024, strings.TrimSuffix(v, "M")
		default:
			return 0, false
		}
		n, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 0, false
		}
		return int(n * mult), true
	}
	return 0, false
}

// netMAC reads the MAC from net0 ("virtio=BC:24:11:AA:BB:CC,bridge=vmbr0").
func netMAC(net0 string) string {
	first, _, _ := strings.Cut(net0, ",")
	if mac, ok := strings.CutPrefix(first, "virtio="); ok && reMAC.MatchString(mac) {
		return mac
	}
	return ""
}
