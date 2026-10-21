package executors

import (
	"testing"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

var hostileWords = []string{"", "-x", "--help", "a,b", "a b", "a=b", "a\nb", "a\x00b", "../x", "a;b", "$(id)", "`id`", "a'b", "A"}

func TestValidVMImageID(t *testing.T) {
	for _, ok := range []string{"debian-12", "ubuntu-24.04", "a", "alma_9", "x0123456789012345678901234567890123456789012345"} {
		if !validVMImageID(ok) {
			t.Errorf("rejected %q", ok)
		}
	}
	for _, bad := range append(hostileWords, ".hidden", "x01234567890123456789012345678901234567890123456789") {
		if validVMImageID(bad) {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestValidIPv4(t *testing.T) {
	if !validIPv4("192.168.1.20") {
		t.Fatal("rejected a plain IPv4")
	}
	for _, bad := range append(hostileWords, "::1", "fe80::1", "192.168.1.20/24", " 192.168.1.20", "192.168.001.020", "1.2.3.4,gw=5.6.7.8") {
		if validIPv4(bad) {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestVMNetArgs(t *testing.T) {
	net0, ip, ns, err := vmNetArgs("vmbr0", nil)
	if err != nil || net0 != "virtio,bridge=vmbr0" || ip != "ip=dhcp" || ns != nil {
		t.Fatalf("dhcp: %q %q %v %v", net0, ip, ns, err)
	}
	net0, ip, ns, err = vmNetArgs("vmbr0", &agentv1.NetworkConfig{Mode: "static", Bridge: "vmbr1",
		IpCidr: "10.0.0.5/24", Gateway: "10.0.0.1", Nameservers: []string{"1.1.1.1"}})
	if err != nil || net0 != "virtio,bridge=vmbr1" || ip != "ip=10.0.0.5/24,gw=10.0.0.1" || len(ns) != 1 {
		t.Fatalf("static: %q %q %v %v", net0, ip, ns, err)
	}
	for _, bad := range []*agentv1.NetworkConfig{
		{Bridge: "vmbr0,firewall=0"},
		{Bridge: "-vmbr0"},
		{Bridge: "vm br0"},
		{Mode: "static", IpCidr: "10.0.0.5/24,gw=1.1.1.1", Gateway: "10.0.0.1"},
		{Mode: "static", IpCidr: "fd00::5/64", Gateway: "10.0.0.1"},
		{Mode: "static", IpCidr: "10.0.0.5/24", Gateway: "10.0.0.1,x=y"},
		{Mode: "static", IpCidr: "10.0.0.5/24", Gateway: "10.0.0.1", Nameservers: []string{"1.1.1.1 8.8.8.8"}},
		{Mode: "static", IpCidr: "10.0.0.5/24", Gateway: "10.0.0.1", Nameservers: []string{"\n"}},
	} {
		if _, _, _, err := vmNetArgs("vmbr0", bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	for _, w := range hostileWords {
		if _, _, _, err := vmNetArgs(w, nil); err == nil && w != "A" {
			t.Errorf("accepted bridge %q", w)
		}
	}
}

func TestPctArgValidatorsRefuseHostileWords(t *testing.T) {
	for _, w := range hostileWords {
		if w == "" || w == "A" {
			continue
		}
		if validPctArg(w, reVmid) {
			t.Errorf("vmid accepted %q", w)
		}
	}
	for _, w := range []string{"-x", "--help", "a,b", "a b", "a\nb", "a\x00b", "a;b", "$(id)"} {
		if validPctArg(w, reNode) {
			t.Errorf("node accepted %q", w)
		}
	}
}
