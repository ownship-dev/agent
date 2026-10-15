// Package router dispatches commands to executors and turns results into events.
package router

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ownship/agent/internal/config"
	"github.com/ownship/agent/internal/executors"
	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
	"github.com/ownship/agent/internal/telemetry"
	"github.com/ownship/agent/internal/updater"
)

const seenCap = 2048

type Router struct {
	exec executors.Executor
	log  *slog.Logger

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
	case *agentv1.Command_AgentUpdate:
		u := cmd.GetAgentUpdate()
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
