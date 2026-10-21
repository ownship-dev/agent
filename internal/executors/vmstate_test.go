package executors

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVMStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "vm-state.json")
	s, err := loadVMState(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.set("w", vmWorkload{Vmid: "200"}); err != nil {
		t.Fatal(err)
	}
	if err := s.setHostKey("200", "SHA256:abc"); err != nil {
		t.Fatal(err)
	}
	if err := s.setBase("debian-12", vmBase{Vmid: "9000", Sha256: "aa"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want 0600", info.Mode().Perm())
	}
	again, err := loadVMState(path)
	if err != nil {
		t.Fatal(err)
	}
	if w, _ := again.get("w"); w.Vmid != "200" || w.HostKeyFpr != "SHA256:abc" {
		t.Fatalf("workload %+v", w)
	}
	if b, _ := again.base("debian-12"); b.Vmid != "9000" || b.Sha256 != "aa" {
		t.Fatalf("base %+v", b)
	}
	if !again.hasWorkloads() {
		t.Fatal("hasWorkloads")
	}
	_ = again.remove("w")
	if again.hasWorkloads() {
		t.Fatal("a base template is not a workload")
	}
}

func TestVMStateMissingFileIsEmpty(t *testing.T) {
	s, err := loadVMState(filepath.Join(t.TempDir(), "none.json"))
	if err != nil {
		t.Fatal(err)
	}
	if s.hasWorkloads() || len(s.bases()) != 0 {
		t.Fatal("want empty")
	}
}

func TestVMStateCorruptFileFailsLoudly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vm.json")
	_ = os.WriteFile(path, []byte("{"), 0o600)
	if _, err := loadVMState(path); err == nil {
		t.Fatal("a corrupt state file must not read as empty")
	}
}

// A workload in both files is ambiguous; the agent must refuse rather than pick one.
func TestGuestForFailsClosedWhenBothStateFilesHoldTheWorkload(t *testing.T) {
	o := newTestProxmox(t)
	_ = o.state.set("w", "105")
	_ = o.vms.set("w", vmWorkload{Vmid: "200"})
	if _, ok := o.guestFor("w"); ok {
		t.Fatal("guestFor must refuse a workload tracked as both kinds")
	}
	if err := o.guestConflict("w"); err == nil {
		t.Fatal("want a conflict error")
	}
	_ = o.state.set("lxc-only", "106")
	if ref, ok := o.guestFor("lxc-only"); !ok || ref.g.Kind() != GuestLXC {
		t.Fatal("lxc-only")
	}
	_ = o.vms.set("vm-only", vmWorkload{Vmid: "201"})
	if ref, ok := o.guestFor("vm-only"); !ok || ref.g.Kind() != GuestVM || ref.vmid != "201" {
		t.Fatalf("vm-only: %+v %v", ref, ok)
	}
}
