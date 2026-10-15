package executors

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

// RuntimeLogs relays live container output. Followers survive reconnects; lines are dropped
// while disconnected.
type RuntimeLogs struct {
	orch Orchestrator
	emit atomic.Value

	mu      sync.Mutex
	cancels map[string]context.CancelFunc
	seqs    map[string]*atomic.Uint64
}

func NewRuntimeLogs(orch Orchestrator) *RuntimeLogs {
	r := &RuntimeLogs{orch: orch, cancels: map[string]context.CancelFunc{}, seqs: map[string]*atomic.Uint64{}}
	r.emit.Store(Emit(func(*agentv1.Event) error { return nil }))
	return r
}

// SetEmit points followers at the current stream on each connect.
func (r *RuntimeLogs) SetEmit(emit Emit) {
	r.emit.Store(emit)
}

func (r *RuntimeLogs) Start(projectID string) {
	r.Stop(projectID)

	ctx, cancel := context.WithCancel(context.Background())
	r.mu.Lock()
	r.cancels[projectID] = cancel
	seq, ok := r.seqs[projectID]
	if !ok {
		seq = &atomic.Uint64{}
		r.seqs[projectID] = seq
	}
	r.mu.Unlock()

	go r.follow(ctx, projectID, seq)
}

func (r *RuntimeLogs) Stop(projectID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cancel, ok := r.cancels[projectID]; ok {
		cancel()
		delete(r.cancels, projectID)
	}
}

// Resync follows everything already running, at agent boot.
func (r *RuntimeLogs) Resync(ctx context.Context) {
	inv, err := r.orch.List(ctx)
	if err != nil {
		return
	}
	for _, id := range inv.IDs {
		r.Start(id)
	}
}

// follow tails the container, re-attaching when it restarts, until stopped or destroyed.
func (r *RuntimeLogs) follow(ctx context.Context, projectID string, seq *atomic.Uint64) {
	onLine := func(stream agentv1.LogStream, text string) {
		emit := r.emit.Load().(Emit)
		_ = emit(&agentv1.Event{
			Payload: &agentv1.Event_RuntimeLog{
				RuntimeLog: &agentv1.RuntimeLogChunk{
					ProjectId: projectID,
					Seq:       seq.Add(1),
					Stream:    stream,
					Data:      text,
					Ts:        time.Now().UnixMilli(),
				},
			},
		})
	}

	const minBackoff = 3 * time.Second
	const maxBackoff = 30 * time.Second
	backoff := minBackoff
	for {
		start := time.Now()
		_ = r.orch.FollowLogs(ctx, projectID, onLine)
		// A tail that ran for a while was healthy; only quick failures grow the backoff.
		if time.Since(start) >= maxBackoff {
			backoff = minBackoff
		}
		// Stop once the workload is destroyed.
		if inv, err := r.orch.List(ctx); err == nil && inv.Complete && !contains(inv.IDs, projectID) {
			r.dropFollower(projectID)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
			// The container may be restarting; retry.
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// dropFollower forgets a follower that gave up.
func (r *RuntimeLogs) dropFollower(projectID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cancels, projectID)
	delete(r.seqs, projectID)
}

func contains(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}
