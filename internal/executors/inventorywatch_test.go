package executors

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestWorkloadRemovedEvent(t *testing.T) {
	cases := []struct {
		name string
		line string
		want bool
	}{
		{"docker destroy", `{"Type":"container","Action":"destroy","Actor":{"Attributes":{"name":"ownship-abc"}}}`, true},
		{"podman remove", `{"Name":"ownship-abc","Status":"remove","Type":"container"}`, true},
		{"docker stop", `{"Type":"container","Action":"stop","Actor":{"Attributes":{"name":"ownship-abc"}}}`, false},
		{"auth sidecar", `{"Action":"destroy","Actor":{"Attributes":{"name":"ownship-abc` + authSidecarSuffix + `"}}}`, false},
		{"compose service", `{"Action":"destroy","Actor":{"Attributes":{"name":"osc-abc-web-1"}}}`, false},
		{"foreign container", `{"Name":"postgres","Status":"remove"}`, false},
		{"garbage", `not json`, false},
	}
	for _, c := range cases {
		if got := workloadRemovedEvent([]byte(c.line)); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestWatchVMListFiresOnlyWhenAGuestDisappears(t *testing.T) {
	dir := t.TempDir()
	oldPath, oldInterval := vmlistPath, vmlistPollInterval
	vmlistPath, vmlistPollInterval = filepath.Join(dir, ".vmlist"), 10*time.Millisecond
	t.Cleanup(func() { vmlistPath, vmlistPollInterval = oldPath, oldInterval })

	write := func(body string) {
		if err := os.WriteFile(vmlistPath, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"version":1,"ids":{"100":{"node":"pve","type":"lxc","version":1},"200":{"node":"pve","type":"qemu","version":1}}}`)

	var fired atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchVMList(ctx, func() { fired.Add(1) })

	settle := func() { time.Sleep(60 * time.Millisecond) }
	settle()
	write(`{"version":2,"ids":{"100":{"node":"pve","type":"lxc","version":2},"101":{"node":"pve","type":"lxc","version":1},"200":{"node":"pve","type":"qemu","version":1}}}`)
	settle()
	if n := fired.Load(); n != 0 {
		t.Fatalf("an added LXC fired %d times", n)
	}
	write(`{"version":3,"ids":{"101":{"node":"pve","type":"lxc","version":1},"200":{"node":"pve","type":"qemu","version":1}}}`)
	settle()
	if n := fired.Load(); n != 1 {
		t.Fatalf("a removed LXC fired %d times, want 1", n)
	}
	write(`{"version":4,"ids":{"101":{"node":"pve","type":"lxc","version":1}}}`)
	settle()
	if n := fired.Load(); n != 2 {
		t.Fatalf("a removed VM fired %d times, want 2", n-1)
	}
}
