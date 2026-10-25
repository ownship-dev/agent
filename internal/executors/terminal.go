package executors

import (
	"context"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"

	"github.com/creack/pty"
	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

// TerminalManager runs PTY sessions by id, outside the durable path: keystrokes must not replay.
type TerminalManager struct {
	orch Orchestrator
	emit atomic.Value

	mu       sync.Mutex
	sessions map[string]*ptySession
}

type ptySession struct {
	ptmx   *os.File
	cancel context.CancelFunc
	// Input frames in arrival order; one goroutine applies them so keystrokes never reorder.
	in   chan *agentv1.TerminalInput
	done chan struct{}
}

// Pending input frames per session before the receive loop blocks.
const terminalInputBuffer = 256

func NewTerminalManager(orch Orchestrator) *TerminalManager {
	m := &TerminalManager{orch: orch, sessions: map[string]*ptySession{}}
	m.emit.Store(Emit(func(*agentv1.Event) error { return nil }))
	return m
}

// SetEmit points sessions at the current stream on each connect.
func (m *TerminalManager) SetEmit(emit Emit) {
	m.emit.Store(emit)
}

// Handle runs in wire order. Only open spawns a goroutine; other frames go to the session's
// ordered channel.
func (m *TerminalManager) Handle(ctx context.Context, in *agentv1.TerminalInput) {
	if in.GetOpen() {
		go m.open(ctx, in)
		return
	}
	m.mu.Lock()
	s := m.sessions[in.GetSessionId()]
	m.mu.Unlock()
	if s == nil {
		return
	}
	select {
	case s.in <- in:
	case <-s.done:
	case <-ctx.Done():
	}
}

// shellBootstrap opens the best interactive shell the image has, with a prompt that shows where
// you are (user@host:~/dir, # for root, $ otherwise) instead of a bare "#". Constant, so nothing user-given is parsed.
const shellBootstrap = `export TERM=xterm-256color COLORTERM=truecolor
export LANG="${LANG:-C.UTF-8}" HOME="${HOME:-/root}"
p='\[\033[1;32m\]\u@\h\[\033[0m\]:\[\033[1;34m\]\w\[\033[0m\]\$ '
if command -v bash >/dev/null 2>&1; then export PS1="$p"; exec bash -i; fi
case "$(readlink -f /bin/sh 2>/dev/null)" in
*busybox*) export PS1='\u@\h:\w\$ ' ;;
*) if [ "$(id -u 2>/dev/null)" = 0 ]; then s='#'; else s='$'; fi
  u="$(id -un 2>/dev/null || echo "$USER")"; h="$(hostname 2>/dev/null || cat /etc/hostname 2>/dev/null)"
  export PS1="$u@${h%%.*}:\${PWD}$s " ;;
esac
exec sh -i`

var interactiveShell = []string{"sh", "-c", shellBootstrap}

func (m *TerminalManager) open(ctx context.Context, in *agentv1.TerminalInput) {
	m.close(in.GetSessionId()) // replace any stale session

	// A shell in the project's workload, or a DB console in a managed service's. Credentials
	// come from the container's own env, never argv or the wire.
	workload := in.GetProjectId()
	inner := interactiveShell
	lxc := in.GetTarget() == agentv1.TerminalTarget_TERMINAL_TARGET_LXC
	if ex := in.GetExec(); ex != nil && ex.GetServiceId() != "" && len(ex.GetArgv()) > 0 {
		workload = ex.GetServiceId()
		inner = ex.GetArgv()
		lxc = false
		for _, kv := range ex.GetEnv() {
			// Prefixed via `env`, never as argv secrets.
			inner = append([]string{"env", kv.GetKey() + "=" + kv.GetValue()}, inner...)
		}
	}
	execArgs := m.orch.ExecArgs
	if lxc {
		execArgs = m.orch.LxcExecArgs
	}
	argv, err := execArgs(ctx, workload, inner, true)
	if err != nil {
		m.emitOutput(in.GetSessionId(), []byte("failed to open shell: "+err.Error()+"\r\n"), true)
		return
	}
	sessionCtx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(sessionCtx, argv[0], argv[1:]...)
	ptmx, err := pty.Start(cmd)
	if err != nil {
		cancel()
		m.emitOutput(in.GetSessionId(), []byte("pty error: "+err.Error()+"\r\n"), true)
		return
	}
	resize(ptmx, in)

	s := &ptySession{
		ptmx:   ptmx,
		cancel: cancel,
		in:     make(chan *agentv1.TerminalInput, terminalInputBuffer),
		done:   make(chan struct{}),
	}
	m.mu.Lock()
	m.sessions[in.GetSessionId()] = s
	m.mu.Unlock()

	go m.drain(in.GetSessionId(), s)
	go m.pump(in.GetSessionId(), ptmx)
}

// drain applies a session's input frames one at a time, in order.
func (m *TerminalManager) drain(sessionID string, s *ptySession) {
	for {
		select {
		case <-s.done:
			return
		case in := <-s.in:
			if in.GetClose() {
				m.close(sessionID)
				return
			}
			resize(s.ptmx, in)
			if len(in.GetData()) > 0 {
				_, _ = s.ptmx.Write(in.GetData())
			}
		}
	}
}

// maxPTYDim matches the control plane's cap; the uint32 to uint16 narrowing would wrap,
// so the wire value is not trusted.
const maxPTYDim = 1000

// ptyDimsOK is separate so the bounds are testable without a PTY.
func ptyDimsOK(in *agentv1.TerminalInput) bool {
	rows, cols := in.GetRows(), in.GetCols()
	return rows > 0 && cols > 0 && rows <= maxPTYDim && cols <= maxPTYDim
}

// resize ignores out-of-range dimensions.
func resize(ptmx *os.File, in *agentv1.TerminalInput) {
	if !ptyDimsOK(in) {
		return
	}
	_ = pty.Setsize(ptmx, &pty.Winsize{Rows: uint16(in.GetRows()), Cols: uint16(in.GetCols())})
}

func (m *TerminalManager) pump(sessionID string, ptmx *os.File) {
	buf := make([]byte, 4096)
	for {
		n, err := ptmx.Read(buf)
		if n > 0 {
			// emit copies synchronously, so buf can be reused.
			m.emitOutput(sessionID, buf[:n], false)
		}
		if err != nil {
			break
		}
	}
	m.emitOutput(sessionID, nil, true)
	m.close(sessionID)
}

func (m *TerminalManager) close(sessionID string) {
	m.mu.Lock()
	s := m.sessions[sessionID]
	delete(m.sessions, sessionID)
	m.mu.Unlock()
	if s != nil {
		close(s.done) // stops drain and unblocks Handle
		s.cancel()
		_ = s.ptmx.Close()
	}
}

func (m *TerminalManager) emitOutput(sessionID string, data []byte, closed bool) {
	emit := m.emit.Load().(Emit)
	_ = emit(&agentv1.Event{
		Payload: &agentv1.Event_TerminalOutput{
			TerminalOutput: &agentv1.TerminalOutput{SessionId: sessionID, Data: data, Closed: closed},
		},
	})
}
