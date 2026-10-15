package executors

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/ownship/agent/internal/linkbridge"
)

// rootlessLinkHost reaches the host's primary address; the host loopback is never mapped in.
const rootlessLinkHost = "host.containers.internal"

var hostPrimaryIP = func() (string, error) {
	c, err := net.Dial("udp", "192.0.2.1:9") // TEST-NET-1: picks the route, sends nothing
	if err != nil {
		return "", fmt.Errorf("find the host address rootless containers reach: %w", err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).IP.String(), nil
}

// EnableLinkBridge forwards private links across engines and reopens saved ones.
func (o *LocalDocker) EnableLinkBridge(statePath string) error {
	o.bridge = linkbridge.New(statePath, o.bridgeDial, o.bridgeAllow)
	return o.bridge.Resume()
}

// LinkAddr uses engine DNS on one engine; across engines it goes through the link bridge.
func (o *LocalDocker) LinkAddr(ctx context.Context, sourceWorkloadID, target string, sourceEngine EngineKind, port uint32) (string, error) {
	name := "ownship-" + target
	tgt := o.eng(ctx, target)
	// Fail on a missing target rather than return an address that never resolves.
	if _, err := runEngineCmd(tgt.Cmd(ctx, "container", "inspect", "--", name)); err != nil {
		return "", fmt.Errorf("no container for link target %s", target)
	}
	src := normalizeKind(sourceEngine)
	if src == tgt.Kind {
		return fmt.Sprintf("http://%s:%d", name, port), nil
	}
	if o.bridge == nil {
		return "", fmt.Errorf("the link from %s to %s crosses engines and the link bridge is off", sourceWorkloadID, target)
	}
	srcEng, ok := o.state.Engine(src)
	if !ok {
		return "", fmt.Errorf("%s is not available on this host", o.state.byKind(src).Name())
	}
	var listen, advertise string
	if srcEng.Rootless {
		ip, err := hostPrimaryIP()
		if err != nil {
			return "", fmt.Errorf("link bridge: %w", err)
		}
		listen, advertise = ip, rootlessLinkHost
	} else {
		gw, err := o.gatewayFor(ctx, srcEng)
		if err != nil {
			return "", fmt.Errorf("link bridge: %w", err)
		}
		listen, advertise = gw, gw
	}
	p, err := o.bridge.Ensure(linkbridge.Key{Source: listen, From: sourceWorkloadID, Target: target, Port: port})
	if err != nil {
		return "", fmt.Errorf("link bridge: %w", err)
	}
	return fmt.Sprintf("http://%s:%d", advertise, p), nil
}

// gatewayFor is the host side of the engine's link network.
func (o *LocalDocker) gatewayFor(ctx context.Context, eng *Engine) (string, error) {
	ensureLinkNetwork(ctx, eng)
	format := "{{range .IPAM.Config}}{{.Subnet}} {{.Gateway}}{{end}}"
	if eng.Kind != KindDocker {
		format = "{{range .Subnets}}{{.Subnet}} {{.Gateway}}{{end}}"
	}
	out, err := runEngineCmd(eng.Cmd(ctx, "network", "inspect", "--format", format, linkNetwork))
	if err != nil {
		return "", fmt.Errorf("inspect %s network: %s", eng.Name(), engineErr(err))
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		return "", fmt.Errorf("%s network %s has no gateway", eng.Name(), linkNetwork)
	}
	_, subnet, err := net.ParseCIDR(fields[0])
	gw := net.ParseIP(fields[1])
	if err != nil || gw == nil || !subnet.Contains(gw) {
		return "", fmt.Errorf("%s network %s: unexpected %q", eng.Name(), linkNetwork, strings.TrimSpace(string(out)))
	}
	o.subnetMu.Lock()
	if o.subnets == nil {
		o.subnets = map[string]*net.IPNet{}
	}
	o.subnets[gw.String()] = subnet
	o.subnetMu.Unlock()
	return gw.String(), nil
}

func (o *LocalDocker) bridgeDial(ctx context.Context, target string, port uint32) (string, error) {
	p, err := publishedPort(ctx, o.eng(ctx, target), "ownship-"+target, port)
	if err != nil {
		return "", fmt.Errorf("link bridge: %s:%d is not published: %w", target, port, err)
	}
	return net.JoinHostPort("127.0.0.1", p), nil
}

// bridgeCache keeps CLI calls off the per-connection check.
type bridgeCache struct {
	mu       sync.Mutex
	addrs    map[string]cachedAddrs
	unplaced map[string]time.Time
}

type cachedAddrs struct {
	ips []net.IP
	at  time.Time
}

const (
	workloadAddrTTL = 5 * time.Second
	unplacedTTL     = time.Minute
)

func (c *bridgeCache) forget(workloadID string) {
	c.mu.Lock()
	delete(c.addrs, workloadID)
	c.mu.Unlock()
}

// isWorkloadAddr checks the peer is the workload's current container.
func (o *LocalDocker) isWorkloadAddr(workloadID string, ip net.IP) bool {
	o.bcache.mu.Lock()
	c, ok := o.bcache.addrs[workloadID]
	o.bcache.mu.Unlock()
	if !ok || time.Since(c.at) > workloadAddrTTL {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		out, err := runEngineCmd(o.eng(ctx, workloadID).Cmd(ctx, "inspect", "--format",
			"{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}", "ownship-"+workloadID))
		cancel()
		if err != nil {
			return false
		}
		c = cachedAddrs{at: time.Now()}
		for _, f := range strings.Fields(string(out)) {
			if p := net.ParseIP(f); p != nil {
				c.ips = append(c.ips, p)
			}
		}
		o.bcache.mu.Lock()
		if o.bcache.addrs == nil {
			o.bcache.addrs = map[string]cachedAddrs{}
		}
		o.bcache.addrs[workloadID] = c
		o.bcache.mu.Unlock()
	}
	for _, a := range c.ips {
		if a.Equal(ip) {
			return true
		}
	}
	return false
}

// bridgeAllow admits only the workload that declared the link; rootless traffic arrives
// from the host's own address.
func (o *LocalDocker) bridgeAllow(k linkbridge.Key, peer net.Addr) bool {
	tcp, ok := peer.(*net.TCPAddr)
	if !ok {
		return false
	}
	// pasta connects from exactly the host's primary address.
	if primary, err := hostPrimaryIP(); err == nil && k.Source == primary {
		return tcp.IP.Equal(net.ParseIP(primary))
	}
	subnet := o.subnetFor(k.Source)
	// Anything that can't be placed fails closed.
	return subnet != nil && k.From != "" && subnet.Contains(tcp.IP) && o.isWorkloadAddr(k.From, tcp.IP)
}

// subnetFor re-learns after a restart, at most once a minute for an unknown gateway.
func (o *LocalDocker) subnetFor(gateway string) *net.IPNet {
	o.subnetMu.Lock()
	subnet := o.subnets[gateway]
	o.subnetMu.Unlock()
	if subnet != nil {
		return subnet
	}
	o.bcache.mu.Lock()
	at, recent := o.bcache.unplaced[gateway]
	o.bcache.mu.Unlock()
	if recent && time.Since(at) < unplacedTTL {
		return nil
	}
	for _, e := range o.state.Engines() {
		if gw, err := o.gatewayFor(context.Background(), e); err == nil && gw == gateway {
			o.subnetMu.Lock()
			subnet = o.subnets[gw]
			o.subnetMu.Unlock()
			return subnet
		}
	}
	o.bcache.mu.Lock()
	if o.bcache.unplaced == nil {
		o.bcache.unplaced = map[string]time.Time{}
	}
	o.bcache.unplaced[gateway] = time.Now()
	o.bcache.mu.Unlock()
	return nil
}
