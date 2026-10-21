package executors

import (
	"fmt"
	"net"
	"regexp"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

// An image id is a file name in VM_IMAGE_DIR and part of a template name, so a bare word.
var reVMImageID = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,47}$`)

func validVMImageID(id string) bool { return reVMImageID.MatchString(id) && id != "." && id != ".." }

// validIPv4 is a dotted IPv4 address and nothing else: it reaches ssh and qm argv.
func validIPv4(s string) bool {
	ip := net.ParseIP(s)
	return ip != nil && ip.To4() != nil && ip.String() == s
}

// vmNetArgs builds a VM's net0, ipconfig0 and nameservers from a validated request. Every
// value is re-checked here, since ',' or '=' would add options to Proxmox's key=value lists.
func vmNetArgs(bridge string, n *agentv1.NetworkConfig) (net0, ipconfig0 string, nameservers []string, err error) {
	if n != nil && n.GetBridge() != "" {
		bridge = n.GetBridge()
	}
	if !reBridge.MatchString(bridge) {
		return "", "", nil, fmt.Errorf("invalid bridge name %q", bridge)
	}
	net0 = "virtio,bridge=" + bridge
	if n == nil || n.GetMode() != "static" {
		return net0, "ip=dhcp", nil, nil
	}
	ip, ipNet, e := net.ParseCIDR(n.GetIpCidr())
	if e != nil || ip.To4() == nil || ipNet == nil {
		return "", "", nil, fmt.Errorf("invalid static IP/CIDR %q", n.GetIpCidr())
	}
	ones, _ := ipNet.Mask.Size()
	cidr := fmt.Sprintf("%s/%d", ip.String(), ones)
	if !validIPv4(n.GetGateway()) {
		return "", "", nil, fmt.Errorf("invalid gateway %q", n.GetGateway())
	}
	for _, ns := range n.GetNameservers() {
		if !validIPv4(ns) {
			return "", "", nil, fmt.Errorf("invalid nameserver %q", ns)
		}
		nameservers = append(nameservers, ns)
	}
	return net0, "ip=" + cidr + ",gw=" + n.GetGateway(), nameservers, nil
}
