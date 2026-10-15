package executors

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ownship/agent/internal/linkbridge"
)

// newLinkedLocal seeds workloads on engines and answers inspect calls like the CLIs would.
func newLinkedLocal(t *testing.T, workloads map[string]EngineKind) *LocalDocker {
	t.Helper()
	o := newTestLocal(t, NewDockerEngine(), NewPodmanEngine())
	for id, k := range workloads {
		_ = o.state.Set(id, k)
	}
	fakeEngineCmds(t, func(args []string) ([]byte, error) {
		switch {
		case len(args) > 2 && args[1] == "container" && args[2] == "inspect":
			name := args[len(args)-1]
			if _, ok := workloads[strings.TrimPrefix(name, "ownship-")]; ok {
				return []byte("{}"), nil
			}
			return nil, errors.New("no such container")
		case len(args) > 2 && args[1] == "network" && args[2] == "inspect":
			return []byte("127.0.0.0/8 127.0.0.1"), nil
		}
		return nil, nil
	})
	o.bridge = linkbridge.New(filepath.Join(t.TempDir(), "lb.json"), o.bridgeDial, o.bridgeAllow)
	t.Cleanup(o.bridge.Close)
	return o
}

func TestLinkAddrSameEngineUsesDNS(t *testing.T) {
	o := newLinkedLocal(t, map[string]EngineKind{"db": KindDocker})
	addr, err := o.LinkAddr(context.Background(), "app", "db", KindDocker, 5432)
	if err != nil || addr != "http://ownship-db:5432" {
		t.Fatalf("%q %v", addr, err)
	}
}

func TestLinkAddrCrossEngineRootfulUsesTheGatewayBridge(t *testing.T) {
	o := newLinkedLocal(t, map[string]EngineKind{"db": KindDocker})
	addr, err := o.LinkAddr(context.Background(), "app", "db", KindPodman, 5432)
	if err != nil || !strings.HasPrefix(addr, "http://127.0.0.1:") || strings.HasSuffix(addr, ":5432") {
		t.Fatalf("%q %v", addr, err)
	}
}

// Rootless containers reach the host's primary address as host.containers.internal (pasta's
// default); nothing maps the host loopback into them.
func TestLinkAddrCrossEngineRootlessUsesTheHostAlias(t *testing.T) {
	o := newLinkedLocal(t, map[string]EngineKind{"db": KindDocker})
	o.state.engines[KindRootless] = rootlessForTest()
	restore := hostPrimaryIP
	t.Cleanup(func() { hostPrimaryIP = restore })
	hostPrimaryIP = func() (string, error) { return "127.0.0.1", nil }
	addr, err := o.LinkAddr(context.Background(), "app", "db", KindRootless, 5432)
	if err != nil || !strings.HasPrefix(addr, "http://"+rootlessLinkHost+":") {
		t.Fatalf("%q %v", addr, err)
	}
}

func TestLinkAddrUnknownTargetFailsLoudly(t *testing.T) {
	o := newLinkedLocal(t, nil)
	if _, err := o.LinkAddr(context.Background(), "app", "ghost", KindDocker, 1); err == nil {
		t.Fatal("expected error")
	}
}

func TestBridgeAllowsOnlyTheSourceNetwork(t *testing.T) {
	o := newLinkedLocal(t, nil)
	o.subnets = map[string]*net.IPNet{"10.89.0.1": mustCIDR(t, "10.89.0.0/24")}
	gw := linkbridge.Key{Source: "10.89.0.1"}
	lo := linkbridge.Key{Source: "127.0.0.1"}
	cases := []struct {
		k    linkbridge.Key
		peer string
		want bool
	}{
		// Being on the network is not enough (no declaring workload here); see the From test below.
		{gw, "10.89.0.7", false},
		{gw, "192.168.1.20", false},
		{gw, "127.0.0.1", false},
		// No listener sits on loopback any more; an old persisted one must admit nobody.
		{lo, "127.0.0.1", false},
		{lo, "10.89.0.7", false},
		{linkbridge.Key{Source: "10.0.0.1"}, "10.0.0.5", false},
	}
	for _, c := range cases {
		if got := o.bridgeAllow(c.k, &net.TCPAddr{IP: net.ParseIP(c.peer)}); got != c.want {
			t.Errorf("source %s peer %s: got %v", c.k.Source, c.peer, got)
		}
	}
}

func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// The rootless listener sits on the host's primary address, and pasta splices rootless traffic
// from exactly that address: nothing else (another host address, a LAN peer) is admitted.
func TestBridgeAllowsOnlyThePrimaryAddressOnTheRootlessListener(t *testing.T) {
	o := newLinkedLocal(t, nil)
	restore := hostPrimaryIP
	t.Cleanup(func() { hostPrimaryIP = restore })
	hostPrimaryIP = func() (string, error) { return "192.168.1.10", nil }
	k := linkbridge.Key{Source: "192.168.1.10"}
	for peer, want := range map[string]bool{"192.168.1.10": true, "192.168.1.50": false, "10.89.0.1": false} {
		if got := o.bridgeAllow(k, &net.TCPAddr{IP: net.ParseIP(peer)}); got != want {
			t.Errorf("peer %s: got %v", peer, got)
		}
	}
	// A gateway whose network can't be inspected (engine down) admits nobody, not "any host address".
	if o.bridgeAllow(linkbridge.Key{Source: "10.89.0.1"}, &net.TCPAddr{IP: net.ParseIP("10.89.0.1")}) {
		t.Fatal("an unknown gateway must fail closed")
	}
}

// A shared network is not a grant: only the workload that declared the link may use it, so the
// peer must be that workload's own container address, re-read per connection.
func TestBridgeAdmitsOnlyTheLinkingWorkloadOnARootfulNetwork(t *testing.T) {
	o := newTestLocal(t, NewDockerEngine(), NewPodmanEngine())
	_ = o.state.Set("app", KindPodman)
	fakeEngineCmds(t, func(args []string) ([]byte, error) {
		if len(args) > 2 && args[1] == "inspect" && args[len(args)-1] == "ownship-app" {
			return []byte("10.89.0.7 "), nil
		}
		return nil, errors.New("unexpected")
	})
	o.subnets = map[string]*net.IPNet{"10.89.0.1": mustCIDR(t, "10.89.0.0/24")}
	k := linkbridge.Key{Source: "10.89.0.1", From: "app", Target: "db", Port: 5432}
	if !o.bridgeAllow(k, &net.TCPAddr{IP: net.ParseIP("10.89.0.7")}) {
		t.Fatal("the linking workload must get through")
	}
	if o.bridgeAllow(k, &net.TCPAddr{IP: net.ParseIP("10.89.0.8")}) {
		t.Fatal("another container on the same network must not")
	}
	if o.bridgeAllow(linkbridge.Key{Source: "10.89.0.1", Target: "db", Port: 5432}, &net.TCPAddr{IP: net.ParseIP("10.89.0.7")}) {
		t.Fatal("a link without a source workload admits nobody")
	}
}

// Rootless needs no exec, the source address is cached briefly, and an unknown gateway
// isn't re-learned on every connection.
func TestBridgeAllowIsCheapPerConnection(t *testing.T) {
	o := newTestLocal(t, NewDockerEngine(), NewPodmanEngine())
	_ = o.state.Set("app", KindPodman)
	restore := hostPrimaryIP
	t.Cleanup(func() { hostPrimaryIP = restore })
	hostPrimaryIP = func() (string, error) { return "192.168.1.10", nil }
	calls := fakeEngineCmds(t, func(args []string) ([]byte, error) {
		if len(args) > 2 && args[1] == "inspect" {
			return []byte("10.89.0.7 "), nil
		}
		return nil, errors.New("nope")
	})
	peer := func(ip string) net.Addr { return &net.TCPAddr{IP: net.ParseIP(ip)} }

	rootless := linkbridge.Key{Source: "192.168.1.10", From: "app"}
	for i := 0; i < 3; i++ {
		o.bridgeAllow(rootless, peer("192.168.1.10"))
	}
	if len(*calls) != 0 {
		t.Fatalf("the rootless listener needs no exec, ran %v", *calls)
	}

	o.subnets = map[string]*net.IPNet{"10.89.0.1": mustCIDR(t, "10.89.0.0/24")}
	rootful := linkbridge.Key{Source: "10.89.0.1", From: "app"}
	for i := 0; i < 3; i++ {
		if !o.bridgeAllow(rootful, peer("10.89.0.7")) {
			t.Fatal("the linking workload must pass")
		}
	}
	if len(*calls) != 1 {
		t.Fatalf("one inspect for three connections, got %d", len(*calls))
	}

	*calls = nil
	unknown := linkbridge.Key{Source: "10.77.0.1", From: "app"}
	o.bridgeAllow(unknown, peer("10.77.0.5"))
	first := len(*calls)
	o.bridgeAllow(unknown, peer("10.77.0.5"))
	if len(*calls) != first {
		t.Fatalf("an unknown gateway is re-learned on every connection: %d then %d", first, len(*calls))
	}
}
