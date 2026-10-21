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

// guestInfo is a cluster guest as the reconcile logic needs it: its type and whether it is
// one of this agent's workloads.
type guestInfo struct {
	Type   string // "lxc" or "qemu"
	Tagged bool
	// A VM template this agent imported from a local image.
	Base bool
}

// clusterGuests maps every vmid to its type and ownship tag. A tracked workload only counts
// as present when its vmid still has the type it was recorded as.
func clusterGuests(ctx context.Context) (map[string]guestInfo, error) {
	out, err := clusterGuestsJSON(ctx)
	if err != nil {
		return nil, fmt.Errorf("list cluster guests: %w", err)
	}
	var guests []clusterGuest
	if err := json.Unmarshal(out, &guests); err != nil {
		return nil, fmt.Errorf("parse cluster guests: %w", err)
	}
	all := make(map[string]guestInfo, len(guests))
	for _, g := range guests {
		if g.Type == "lxc" || g.Type == "qemu" {
			all[strconv.Itoa(g.VMID)] = guestInfo{Type: g.Type, Tagged: hasOwnshipTag(g.Tags), Base: hasTag(g.Tags, ownshipBaseTag)}
		}
	}
	return all, nil
}

// pveType is the name /cluster/resources uses for a guest kind.
func pveType(k GuestKind) string {
	if k == GuestVM {
		return "qemu"
	}
	return "lxc"
}

func (i guestInfo) is(k GuestKind) bool { return i.Type == pveType(k) }

// Imported VM templates carry their own tag, so a template never reads as a lost workload.
const ownshipBaseTag = "ownship-base"

func hasOwnshipTag(tags string) bool { return hasTag(tags, ownshipTag) }

// Proxmox joins tags with ';' (',' and spaces are accepted on input).
func hasTag(tags, want string) bool {
	for _, t := range strings.FieldsFunc(tags, func(r rune) bool { return r == ';' || r == ',' || r == ' ' }) {
		if t == want {
			return true
		}
	}
	return false
}

// guestGone is true only when vmid provably no longer exists as kind; a vmid recycled by the
// other type is gone too. Any doubt is "not gone".
func guestGone(ctx context.Context, kind GuestKind, vmid string) bool {
	all, err := clusterGuests(ctx)
	if err != nil {
		return false
	}
	g, ok := all[vmid]
	return !ok || !g.is(kind)
}

func lxcGone(ctx context.Context, vmid string) bool { return guestGone(ctx, GuestLXC, vmid) }
