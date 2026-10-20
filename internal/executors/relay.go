package executors

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

const (
	// Frames in flight each way before waiting for credit; must match the gateway's RELAY_WINDOW.
	relayWindow     = 32
	relayChunkBytes = 256 << 10
	relayIdle       = 2 * time.Minute
	relayMaxActive  = 16
)

// Relay answers S3 requests relayed through the gateway to a backup buddy on this machine,
// so its MinIO needs no inbound route.
type Relay struct {
	// Where a workload listens, as recorded at deploy time.
	resolve func(workloadID string) (string, bool)
	client  *http.Client
	emit    atomic.Value

	mu       sync.Mutex
	sessions map[string]*relaySession
}

type relaySession struct {
	in     chan *agentv1.RelayRequest
	cancel context.CancelFunc
	// Highest response frame the gateway reported delivered.
	acked  atomic.Uint64
	credit chan struct{}
}

func NewRelay(resolve func(workloadID string) (string, bool)) *Relay {
	return &Relay{
		resolve:  resolve,
		sessions: map[string]*relaySession{},
		client: &http.Client{
			// The caller's client negotiates encoding and follows redirects itself.
			Transport: &http.Transport{
				DisableCompression:    true,
				ResponseHeaderTimeout: 10 * time.Minute,
				MaxIdleConnsPerHost:   relayMaxActive,
				IdleConnTimeout:       90 * time.Second,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// SetEmit points sessions at the current stream on each connect.
func (r *Relay) SetEmit(emit Emit) {
	r.emit.Store(emit)
}

func (r *Relay) send(e *agentv1.RelayResponse) error {
	emit, _ := r.emit.Load().(Emit)
	if emit == nil {
		return errors.New("no stream")
	}
	return emit(&agentv1.Event{Payload: &agentv1.Event_RelayResponse{RelayResponse: e}})
}

// Handle runs in wire order and never blocks; the gateway's credit window bounds each
// session's channel.
func (r *Relay) Handle(ctx context.Context, in *agentv1.RelayRequest) {
	id := in.GetSessionId()
	if in.GetHead() != nil {
		r.open(ctx, in)
		return
	}
	r.mu.Lock()
	s := r.sessions[id]
	r.mu.Unlock()
	if s == nil {
		return
	}
	if in.GetAbort() {
		s.cancel()
		return
	}
	if a := in.GetAcked(); a > s.acked.Load() {
		s.acked.Store(a)
		select {
		case s.credit <- struct{}{}:
		default:
		}
	}
	if in.GetSeq() == 0 {
		return
	}
	select {
	case s.in <- in:
	default:
		// The gateway overran its window, so ordering can't be trusted.
		s.cancel()
	}
}

func (r *Relay) open(ctx context.Context, first *agentv1.RelayRequest) {
	id := first.GetSessionId()
	sctx, cancel := context.WithCancel(ctx)
	s := &relaySession{in: make(chan *agentv1.RelayRequest, relayWindow+8), cancel: cancel, credit: make(chan struct{}, 1)}
	r.mu.Lock()
	_, dup := r.sessions[id]
	full := len(r.sessions) >= relayMaxActive
	if !dup && !full {
		r.sessions[id] = s
	}
	r.mu.Unlock()
	if dup || full {
		cancel()
		if full {
			_ = r.send(&agentv1.RelayResponse{SessionId: id, Error: "relay busy"})
		}
		return
	}
	go func() {
		defer func() {
			cancel()
			r.mu.Lock()
			delete(r.sessions, id)
			r.mu.Unlock()
		}()
		if err := r.run(sctx, id, s, first); err != nil && sctx.Err() == nil {
			_ = r.send(&agentv1.RelayResponse{SessionId: id, Error: err.Error()})
		}
	}()
}

func (r *Relay) run(ctx context.Context, id string, s *relaySession, first *agentv1.RelayRequest) error {
	base, ok := r.resolve(first.GetWorkloadId())
	if !ok {
		return errors.New("the backup buddy is not running on this machine")
	}
	target, err := relayTarget(base, first.GetHead().GetPath())
	if err != nil {
		return err
	}
	body := &relayBody{ctx: ctx, s: s, ack: func(seq uint64) error {
		return r.send(&agentv1.RelayResponse{SessionId: id, Acked: seq})
	}}
	if err := body.ack(first.GetSeq()); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, first.GetHead().GetMethod(), target, body)
	if err != nil {
		return fmt.Errorf("bad request: %w", err)
	}
	req.ContentLength = -1
	hasLength := false
	for _, h := range first.GetHead().GetHeaders() {
		switch name := strings.ToLower(h.GetName()); {
		case name == "host":
			// MinIO checks the SigV4 signature against the signed host.
			req.Host = h.GetValue()
		case name == "content-length":
			if n, err := strconv.ParseInt(h.GetValue(), 10, 64); err == nil && n >= 0 {
				req.ContentLength, hasLength = n, true
			}
		case hopByHop[name]:
		default:
			req.Header.Add(h.GetName(), h.GetValue())
		}
	}
	// No body is sent, but frames are still drained so the gateway's window keeps moving.
	if req.ContentLength == 0 || (!hasLength && bodyless[req.Method]) {
		req.Body, req.ContentLength = http.NoBody, 0
		go func() { _, _ = io.Copy(io.Discard, body) }()
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("buddy storage unreachable: %w", err)
	}
	defer resp.Body.Close()

	head := &agentv1.RelayHead{Status: uint32(resp.StatusCode)}
	for name, values := range resp.Header {
		if hopByHop[strings.ToLower(name)] {
			continue
		}
		for _, v := range values {
			head.Headers = append(head.Headers, &agentv1.RelayHeader{Name: name, Value: v})
		}
	}
	seq := uint64(1)
	if err := r.send(&agentv1.RelayResponse{SessionId: id, Seq: seq, Head: head}); err != nil {
		return err
	}
	buf := make([]byte, relayChunkBytes)
	for {
		n, readErr := io.ReadFull(resp.Body, buf)
		if n > 0 {
			if err := s.waitCredit(ctx, seq); err != nil {
				return err
			}
			seq++
			if err := r.send(&agentv1.RelayResponse{SessionId: id, Seq: seq, Data: append([]byte(nil), buf[:n]...)}); err != nil {
				return err
			}
		}
		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			break
		}
		if readErr != nil {
			return fmt.Errorf("buddy storage read: %w", readErr)
		}
	}
	return r.send(&agentv1.RelayResponse{SessionId: id, Seq: seq + 1, Eof: true})
}

// waitCredit blocks while a full window is undelivered.
func (s *relaySession) waitCredit(ctx context.Context, sent uint64) error {
	for sent-min(sent, s.acked.Load()) >= relayWindow {
		idle := time.NewTimer(relayIdle)
		select {
		case <-s.credit:
			idle.Stop()
		case <-ctx.Done():
			idle.Stop()
			return ctx.Err()
		case <-idle.C:
			return errors.New("relay stalled")
		}
	}
	return nil
}

// relayTarget appends the caller's path to the buddy's address; it can never name another host.
func relayTarget(base, path string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return "", errors.New("the backup buddy has no address")
	}
	if !strings.HasPrefix(path, "/") {
		return "", errors.New("bad request path")
	}
	ref, err := url.ParseRequestURI(path)
	if err != nil || ref.Host != "" || ref.Scheme != "" {
		return "", errors.New("bad request path")
	}
	u.Path, u.RawPath, u.RawQuery, u.Fragment = ref.Path, ref.RawPath, ref.RawQuery, ""
	return u.String(), nil
}

var hopByHop = map[string]bool{
	"connection": true, "keep-alive": true, "proxy-authenticate": true, "proxy-authorization": true,
	"proxy-connection": true, "te": true, "trailer": true, "transfer-encoding": true, "upgrade": true,
}

var bodyless = map[string]bool{http.MethodGet: true, http.MethodHead: true, http.MethodDelete: true, http.MethodOptions: true}

// relayBody acknowledges each frame once the HTTP client takes it, pacing the gateway.
type relayBody struct {
	ctx  context.Context
	s    *relaySession
	ack  func(seq uint64) error
	rest []byte
	done bool
}

func (b *relayBody) Read(p []byte) (int, error) {
	for len(b.rest) == 0 {
		if b.done {
			return 0, io.EOF
		}
		idle := time.NewTimer(relayIdle)
		select {
		case f := <-b.s.in:
			idle.Stop()
			if err := b.ack(f.GetSeq()); err != nil {
				return 0, err
			}
			b.rest, b.done = f.GetData(), f.GetEof()
		case <-b.ctx.Done():
			idle.Stop()
			return 0, b.ctx.Err()
		case <-idle.C:
			return 0, errors.New("relay stalled")
		}
	}
	n := copy(p, b.rest)
	b.rest = b.rest[n:]
	return n, nil
}

func (b *relayBody) Close() error { return nil }

// relayRoundTripper adds the relay token after signing, so the signature is untouched.
type relayRoundTripper struct {
	base  http.RoundTripper
	token string
}

func (t relayRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	out := req.Clone(req.Context())
	out.Header.Set("X-Ownship-Relay", t.token)
	return t.base.RoundTrip(out)
}
