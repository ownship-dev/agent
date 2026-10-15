package executors

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// ownshipTag marks this agent's LXCs in the Proxmox UI.
const ownshipTag = "ownship"

// Guest configs are replicated cluster-wide, so a CT on an offline node is still listed.
var clusterGuestsJSON = func(ctx context.Context) ([]byte, error) {
	return exec.CommandContext(ctx, "pvesh", "get", "/cluster/resources",
		"--type", "vm", "--output-format", "json").Output()
}

type clusterGuest struct {
	VMID int    `json:"vmid"`
	Type string `json:"type"`
	Tags string `json:"tags"`
}

// clusterLXCs maps each LXC vmid to whether it carries the ownship tag. VMs are excluded:
// a vmid reused by one isn't the container this agent created.
func clusterLXCs(ctx context.Context) (map[string]bool, error) {
	out, err := clusterGuestsJSON(ctx)
	if err != nil {
		return nil, fmt.Errorf("list cluster guests: %w", err)
	}
	var guests []clusterGuest
	if err := json.Unmarshal(out, &guests); err != nil {
		return nil, fmt.Errorf("parse cluster guests: %w", err)
	}
	lxcs := make(map[string]bool, len(guests))
	for _, g := range guests {
		if g.Type == "lxc" {
			lxcs[strconv.Itoa(g.VMID)] = hasOwnshipTag(g.Tags)
		}
	}
	return lxcs, nil
}

func existingLXCs(ctx context.Context) (map[string]bool, error) {
	lxcs, err := clusterLXCs(ctx)
	if err != nil {
		return nil, err
	}
	for vmid := range lxcs {
		lxcs[vmid] = true
	}
	return lxcs, nil
}

// Proxmox joins tags with ';' (',' and spaces are accepted on input).
func hasOwnshipTag(tags string) bool {
	for _, t := range strings.FieldsFunc(tags, func(r rune) bool { return r == ';' || r == ',' || r == ' ' }) {
		if t == ownshipTag {
			return true
		}
	}
	return false
}

// lxcGone is true only when vmid provably no longer exists; any doubt is "not gone".
func lxcGone(ctx context.Context, vmid string) bool {
	lxcs, err := existingLXCs(ctx)
	return err == nil && !lxcs[vmid]
}
