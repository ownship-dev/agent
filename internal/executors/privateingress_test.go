package executors

import (
	"context"
	"net"
	"strconv"
	"testing"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
	"github.com/ownship/agent/internal/lanproxy"
)

func lanTestPort(t *testing.T) uint32 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return uint32(ln.Addr().(*net.TCPAddr).Port)
}

func TestPrivateIngressRoutesOnTheLANAndUnroutesOnSwitch(t *testing.T) {
	lan, err := lanproxy.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d := &Deployer{lan: lan}
	noLog := func(agentv1.LogStream, string) {}
	noEmit := func(*agentv1.Event) error { return nil }
	port := lanTestPort(t)

	cmd := &agentv1.Deploy{Lan: &agentv1.LanTarget{Hostnames: []string{"app.lab.lan"}, HttpsPort: port, CaDomain: "lab.lan"}}
	url, ca, err := d.configurePrivateIngress(context.Background(), cmd, "wl1", "dep1", "http://10.0.0.9:3000", "http://10.0.0.9:3000", noLog, noEmit)
	if err != nil {
		t.Fatal(err)
	}
	if ca == "" {
		t.Fatal("the first LAN deploy must report the new CA")
	}
	if want := "https://app.lab.lan:" + strconv.Itoa(int(port)); url != want {
		t.Fatalf("url = %s, want %s", url, want)
	}
	if hosts := lan.Hosts("wl1"); len(hosts) != 1 {
		t.Fatalf("hosts = %v", hosts)
	}

	// Redeployed with ingress switched away: the LAN route must go.
	url, _, err = d.configurePrivateIngress(context.Background(), &agentv1.Deploy{}, "wl1", "dep2", "http://10.0.0.9:3000", "http://10.0.0.9:3000", noLog, noEmit)
	if err != nil || url != "" || len(lan.Hosts("wl1")) != 0 {
		t.Fatalf("url=%q err=%v hosts=%v", url, err, lan.Hosts("wl1"))
	}
}

func TestPrivateIngressRefusesWhenUnavailable(t *testing.T) {
	d := &Deployer{}
	noLog := func(agentv1.LogStream, string) {}
	noEmit := func(*agentv1.Event) error { return nil }
	cmd := &agentv1.Deploy{Tailscale: &agentv1.TailscaleTarget{Hostname: "app"}}
	if _, _, err := d.configurePrivateIngress(context.Background(), cmd, "wl1", "d", "http://x:1", "http://x:1", noLog, noEmit); err == nil {
		t.Fatal("expected an error without a tailscale ingress")
	}
}
