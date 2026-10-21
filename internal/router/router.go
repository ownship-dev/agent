// Package router dispatches commands to executors and turns results into events.
package router

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/ownship/agent/internal/config"
	"github.com/ownship/agent/internal/executors"
	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
	"github.com/ownship/agent/internal/telemetry"
	"github.com/ownship/agent/internal/updater"
)

const seenCap = 2048

// Capabilities names commands and behaviours added after the original set, announced in Hello.
// A new live-channel command joins here, or the control plane refuses it.
var Capabilities = []string{"db_query", "self_update", "adopt", "mongo_typed_json", "uninstall", "vm_workloads"}

type Router struct {
	exec executors.Executor
	log  *slog.Logger
	// Removes the agent from the machine; unset in tests.
	Uninstall func() error
	// Builds a fresh Hello; unset in tests.
	Announce func(context.Context) *agentv1.Event

	mu   sync.Mutex
	seen map[string]struct{}
	// A fixed-size ring of recent ids, so memory never grows.
	ring [seenCap]string
	pos  int
	n    int // capped at seenCap
}

func New(exec executors.Executor, log *slog.Logger) *Router {
	return &Router{exec: exec, log: log, seen: make(map[string]struct{})}
}

// firstSee reports whether a command id is new, evicting the oldest when full.
func (r *Router) firstSee(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.seen[id]; ok {
		return false
	}
	if r.n == seenCap {
		delete(r.seen, r.ring[r.pos]) // the slot holds the oldest id
	} else {
		r.n++
	}
	r.ring[r.pos] = id
	r.pos = (r.pos + 1) % seenCap
	r.seen[id] = struct{}{}
	return true
}

// Handle acks every command but runs only new ids. Panics are recovered.
func (r *Router) Handle(ctx context.Context, cmd *agentv1.Command, emit executors.Emit) {
	defer func() {
		if rec := recover(); rec != nil {
			r.log.Error("recovered from panic handling command",
				"id", cmd.GetId(), "deployment_id", cmd.GetDeploy().GetDeploymentId(), "panic", rec)
			// Without the deployment id the control plane can't attach the failure.
			_ = emit(failure(cmd.GetDeploy().GetDeploymentId(), fmt.Errorf("internal agent error")))
		}
	}()

	// Join the control plane's trace so every span and event lands under its deploy.
	ctx = telemetry.FromTraceparent(ctx, cmd.GetTraceContext())
	// Stamp outgoing events so the gateway continues the same trace.
	emit = tracedEmit(ctx, emit)

	if err := emit(ack(cmd.GetId())); err != nil {
		r.log.Error("failed to ack command", "id", cmd.GetId(), "err", err)
	}
	if !r.firstSee(cmd.GetId()) {
		r.log.Debug("duplicate command ignored", "id", cmd.GetId())
		return
	}

	switch cmd.GetPayload().(type) {
	case *agentv1.Command_Deploy:
		r.run(ctx, cmd.GetDeploy().GetDeploymentId(), emit, func(c context.Context) error {
			return r.exec.Deploy(c, cmd.GetDeploy(), emit)
		})
	case *agentv1.Command_Destroy:
		r.run(ctx, "", emit, func(c context.Context) error {
			return r.exec.Destroy(c, cmd.GetDestroy(), emit)
		})
	case *agentv1.Command_RouteUpdate:
		r.run(ctx, "", emit, func(c context.Context) error {
			return r.exec.RouteUpdate(c, cmd.GetRouteUpdate(), emit)
		})
	case *agentv1.Command_NetworkUpdate:
		r.run(ctx, "", emit, func(c context.Context) error {
			return r.exec.NetworkUpdate(c, cmd.GetNetworkUpdate(), emit)
		})
	case *agentv1.Command_Sleep:
		r.run(ctx, "", emit, func(c context.Context) error {
			return r.exec.Sleep(c, cmd.GetSleep(), emit)
		})
	case *agentv1.Command_Wake:
		r.run(ctx, "", emit, func(c context.Context) error {
			return r.exec.Wake(c, cmd.GetWake(), emit)
		})
	case *agentv1.Command_RunCommand:
		r.run(ctx, "", emit, func(c context.Context) error {
			return r.exec.RunCommand(c, cmd.GetRunCommand(), emit)
		})
	case *agentv1.Command_Backup:
		r.run(ctx, "", emit, func(c context.Context) error {
			return r.exec.Backup(c, cmd.GetBackup(), emit)
		})
	case *agentv1.Command_RestoreBackup:
		r.run(ctx, "", emit, func(c context.Context) error {
			return r.exec.RestoreBackup(c, cmd.GetRestoreBackup(), emit)
		})
	case *agentv1.Command_DeleteBackup:
		r.run(ctx, "", emit, func(c context.Context) error {
			return r.exec.DeleteBackup(c, cmd.GetDeleteBackup(), emit)
		})
	case *agentv1.Command_DiskUsage:
		r.run(ctx, "", emit, func(c context.Context) error {
			return r.exec.DiskUsage(c, emit)
		})
	case *agentv1.Command_Prune:
		r.run(ctx, "", emit, func(c context.Context) error {
			return r.exec.Prune(c, emit)
		})
	case *agentv1.Command_CheckImageUpdate:
		r.run(ctx, "", emit, func(c context.Context) error {
			return r.exec.CheckImageUpdate(c, cmd.GetCheckImageUpdate(), emit)
		})
	case *agentv1.Command_ListVolumeFiles:
		r.run(ctx, "", emit, func(c context.Context) error {
			return r.exec.ListVolumeFiles(c, cmd.GetListVolumeFiles(), emit)
		})
	case *agentv1.Command_ReadVolumeFile:
		r.run(ctx, "", emit, func(c context.Context) error {
			return r.exec.ReadVolumeFile(c, cmd.GetReadVolumeFile(), emit)
		})
	case *agentv1.Command_DbQuery:
		r.run(ctx, "", emit, func(c context.Context) error {
			return r.exec.DbQuery(c, cmd.GetDbQuery(), emit)
		})
	case *agentv1.Command_Adopt:
		r.run(ctx, "", emit, func(c context.Context) error {
			return r.exec.Adopt(c, cmd.GetAdopt(), emit)
		})
	case *agentv1.Command_ConvertPrepare, *agentv1.Command_ConvertCommit, *agentv1.Command_ConvertRollback:
		r.convert(ctx, cmd, emit)
	case *agentv1.Command_Uninstall:
		r.uninstall(ctx, cmd.GetUninstall(), emit)
	case *agentv1.Command_AgentUpdate:
		u := cmd.GetAgentUpdate()
		if u.GetDownloadUrl() == "" {
			r.selfUpdate(ctx, emit)
			return
		}
		r.log.Info("applying agent update", "version", u.GetVersion())
		// On success this re-execs and never returns.
		if err := updater.Apply(ctx, u.GetDownloadUrl(), u.GetSignature(), u.GetVersion(), config.Version); err != nil {
			r.log.Error("agent update failed", "err", err)
			_ = emit(failure("", err))
		}
	default:
		r.log.Warn("unhandled command", "id", cmd.GetId())
		_ = emit(failure("", fmt.Errorf("unhandled command type")))
	}
}

// convertExecutor is optional so test executors need not implement it.
type convertExecutor interface {
	ConvertPrepare(ctx context.Context, cmd *agentv1.ConvertPrepare, emit executors.Emit) error
	ConvertCommit(ctx context.Context, cmd *agentv1.ConvertCommit, emit executors.Emit) error
	ConvertRollback(ctx context.Context, cmd *agentv1.ConvertRollback, emit executors.Emit) error
}

func (r *Router) convert(ctx context.Context, cmd *agentv1.Command, emit executors.Emit) {
	c, ok := r.exec.(convertExecutor)
	if !ok {
		_ = emit(failure("", fmt.Errorf("this agent can't convert workloads")))
		return
	}
	r.run(ctx, "", emit, func(ctx context.Context) error {
		switch p := cmd.GetPayload().(type) {
		case *agentv1.Command_ConvertPrepare:
			return c.ConvertPrepare(ctx, p.ConvertPrepare, emit)
		case *agentv1.Command_ConvertCommit:
			return c.ConvertCommit(ctx, p.ConvertCommit, emit)
		case *agentv1.Command_ConvertRollback:
			return c.ConvertRollback(ctx, p.ConvertRollback, emit)
		}
		return nil
	})
}

// uninstall destroys the workloads in order, then removes the agent and exits. A workload that
// fails to destroy aborts it, so the agent stays reachable for a retry.
func (r *Router) uninstall(ctx context.Context, u *agentv1.Uninstall, emit executors.Emit) {
	for _, id := range u.GetWorkloadIds() {
		var err error
		r.run(ctx, "", emit, func(c context.Context) error {
			err = r.exec.Destroy(c, &agentv1.Destroy{ProjectId: id}, emit)
			return err
		})
		if err != nil {
			return
		}
	}
	// VM templates are not workloads, so the control plane never lists them.
	if c, ok := r.exec.(interface{ UninstallCleanup(context.Context) error }); ok {
		if err := c.UninstallCleanup(ctx); err != nil {
			r.log.Error("uninstall cleanup failed", "err", err)
			_ = emit(failure("", err))
			return
		}
	}
	if r.Uninstall == nil {
		return
	}
	if err := r.Uninstall(); err != nil {
		r.log.Error("uninstall failed", "err", err)
		_ = emit(failure("", err))
		return
	}
	r.log.Info("uninstalling agent")
	os.Exit(0)
}

// selfUpdate installs the latest release from the signed channel the hourly loop uses, so a
// pushed update needs no per-binary signature from the control plane.
func (r *Router) selfUpdate(ctx context.Context, emit executors.Emit) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	exe, sum, err := updater.SelfUpdate(ctx, runtime.GOOS, runtime.GOARCH)
	if errors.Is(err, updater.ErrNotNewer) {
		// The control plane still shows an old version, so a lost hello must not stick.
		r.log.Info("agent update requested, already on the latest release", "version", config.Version, "detail", err)
		if r.Announce != nil {
			_ = emit(r.Announce(ctx))
		}
		return
	}
	if err != nil {
		r.log.Error("agent update failed", "err", err)
		_ = emit(failure("", err))
		return
	}
	r.log.Info("updated, restarting into new binary", "binary", exe, "sha256", sum)
	if err := syscall.Exec(exe, os.Args, os.Environ()); err != nil {
		// Exit so systemd restarts on the new binary.
		r.log.Error("re-exec after update failed; exiting for service restart", "err", err)
		os.Exit(0)
	}
}

// commandTimeout stops a hung tool wedging the agent.
const commandTimeout = 30 * time.Minute

// The deployment id is logged so a failure can be linked to its deploy.
func (r *Router) run(ctx context.Context, deploymentID string, emit executors.Emit, fn func(context.Context) error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	var err error
	ctx, end := telemetry.Span(ctx, "agent.command", telemetry.String("deployment.id", deploymentID))
	defer end(&err)
	if err = fn(ctx); err != nil {
		r.log.Error("command execution failed", "deployment_id", deploymentID, "err", err)
		_ = emit(failure(deploymentID, err))
	}
}

// tracedEmit stamps events with the traceparent, captured once since emit is called from
// goroutines without the context.
func tracedEmit(ctx context.Context, emit executors.Emit) executors.Emit {
	tp := telemetry.Traceparent(ctx)
	if tp == "" {
		return emit
	}
	return func(e *agentv1.Event) error {
		e.TraceContext = tp
		return emit(e)
	}
}

func ack(id string) *agentv1.Event {
	return &agentv1.Event{Payload: &agentv1.Event_Ack{Ack: &agentv1.Ack{CommandId: id}}}
}

func failure(deploymentID string, err error) *agentv1.Event {
	// Keep the code so the control plane can react to the failure kind.
	code := "EXEC_FAILED"
	var coded *executors.CodedError
	if errors.As(err, &coded) {
		code = coded.Code
	}
	return &agentv1.Event{
		Payload: &agentv1.Event_Error{
			Error: &agentv1.Failure{
				DeploymentId: deploymentID,
				Code:         code,
				Message:      err.Error(),
				Retryable:    false,
			},
		},
	}
}
