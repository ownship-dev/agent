package executors

import (
	"testing"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

func TestLxcHostname(t *testing.T) {
	if got := lxcHostname("ownship-postgres-a3f9k", "abc123"); got != "ownship-postgres-a3f9k" {
		t.Fatalf("valid hostname rejected: %q", got)
	}
	// Invalid ones fall back to the default derived from the workload id.
	for _, bad := range []string{"", "Bad_Name", "-leading", "has space", "up.per"} {
		if got := lxcHostname(bad, "abc123"); got != "ownship-"+shortID("abc123") {
			t.Fatalf("hostname %q should fall back, got %q", bad, got)
		}
	}
}

func TestNet0Arg(t *testing.T) {
	// DHCP paths.
	for _, n := range []*agentv1.NetworkConfig{nil, {Mode: "dhcp"}, {Mode: ""}} {
		got, ns, err := net0Arg("vmbr0", n)
		if err != nil || got != "name=eth0,bridge=vmbr0,ip=dhcp" || ns != nil {
			t.Fatalf("dhcp: got %q ns=%v err=%v", got, ns, err)
		}
	}

	// Static, with nameservers.
	got, ns, err := net0Arg("vmbr0", &agentv1.NetworkConfig{
		Mode: "static", IpCidr: "192.168.1.50/24", Gateway: "192.168.1.1",
		Nameservers: []string{"1.1.1.1", "8.8.8.8"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "name=eth0,bridge=vmbr0,ip=192.168.1.50/24,gw=192.168.1.1" {
		t.Fatalf("static net0: %q", got)
	}
	if len(ns) != 2 || ns[0] != "1.1.1.1" {
		t.Fatalf("nameservers: %v", ns)
	}

	// Invalid inputs are rejected before reaching argv.
	bad := []*agentv1.NetworkConfig{
		{Mode: "static", IpCidr: "not-an-ip", Gateway: "192.168.1.1"},
		{Mode: "static", IpCidr: "192.168.1.50/24", Gateway: "999.1.1.1"},
		{Mode: "static", IpCidr: "192.168.1.50/24", Gateway: "192.168.1.1", Nameservers: []string{"bad"}},
	}
	for i, n := range bad {
		if _, _, err := net0Arg("vmbr0", n); err == nil {
			t.Fatalf("case %d should have errored", i)
		}
	}

	// A bridge carrying commas/`=`/spaces would inject extra net0 sub-options —
	// rejected whether it arrives as the host default or the per-deploy override.
	for _, b := range []string{"vmbr0,tag=100", "vmbr0 firewall=0", "vmbr0=x", ""} {
		if _, _, err := net0Arg(b, nil); err == nil {
			t.Fatalf("bad host bridge %q should have errored", b)
		}
	}
	for _, b := range []string{"vmbr0,tag=100", "vmbr0 firewall=0", "vmbr0=x"} {
		if _, _, err := net0Arg("vmbr0", &agentv1.NetworkConfig{Mode: "dhcp", Bridge: b}); err == nil {
			t.Fatalf("bad override bridge %q should have errored", b)
		}
	}
}
