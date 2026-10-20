package executors

import (
	"context"
	"os/exec"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

// Inventory is a workload listing. Complete means a workload absent from IDs is gone, not unknown.
type Inventory struct {
	IDs      []string
	Complete bool
}

type Target struct {
	URL         string
	ContainerID string
	LxcID       string
}

// RunSpec is everything needed to run one workload: a built Image, or Repo/Build for
// orchestrators that build inside the LXC.
type RunSpec struct {
	WorkloadID string
	Image      string
	Resources  *agentv1.Resources
	Port       uint32
	Env        []*agentv1.EnvEntry
	Volumes    []*agentv1.VolumeMount
	Args       []string
	TargetNode string
	LogLine    LogFunc
	Repo       *agentv1.RepoSource
	// Cache keeps the LXC checkout between deploys; off means a fresh clone each time.
	Cache        bool
	Build        *agentv1.BuildConfig
	BaseTemplate string
	// Golden CT template to clone (Proxmox); empty creates from BaseTemplate.
	TemplateVmid string
	// BaseOS is a curated id resolved to a vztmpl volid; TemplateVmid and BaseTemplate win.
	BaseOS string
	// Registry credentials for the pull; nil is anonymous.
	RegistryAuth *agentv1.RegistryAuth
	// When set, a Basic Auth sidecar owns the published port and the app publishes nothing.
	BasicAuth *agentv1.BasicAuth
	// Pull even when the tag exists locally, so a moved tag lands. Never set on rollbacks.
	PullImage bool
	// Cosmetic hostname; workloads are tracked by WorkloadID.
	Hostname string
	// Proxmox only; nil means DHCP.
	Network *agentv1.NetworkConfig
	// AllowRoot skips forcing a non-root user.
	AllowRoot bool
	// Managed databases need root to init their volume, so non-root is never forced.
	IsManagedService bool
	// LXC root password (Proxmox), re-applied every deploy; empty leaves it untouched.
	LxcPassword string
	// Compose replaces Image; the primary service takes the workload's name, port and env.
	Compose *ComposeRun
	// UNSPECIFIED means docker.
	Engine EngineKind
}

// Orchestrator runs workloads: LocalDocker on the agent host, ProxmoxLXC in one LXC each.
type Orchestrator interface {
	// Backup commands are returned unstarted; stdout/stdin carry the data and callers start/wait.
	// VolumeTarCmd streams the volume as tar.gz.
	VolumeTarCmd(ctx context.Context, workloadID, volumeName string) (*exec.Cmd, error)
	// VolumeUntarCmd wipes the volume, then extracts tar.gz from stdin.
	VolumeUntarCmd(ctx context.Context, workloadID, volumeName string) (*exec.Cmd, error)
	VolumeWriteFileCmd(ctx context.Context, workloadID, volumeName, fileName string) (*exec.Cmd, error)
	// AppExecCmd runs a fixed `sh -c` script in the app container (engine dumps/restores).
	AppExecCmd(ctx context.Context, workloadID, script string) (*exec.Cmd, error)
	// Read-only volume browsing; relPath is pre-validated and no shell is involved.
	VolumeListCmd(ctx context.Context, workloadID, volumeName, relPath string) (*exec.Cmd, error)
	VolumeReadCmd(ctx context.Context, workloadID, volumeName, relPath string) (*exec.Cmd, error)
	Run(ctx context.Context, spec RunSpec) (Target, error)
	// HostBuilds is true when the image is built on the agent host rather than in the LXC.
	HostBuilds() bool
	Destroy(ctx context.Context, workloadID string, keepVolumes bool) error
	// List returns the workloads that exist, so the control plane can GC orphans and notice deletions.
	List(ctx context.Context) (Inventory, error)
	// FollowLogs tails the running container until ctx is cancelled or it stops.
	FollowLogs(ctx context.Context, workloadID string, onLine LogFunc) error
	// Stop and Start are sleep and wake.
	Stop(ctx context.Context, workloadID string) error
	Start(ctx context.Context, workloadID string) error
	// NetworkUpdate reconfigures the network in place; LocalDocker errors.
	NetworkUpdate(ctx context.Context, workloadID string, network *agentv1.NetworkConfig) error
	// StatsAll samples every workload in a constant number of forks.
	StatsAll(ctx context.Context) (map[string]StatsSample, error)
	// ExecArgs builds the argv to run inner in the container; tty requests a PTY.
	ExecArgs(ctx context.Context, workloadID string, inner []string, tty bool) ([]string, error)
	// LxcExecArgs targets the machine hosting the container (the LXC). LocalDocker errors:
	// the agent host is not part of the workload.
	LxcExecArgs(ctx context.Context, workloadID string, inner []string, tty bool) ([]string, error)
	// LinkAddr is a sibling's internal address for private links.
	LinkAddr(ctx context.Context, sourceWorkloadID, targetWorkloadID string, sourceEngine EngineKind, port uint32) (string, error)
	// DialAddr is where the agent itself reaches a workload's port.
	DialAddr(ctx context.Context, workloadID string, port uint32) (string, error)
	DiskUsage(ctx context.Context) (*agentv1.DiskUsageReport, error)
	// Prune reclaims unused images and build cache, never volumes.
	Prune(ctx context.Context) error
	// ImageDigest returns the local digest of an image, "" if not pulled.
	ImageDigest(ctx context.Context, workloadID, image string) (string, error)
}

// ClusterDiscovery is Proxmox-only; callers type-assert and fall back to empty.
type ClusterDiscovery interface {
	Nodes(ctx context.Context) []*agentv1.NodeInfo
	// Templates lists base LXC templates (vztmpl images).
	Templates(ctx context.Context) []string
	Bridges(ctx context.Context) []string
	// InvalidateCaches makes the next report read fresh inventory (on RequestState).
	InvalidateCaches()
	// PveVersion is e.g. "8.2.4", or "" when unreadable.
	PveVersion(ctx context.Context) string
}

// Adopter re-tracks workloads whose local mapping was lost; ProxmoxLXC only.
type Adopter interface {
	Adopt(ctx context.Context, pairs map[string]string) ([]string, error)
}

type LogFunc func(agentv1.LogStream, string)
