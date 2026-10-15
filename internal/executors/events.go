package executors

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

// growBackoff grows by 1.5x up to max, so fast workloads are seen quickly without hammering
// slow ones.
func growBackoff(cur, max time.Duration) time.Duration {
	next := cur * 3 / 2
	if next > max {
		return max
	}
	return next
}

func emitLifecycle(emit Emit, deploymentID string, phase agentv1.Phase, detail string) error {
	return emit(&agentv1.Event{
		Payload: &agentv1.Event_Lifecycle{
			Lifecycle: &agentv1.Lifecycle{DeploymentId: deploymentID, Phase: phase, Detail: detail},
		},
	})
}

func logChunk(id string, seq uint64, stream agentv1.LogStream, data string) *agentv1.Event {
	return &agentv1.Event{
		Payload: &agentv1.Event_Log{
			Log: &agentv1.LogChunk{
				DeploymentId: id,
				Seq:          seq,
				Stream:       stream,
				Data:         data,
				Ts:           time.Now().UnixMilli(),
			},
		},
	}
}

func runStreaming(ctx context.Context, dir, name string, args []string, onLine func(agentv1.LogStream, string)) error {
	return runStreamingEnv(ctx, dir, name, args, nil, onLine)
}

// runStreamingEnv adds environment entries, keeping secrets off argv.
func runStreamingEnv(ctx context.Context, dir, name string, args, extraEnv []string, onLine func(agentv1.LogStream, string)) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	return runStreamingCmd(cmd, onLine)
}

func runStreamingCmd(cmd *exec.Cmd, onLine func(agentv1.LogStream, string)) error {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go scanLines(&wg, stdout, agentv1.LogStream_LOG_STREAM_STDOUT, onLine)
	go scanLines(&wg, stderr, agentv1.LogStream_LOG_STREAM_STDERR, onLine)
	wg.Wait()

	return cmd.Wait()
}

// runStreamingCode returns the exit code, or -1 if the command never started.
func runStreamingCode(ctx context.Context, name string, args []string, onLine func(agentv1.LogStream, string)) int {
	if err := runStreaming(ctx, "", name, args, onLine); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode()
		}
		return -1
	}
	return 0
}

func scanLines(wg *sync.WaitGroup, r io.Reader, stream agentv1.LogStream, onLine func(agentv1.LogStream, string)) {
	defer wg.Done()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		onLine(stream, scanner.Text())
	}
	// Drain the rest so the child never blocks on a full pipe and deadlocks Wait.
	if scanner.Err() != nil {
		_, _ = io.Copy(io.Discard, r)
	}
}
