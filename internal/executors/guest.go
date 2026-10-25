package executors

import (
	"context"
	"os/exec"
	"strings"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

type GuestKind = agentv1.GuestKind

const (
	GuestLXC = agentv1.GuestKind_GUEST_KIND_LXC
	GuestVM  = agentv1.GuestKind_GUEST_KIND_VM
)

// normalizeGuestKind: UNSPECIFIED is an LXC, so older control planes keep today's behaviour.
func normalizeGuestKind(k GuestKind) GuestKind {
	if k == GuestVM {
		return GuestVM
	}
	return GuestLXC
}

// GuestPrefix runs an inner argv inside a guest. With Quote, each inner word is shell-quoted,
// because SSH joins its words and the remote shell parses them again.
type GuestPrefix struct {
	Argv  []string
	Quote bool
}

func (p GuestPrefix) Wrap(inner ...string) []string {
	out := append([]string{}, p.Argv...)
	if !p.Quote {
		return append(out, inner...)
	}
	for _, w := range inner {
		// A shell word can't hold NUL; refuse rather than let the remote side truncate it.
		if strings.ContainsRune(w, 0) {
			return failArgv("refusing a command word containing NUL")
		}
		out = append(out, shellQuote(w))
	}
	return out
}

// netSpec is a validated network request: net0 for an LXC, ipconfig0 for a VM.
type netSpec struct {
	Bridge      string
	Network     *agentv1.NetworkConfig
	Nameservers []string
}

// createReq is everything a guest needs to be created; each kind reads its own fields.
type createReq struct {
	Vmid          string
	WorkloadID    string
	Hostname      string
	Cores         string
	MemoryMB      string
	Net           netSpec
	Storage       string
	RootfsGB      int
	TargetNode    string
	CloneFrom     string // LXC golden template vmid
	BaseVolid     string // LXC vztmpl volid
	VMImage       string // VM image id
	BaseVmid      string // VM template imported from VMImage
	AuthorizedKey string // the agent's VM SSH public key
	LogLine       LogFunc
}

// guest is every Proxmox operation that differs between an LXC and a VM. Everything above it
// (engine install, build, volumes, backups, stats) is shared and runs through ExecPrefix.
type guest interface {
	Kind() GuestKind
	// Label names the guest in user-facing lines ("LXC 105", "VM 105").
	Label(vmid string) string
	ExecPrefix(ctx context.Context, vmid string, tty bool) (GuestPrefix, error)
	Running(ctx context.Context, vmid string) bool
	// Stopped is true only when the status read worked and says stopped, so a failed read is not a shutdown.
	Stopped(ctx context.Context, vmid string) bool
	Start(ctx context.Context, vmid string) error
	// Ready waits until commands can run in a started guest.
	Ready(ctx context.Context, vmid string) error
	// Shutdown is graceful, then forced after a deadline; Stop is immediate.
	Shutdown(ctx context.Context, vmid string) error
	Stop(ctx context.Context, vmid string) error
	Reboot(ctx context.Context, vmid string) error
	// Resize applies cores/memory and reports whether a restart is needed for them to apply.
	Resize(ctx context.Context, vmid string, res *agentv1.Resources) (restartNeeded bool, err error)
	SetNetwork(ctx context.Context, vmid string, n netSpec) error
	Create(ctx context.Context, req createReq) error
	Destroy(ctx context.Context, vmid string) error
	IP(ctx context.Context, vmid string) (string, error)
	// Gone is true only when vmid provably no longer exists as this kind of guest.
	Gone(ctx context.Context, vmid string) bool
	Config(ctx context.Context, vmid string) (string, error)
	ApplyGPU(ctx context.Context, o *ProxmoxLXC, vmid string, want bool, g gpuDevices, logLine LogFunc) error
}

// guestRef is a workload's guest: the strategy and the vmid it names.
type guestRef struct {
	g    guest
	vmid string
}

func (r guestRef) label() string { return r.g.Label(r.vmid) }

// word is the lowercase kind for error text ("lxc", "vm").
func (r guestRef) word() string {
	if r.g.Kind() == GuestVM {
		return "vm"
	}
	return "lxc"
}

// cmd runs inner inside the guest; a guest it can't reach yields a command that fails with why.
func (r guestRef) cmd(ctx context.Context, inner ...string) *exec.Cmd {
	argv := r.argv(ctx, false, inner...)
	return exec.CommandContext(ctx, argv[0], argv[1:]...)
}

func (r guestRef) argv(ctx context.Context, tty bool, inner ...string) []string {
	p, err := r.g.ExecPrefix(ctx, r.vmid, tty)
	if err != nil {
		return failArgv(err.Error())
	}
	return p.Wrap(inner...)
}

// engine places eng inside the guest.
func (r guestRef) engine(ctx context.Context, eng *Engine) *Engine {
	p, err := r.g.ExecPrefix(ctx, r.vmid, false)
	if err != nil {
		c := *eng
		c.unavailable = err.Error()
		return &c
	}
	return eng.InGuest(p)
}
