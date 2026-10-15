package executors

import (
	"fmt"
	"net"
	"regexp"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

var reHostname = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// Bridge names go into --net0's comma-separated value, so only bare interface names pass.
var reBridge = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,14}$`)

// lxcHostname falls back to ownship-<shortID> unless requested is a valid DNS label.
func lxcHostname(requested, workloadID string) string {
	if reHostname.MatchString(requested) {
		return requested
	}
	return "ownship-" + shortID(workloadID)
}

// net0Arg validates static ip, gateway and dns as real IPs.
func net0Arg(bridge string, net_ *agentv1.NetworkConfig) (net0 string, nameservers []string, err error) {
	if net_ != nil && net_.GetBridge() != "" {
		bridge = net_.GetBridge()
	}
	if !reBridge.MatchString(bridge) {
		return "", nil, fmt.Errorf("invalid bridge name %q", bridge)
	}
	if net_ == nil || net_.GetMode() != "static" {
		return fmt.Sprintf("name=eth0,bridge=%s,ip=dhcp", bridge), nil, nil
	}
	if _, _, e := net.ParseCIDR(net_.GetIpCidr()); e != nil {
		return "", nil, fmt.Errorf("invalid static IP/CIDR %q", net_.GetIpCidr())
	}
	if ip := net.ParseIP(net_.GetGateway()); ip == nil || ip.To4() == nil {
		return "", nil, fmt.Errorf("invalid gateway %q", net_.GetGateway())
	}
	for _, ns := range net_.GetNameservers() {
		if ip := net.ParseIP(ns); ip == nil || ip.To4() == nil {
			return "", nil, fmt.Errorf("invalid nameserver %q", ns)
		}
		nameservers = append(nameservers, ns)
	}
	return fmt.Sprintf("name=eth0,bridge=%s,ip=%s,gw=%s", bridge, net_.GetIpCidr(), net_.GetGateway()),
		nameservers, nil
}
