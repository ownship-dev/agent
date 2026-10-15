// Package executors performs the side effects of commands on the homelab.
package executors

import (
	"context"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

// Emit sends an event to the control plane.
type Emit func(*agentv1.Event) error

// Executor performs a command's side effects. Implementations must be idempotent.
type Executor interface {
	Deploy(ctx context.Context, cmd *agentv1.Deploy, emit Emit) error
	Destroy(ctx context.Context, cmd *agentv1.Destroy, emit Emit) error
	RouteUpdate(ctx context.Context, cmd *agentv1.RouteUpdate, emit Emit) error
	NetworkUpdate(ctx context.Context, cmd *agentv1.NetworkUpdate, emit Emit) error
	Sleep(ctx context.Context, cmd *agentv1.Sleep, emit Emit) error
	Wake(ctx context.Context, cmd *agentv1.Wake, emit Emit) error
	RunCommand(ctx context.Context, cmd *agentv1.RunCommand, emit Emit) error
	// Backups stream volume tarballs or engine dumps to and from the user's S3 endpoint.
	Backup(ctx context.Context, cmd *agentv1.Backup, emit Emit) error
	RestoreBackup(ctx context.Context, cmd *agentv1.RestoreBackup, emit Emit) error
	DeleteBackup(ctx context.Context, cmd *agentv1.DeleteBackup, emit Emit) error
	// Read-only volume browsing, answered by request id.
	ListVolumeFiles(ctx context.Context, cmd *agentv1.ListVolumeFiles, emit Emit) error
	ReadVolumeFile(ctx context.Context, cmd *agentv1.ReadVolumeFile, emit Emit) error
	DbQuery(ctx context.Context, cmd *agentv1.DbQuery, emit Emit) error
	DiskUsage(ctx context.Context, emit Emit) error
	// Prune reclaims unused images and build cache, then re-reports disk usage.
	Prune(ctx context.Context, emit Emit) error
	// CheckImageUpdate compares the running image's digest with the registry's for the same tag.
	CheckImageUpdate(ctx context.Context, cmd *agentv1.CheckImageUpdate, emit Emit) error
	List(ctx context.Context) (Inventory, error)
	// Adopt re-tracks workloads the control plane recorded but the agent's state lost.
	Adopt(ctx context.Context, cmd *agentv1.Adopt, emit Emit) error
	LanCAPEM() string
	GPUs(ctx context.Context) []*agentv1.Gpu
	PveVersion(ctx context.Context) string
	Engines(ctx context.Context) []*agentv1.EngineInfo
	Sample(ctx context.Context) []*agentv1.ProjectMetrics
	Nodes(ctx context.Context) []*agentv1.NodeInfo
	Templates(ctx context.Context) []string
	Bridges(ctx context.Context) []string
	InvalidateCaches()
}

// CodedError carries a failure code (e.g. HEALTHCHECK_FAILED) the control plane acts on.
type CodedError struct {
	Code string
	Err  error
}

func (e *CodedError) Error() string { return e.Err.Error() }
func (e *CodedError) Unwrap() error { return e.Err }

const CodeHealthcheckFailed = "HEALTHCHECK_FAILED"

// A failed pre-deploy hook gets the same auto-rollback as a failed health gate.
const CodePreDeployHookFailed = "PRE_DEPLOY_HOOK_FAILED"

// Run tears down the previous workload before the new one is confirmed, so any Run failure
// gets the same auto-rollback.
const CodeRunFailed = "RUN_FAILED"
