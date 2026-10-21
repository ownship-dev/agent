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

// A lost state file (reinstall) must not read as "every workload was deleted".
func TestProxmoxListIsIncompleteWithAnUntrackedOwnshipLXC(t *testing.T) {
	o := proxmoxWithLXC(t, "kept", "101")
	fakeClusterGuests(t, `[{"vmid":101,"type":"lxc","tags":"ownship"},{"vmid":102,"type":"lxc","tags":"db;ownship"},{"vmid":300,"type":"lxc","tags":"mine"}]`, nil)

	inv, err := o.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if inv.Complete || !reflect.DeepEqual(inv.IDs, []string{"kept"}) {
		t.Fatalf("got %+v, want incomplete [kept]", inv)
	}
}

func TestProxmoxListIgnoresUntaggedForeignLXCs(t *testing.T) {
	o := proxmoxWithLXC(t, "kept", "101")
	fakeClusterGuests(t, `[{"vmid":101,"type":"lxc","tags":"ownship"},{"vmid":300,"type":"lxc"},{"vmid":301,"type":"qemu","tags":"ownship-base"},{"vmid":302,"type":"qemu"}]`, nil)

	inv, err := o.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !inv.Complete {
		t.Fatalf("got %+v, want complete", inv)
	}
}

func TestProxmoxAdoptTakesOnlyFreeOwnshipLXCs(t *testing.T) {
	o := proxmoxWithLXC(t, "tracked", "101")
	fakeClusterGuests(t, `[
		{"vmid":101,"type":"lxc","tags":"ownship"},
		{"vmid":102,"type":"lxc","tags":"ownship"},
		{"vmid":103,"type":"lxc","tags":"other"},
		{"vmid":105,"type":"qemu","tags":"ownship"},
		{"vmid":106,"type":"lxc","tags":"ownship"},
		{"vmid":107,"type":"lxc","tags":"ownship"}]`, nil)

	adopted, err := o.Adopt(context.Background(), map[string]string{
		"tracked":  "107", // already mapped: never remapped
		"db":       "102",
		"untagged": "103", // not an Ownship container
		"gone":     "104",
		"vm":       "105",
		"held":     "101", // another workload's LXC
		"stale":    "106", // two rows naming one vmid: neither wins
		"live":     "106",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(adopted, []string{"db"}) {
		t.Fatalf("adopted %v, want [db]", adopted)
	}
	if v, _ := o.state.get("tracked"); v != "101" {
		t.Fatalf("tracked remapped to %s", v)
	}
	if v, _ := o.state.get("db"); v != "102" {
		t.Fatalf("db mapped to %q", v)
	}
	// 106 and 107 stay untracked, yet once adopted the listing is trusted again.
	inv, _ := o.List(context.Background())
	if !inv.Complete || !reflect.DeepEqual(inv.IDs, []string{"db", "tracked"}) {
		t.Fatalf("after adopt got %+v, want complete [db tracked]", inv)
	}
}

func TestProxmoxListTrustsAStrayOwnshipLXCAfterAdopt(t *testing.T) {
	o := proxmoxWithLXC(t, "kept", "101")
	fakeClusterGuests(t, `[{"vmid":101,"type":"lxc","tags":"ownship"},{"vmid":102,"type":"lxc","tags":"ownship"}]`, nil)
	if _, err := o.Adopt(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	inv, err := o.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !inv.Complete || !reflect.DeepEqual(inv.IDs, []string{"kept"}) {
		t.Fatalf("got %+v, want complete [kept]", inv)
	}
}

func TestProxmoxAdoptFailsClosedWhenPveshFails(t *testing.T) {
	o := proxmoxWithLXC(t, "tracked", "101")
	fakeClusterGuests(t, "", errors.New("boom"))
	if adopted, err := o.Adopt(context.Background(), map[string]string{"db": "102"}); err == nil || len(adopted) > 0 {
		t.Fatalf("got %v / %v, want an error and nothing adopted", adopted, err)
	}
}

// A VM workload is tagged like an LXC one, so a lost vmstate.json is caught the same way.
func TestProxmoxListIsIncompleteWithAnUntrackedOwnshipVM(t *testing.T) {
	o := proxmoxWithLXC(t, "kept", "101")
	fakeClusterGuests(t, `[{"vmid":101,"type":"lxc","tags":"ownship"},{"vmid":400,"type":"qemu","tags":"ownship"}]`, nil)
	inv, err := o.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if inv.Complete {
		t.Fatalf("got %+v, want incomplete", inv)
	}
}

// Base templates carry ownship-base, never ownship: they are not lost workloads.
func TestBaseTemplatesNeverMakeTheListingIncomplete(t *testing.T) {
	o := proxmoxWithLXC(t, "kept", "101")
	fakeClusterGuests(t, `[{"vmid":101,"type":"lxc","tags":"ownship"},{"vmid":9000,"type":"qemu","tags":"ownship-base"}]`, nil)
	if inv, _ := o.List(context.Background()); !inv.Complete {
		t.Fatalf("got %+v, want complete", inv)
	}
}

func TestGuestGoneByType(t *testing.T) {
	ctx := context.Background()
	fakeClusterGuests(t, `[{"vmid":101,"type":"lxc"},{"vmid":102,"type":"qemu"}]`, nil)
	cases := []struct {
		kind GuestKind
		vmid string
		gone bool
	}{
		{GuestLXC, "101", false},
		{GuestVM, "102", false},
		{GuestVM, "101", true},  // recycled by an LXC: not our VM
		{GuestLXC, "102", true}, // recycled by a VM: not our LXC
		{GuestVM, "103", true},
	}
	for _, c := range cases {
		if got := guestGone(ctx, c.kind, c.vmid); got != c.gone {
			t.Errorf("guestGone(%v, %s) = %v, want %v", c.kind, c.vmid, got, c.gone)
		}
	}
	fakeClusterGuests(t, "", errors.New("boom"))
	if guestGone(ctx, GuestVM, "103") {
		t.Fatal("unknown state must not read as gone")
	}
}
