package executors

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func fakeClusterGuests(t *testing.T, json string, err error) {
	t.Helper()
	orig := clusterGuestsJSON
	clusterGuestsJSON = func(context.Context) ([]byte, error) { return []byte(json), err }
	t.Cleanup(func() { clusterGuestsJSON = orig })
}

func TestProxmoxListReportsOnlyLXCsThatExist(t *testing.T) {
	o := proxmoxWithLXC(t, "kept", "101")
	if err := o.state.set("deleted", "102"); err != nil {
		t.Fatal(err)
	}
	fakeClusterGuests(t, `[{"vmid":101,"type":"lxc"},{"vmid":300,"type":"lxc"}]`, nil)

	inv, err := o.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !inv.Complete || !reflect.DeepEqual(inv.IDs, []string{"kept"}) {
		t.Fatalf("got %+v, want complete [kept]", inv)
	}
	// Listing never forgets: only a use site that proves the vmid gone may drop it.
	if _, ok := o.state.get("deleted"); !ok {
		t.Fatal("List pruned the tracking map")
	}
}

// A pvesh failure must never read as "everything was deleted".
func TestProxmoxListIsIncompleteWhenPveshFails(t *testing.T) {
	o := proxmoxWithLXC(t, "proj", "101")
	fakeClusterGuests(t, "", errors.New("pvesh: connection refused"))

	inv, err := o.List(context.Background())
	if err == nil {
		t.Fatal("want the pvesh error surfaced")
	}
	if inv.Complete || !reflect.DeepEqual(inv.IDs, []string{"proj"}) {
		t.Fatalf("got %+v, want incomplete [proj]", inv)
	}
}

// A vmid reused by a VM is not the container this agent created.
func TestProxmoxListTreatsVMIDReusedByQemuAsGone(t *testing.T) {
	o := proxmoxWithLXC(t, "proj", "101")
	fakeClusterGuests(t, `[{"vmid":101,"type":"qemu"}]`, nil)

	inv, err := o.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !inv.Complete || len(inv.IDs) != 0 {
		t.Fatalf("got %+v, want complete and empty", inv)
	}
}

func TestProxmoxListIsIncompleteOnUnparseableOutput(t *testing.T) {
	o := proxmoxWithLXC(t, "proj", "101")
	fakeClusterGuests(t, "not json", nil)

	if inv, err := o.List(context.Background()); err == nil || inv.Complete {
		t.Fatalf("got %+v / %v, want an incomplete listing and an error", inv, err)
	}
}

// Deleting a project whose LXC is already gone must succeed without touching pct.
func TestProxmoxDestroyOfDeletedLXCForgetsTheMapping(t *testing.T) {
	o := proxmoxWithLXC(t, "proj", "101")
	fakeClusterGuests(t, `[]`, nil)

	if err := o.Destroy(context.Background(), "proj", false); err != nil {
		t.Fatal(err)
	}
	if _, ok := o.state.get("proj"); ok {
		t.Fatal("mapping to a deleted LXC survived Destroy")
	}
}

func TestLxcGoneOnlyWhenProven(t *testing.T) {
	fakeClusterGuests(t, `[{"vmid":101,"type":"lxc"}]`, nil)
	if lxcGone(context.Background(), "101") {
		t.Fatal("existing LXC reported gone")
	}
	if !lxcGone(context.Background(), "102") {
		t.Fatal("absent LXC not reported gone")
	}
	fakeClusterGuests(t, "", errors.New("boom"))
	if lxcGone(context.Background(), "102") {
		t.Fatal("unknown state must not read as gone")
	}
}
