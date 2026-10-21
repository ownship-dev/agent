package executors

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"sync"
	"time"
)

// InventoryWatcher calls onRemoved when a workload may have been deleted, so the inventory is
// re-reported at once instead of on the next tick. It returns when ctx is done.
type InventoryWatcher interface {
	WatchInventory(ctx context.Context, onRemoved func())
}

func (d *Deployer) WatchInventory(ctx context.Context, onRemoved func()) {
	if w, ok := d.orch.(InventoryWatcher); ok {
		w.WatchInventory(ctx, onRemoved)
	}
}

// pmxcfs has no change feed, but .vmlist is an in-memory file listing every guest in the cluster,
// so reading it is far cheaper than pvesh.
var (
	vmlistPath         = "/etc/pve/.vmlist"
	vmlistPollInterval = 2 * time.Second
)

func (o *ProxmoxLXC) WatchInventory(ctx context.Context, onRemoved func()) {
	watchVMList(ctx, onRemoved)
}

func watchVMList(ctx context.Context, onRemoved func()) {
	var prev map[string]bool
	ticker := time.NewTicker(vmlistPollInterval)
	defer ticker.Stop()
	for {
		if cur, err := readVMListGuests(); err == nil {
			if guestRemoved(prev, cur) {
				onRemoved()
			}
			prev = cur
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func readVMListGuests() (map[string]bool, error) {
	data, err := os.ReadFile(vmlistPath)
	if err != nil {
		return nil, err
	}
	var list struct {
		IDs map[string]struct {
			Type string `json:"type"`
		} `json:"ids"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}
	// VMs count too: a VM workload deleted outside Ownship is reported at once like an LXC.
	guests := make(map[string]bool, len(list.IDs))
	for id, g := range list.IDs {
		if g.Type == "lxc" || g.Type == "qemu" {
			guests[id] = true
		}
	}
	return guests, nil
}

func guestRemoved(prev, cur map[string]bool) bool {
	for id := range prev {
		if !cur[id] {
			return true
		}
	}
	return false
}

const engineEventsRetry = 10 * time.Second

// WatchInventory follows each engine's event stream for removed ownship containers. Engines
// that come up later are picked up on the next retry.
func (o *LocalDocker) WatchInventory(ctx context.Context, onRemoved func()) {
	var mu sync.Mutex
	watching := map[string]bool{}
	for {
		for _, eng := range o.state.Engines() {
			mu.Lock()
			busy := watching[eng.Name()]
			watching[eng.Name()] = true
			mu.Unlock()
			if busy {
				continue
			}
			go func(eng *Engine) {
				followEngineRemovals(ctx, eng, onRemoved)
				mu.Lock()
				delete(watching, eng.Name())
				mu.Unlock()
			}(eng)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(engineEventsRetry):
		}
	}
}

// followEngineRemovals returns when the stream ends; the caller restarts it.
func followEngineRemovals(ctx context.Context, eng *Engine, onRemoved func()) {
	cmd := eng.Cmd(ctx, "events", "--filter", "type=container", "--format", "{{json .}}")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	if err := cmd.Start(); err != nil {
		return
	}
	scanner := bufio.NewScanner(out)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		if workloadRemovedEvent(scanner.Bytes()) {
			onRemoved()
		}
	}
	_ = cmd.Wait()
}

// workloadRemovedEvent reads both Docker's event JSON (Action, Actor.Attributes.name) and
// Podman's (Status, Name).
func workloadRemovedEvent(line []byte) bool {
	var ev struct {
		Action string `json:"Action"`
		Status string `json:"Status"`
		Name   string `json:"Name"`
		Actor  struct {
			Attributes map[string]string `json:"Attributes"`
		} `json:"Actor"`
	}
	if json.Unmarshal(line, &ev) != nil {
		return false
	}
	action := ev.Action
	if action == "" {
		action = ev.Status
	}
	if action != "destroy" && action != "remove" {
		return false
	}
	name := ev.Name
	if name == "" {
		name = ev.Actor.Attributes["name"]
	}
	return len(workloadIDsFromNames([]string{name})) == 1
}
