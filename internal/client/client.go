// Package client owns the outbound stream: connect, heartbeat, reconnect.
package client

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/net/websocket"
	"google.golang.org/protobuf/proto"

	"github.com/ownship/agent/internal/config"
	"github.com/ownship/agent/internal/executors"
	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
	"github.com/ownship/agent/internal/hardware"
	"github.com/ownship/agent/internal/router"
	"github.com/ownship/agent/internal/uninstall"
)

const (
	heartbeatInterval = 20 * time.Second
	metricsInterval   = 30 * time.Second
	// A workload is marked missing after two absent reports; a watched removal skips the wait.
	stateInterval = time.Minute
	// Lets a burst of removals settle into one report.
	stateDebounce = 2 * time.Second
	// The control plane needs a second absent report before it marks a workload missing.
	stateConfirmDelay = 10 * time.Second
	hardwareInterval  = 5 * time.Minute
	// RAPL power needs two reads, so it appears from the second report.
	hardwareFirstDelay = 30 * time.Second
	minBackoff         = 1 * time.Second
	maxBackoff         = 30 * time.Second
	healthyThreshold   = 30 * time.Second
	// Commands are small; the cap stops a bad gateway exhausting memory.
	maxFrameBytes = 16 << 20
	// Outbound frames buffered before producers block.
	outboundQueueDepth = 512
)

type Client struct {
	cfg    config.Config
	token  string
	exec   executors.Executor
	rtl    *executors.RuntimeLogs
	term   *executors.TerminalManager
	waker  *executors.Waker
	relay  *executors.Relay
	router *router.Router
	log    *slog.Logger
	// Kept across reconnects: power is energy between two reads.
	hw *hardware.Collector
}

func New(cfg config.Config, token string, exec executors.Executor, rtl *executors.RuntimeLogs, term *executors.TerminalManager, waker *executors.Waker, relay *executors.Relay, log *slog.Logger) *Client {
	return &Client{cfg: cfg, token: token, exec: exec, rtl: rtl, term: term, waker: waker, relay: relay, router: newRouter(cfg, exec, log), log: log, hw: hardware.New()}
}

// Run keeps a stream open until ctx is cancelled, reconnecting with exponential backoff.
func (c *Client) Run(ctx context.Context) error {
	backoff := minBackoff
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		start := time.Now()
		if err := c.session(ctx); err != nil {
			c.log.Warn("stream ended, reconnecting", "err", err, "backoff", backoff)
		}
		// A session that stayed up was healthy; reset the backoff.
		if time.Since(start) > healthyThreshold {
			backoff = minBackoff
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// session runs one WebSocket: Events out, Commands in, as protobuf.
func (c *Client) session(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	conn, err := dialWebSocket(c.cfg.ControlPlaneURL, c.token)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.MaxPayloadBytes = maxFrameBytes

	// websocket writes aren't concurrency-safe, so a single writer drains sendCh.
	sendCh := make(chan []byte, outboundQueueDepth)
	emit := func(e *agentv1.Event) error {
		b, err := proto.Marshal(e)
		if err != nil {
			return err
		}
		select {
		case sendCh <- b:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// Drains the whole burst per wakeup.
	go func() {
		writeBurst := func(first []byte) bool {
			if websocket.Message.Send(conn, first) != nil {
				return false
			}
			for {
				select {
				case b := <-sendCh:
					if websocket.Message.Send(conn, b) != nil {
						return false
					}
				default:
					return true
				}
			}
		}
		for {
			select {
			case <-ctx.Done():
				return
			case b := <-sendCh:
				if !writeBurst(b) {
					cancel()
					return
				}
			}
		}
	}()

	// Point long-lived followers (runtime logs, terminals, waker) at this session.
	c.rtl.SetEmit(emit)
	c.term.SetEmit(emit)
	c.waker.SetEmit(emit)
	c.relay.SetEmit(emit)

	// Enqueued first, so Hello is sent first.
	_ = emit(hello(c.cfg, c.exec.Nodes(ctx), c.exec.Templates(ctx), c.exec.Bridges(ctx), c.exec.LanCAPEM(), c.exec.GPUs(ctx), c.exec.PveVersion(ctx), c.exec.Engines(ctx)))
	// Lets the control plane reconcile and GC orphans.
	_ = emit(c.stateEvent(ctx))
	go c.heartbeat(ctx, emit)
	go c.sampleMetrics(ctx, emit)
	go c.reportState(ctx, emit)
	go c.reportHardware(ctx, emit)

	// Close the socket on cancellation so Receive returns.
	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	for {
		var data []byte
		if err := websocket.Message.Receive(conn, &data); err != nil {
			return err
		}
		var cmd agentv1.Command
		if err := proto.Unmarshal(data, &cmd); err != nil {
			c.log.Warn("dropped malformed command frame", "err", err)
			continue
		}
		// Keystrokes bypass the durable queue (they must never replay) and stay in order.
		if ti := cmd.GetTerminalInput(); ti != nil {
			c.term.Handle(ctx, ti)
			continue
		}
		// Relay frames are live-only too, and their order is the byte order.
		if rr := cmd.GetRelayRequest(); rr != nil {
			c.relay.Handle(ctx, rr)
			continue
		}
		// Re-send state and Hello, e.g. so a newly downloaded template shows up.
		if cmd.GetRequestState() != nil {
			go func() {
				c.exec.InvalidateCaches()
				_ = emit(hello(c.cfg, c.exec.Nodes(ctx), c.exec.Templates(ctx), c.exec.Bridges(ctx), c.exec.LanCAPEM(), c.exec.GPUs(ctx), c.exec.PveVersion(ctx), c.exec.Engines(ctx)))
				_ = emit(c.stateEvent(ctx))
			}()
			continue
		}
		// Off the receive loop, so a long deploy doesn't block delivery.
		go c.router.Handle(ctx, &cmd, emit)
	}
}

// dialWebSocket maps https/http to wss/ws; the token goes in the Authorization header.
func dialWebSocket(controlPlaneURL, token string) (*websocket.Conn, error) {
	wsURL := controlPlaneURL
	switch {
	case strings.HasPrefix(wsURL, "https://"):
		wsURL = "wss://" + strings.TrimPrefix(wsURL, "https://")
	case strings.HasPrefix(wsURL, "http://"):
		wsURL = "ws://" + strings.TrimPrefix(wsURL, "http://")
	}
	wsURL = strings.TrimRight(wsURL, "/") + "/agent"

	cfg, err := websocket.NewConfig(wsURL, controlPlaneURL)
	if err != nil {
		return nil, err
	}
	cfg.Header = http.Header{"Authorization": {"Bearer " + token}}
	return websocket.DialConfig(cfg)
}

func (c *Client) reportHardware(ctx context.Context, emit executors.Emit) {
	node, _ := os.Hostname()
	wait := hardwareFirstDelay
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = hardwareInterval
		if err := emit(&agentv1.Event{Payload: &agentv1.Event_Hardware{Hardware: c.hw.Report(ctx, node)}}); err != nil {
			return
		}
	}
}

func (c *Client) sampleMetrics(ctx context.Context, emit executors.Emit) {
	ticker := time.NewTicker(metricsInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			samples := c.exec.Sample(ctx)
			if len(samples) == 0 {
				continue
			}
			if err := emit(&agentv1.Event{
				Payload: &agentv1.Event_Metrics{Metrics: &agentv1.Metrics{Samples: samples}},
			}); err != nil {
				return
			}
		}
	}
}

func (c *Client) heartbeat(ctx context.Context, emit executors.Emit) {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-ticker.C:
			if err := emit(&agentv1.Event{
				Payload: &agentv1.Event_Heartbeat{
					Heartbeat: &agentv1.Heartbeat{
						Ts:            t.UnixMilli(),
						DiskFreeBytes: diskFreeBytes(),
						Nodes:         c.exec.Nodes(ctx),
					},
				},
			}); err != nil {
				return
			}
		}
	}
}

func hello(cfg config.Config, nodes []*agentv1.NodeInfo, templates []string, bridges []string, lanCA string, gpus []*agentv1.Gpu, pveVersion string, engines []*agentv1.EngineInfo) *agentv1.Event {
	dockerVersion := ""
	for _, e := range engines {
		if e.GetKind() == agentv1.ContainerEngine_CONTAINER_ENGINE_DOCKER {
			dockerVersion = e.GetVersion()
		}
	}
	return &agentv1.Event{
		Payload: &agentv1.Event_Hello{
			Hello: &agentv1.Hello{
				Agent:          &agentv1.AgentInfo{Version: config.Version, Os: cfg.OS, Arch: cfg.Arch},
				Nodes:          nodes,
				Templates:      templates,
				Bridges:        bridges,
				LanCaPem:       lanCA,
				Gpus:           gpus,
				ProxmoxVersion: pveVersion,
				Engines:        engines,
				Capabilities:   router.Capabilities,
				// Older control planes still read it.
				DockerVersion: dockerVersion,
			},
		},
	}
}

func (c *Client) stateEvent(ctx context.Context) *agentv1.Event {
	inv, err := c.exec.List(ctx)
	if err != nil {
		c.log.Warn("failed to list state", "err", err)
	}
	return executors.StateEvent(inv, err)
}

// reportState re-sends the inventory so outside deletions are noticed without a reconnect.
func (c *Client) reportState(ctx context.Context, emit executors.Emit) {
	ticker := time.NewTicker(stateInterval)
	defer ticker.Stop()
	removed := make(chan struct{}, 1)
	if w, ok := c.exec.(executors.InventoryWatcher); ok {
		go w.WatchInventory(ctx, func() {
			select {
			case removed <- struct{}{}:
			default:
			}
		})
	}
	var confirm <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-removed:
			select {
			case <-ctx.Done():
				return
			case <-time.After(stateDebounce):
			}
			confirm = time.After(stateConfirmDelay)
		case <-confirm:
			confirm = nil
		}
		if err := emit(c.stateEvent(ctx)); err != nil {
			return
		}
	}
}

func newRouter(cfg config.Config, exec executors.Executor, log *slog.Logger) *router.Router {
	r := router.New(exec, log)
	r.Uninstall = func() error { return uninstall.Run(cfg) }
	return r
}
