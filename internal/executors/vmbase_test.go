package executors

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

func fakeNextVmid(t *testing.T, ids ...string) {
	t.Helper()
	orig := nextVmid
	i := 0
	nextVmid = func(context.Context) (string, error) {
		id := ids[i%len(ids)]
		i++
		return id, nil
	}
	t.Cleanup(func() { nextVmid = orig })
}

func vmProxmox(t *testing.T) *ProxmoxLXC {
	t.Helper()
	o := newTestProxmox(t)
	o.cfg.Bridge, o.cfg.Storage = "vmbr0", "local-lvm"
	o.baseLocks = newKeyedLocks()
	return o
}

func testImage(sum string) vmImageFile {
	return vmImageFile{Path: "/var/lib/ownship/vm-images/debian-12.qcow2", Format: "qcow2",
		Info: &agentv1.VmImage{Id: "debian-12", Sha256: sum, Usable: true}}
}

func importReply(args []string) ([]byte, error) {
	if args[0] == "importdisk" {
		return []byte("transferred 2.0 GiB\nSuccessfully imported disk as 'unused0:local-lvm:vm-" + args[1] + "-disk-0'\n"), nil
	}
	return nil, nil
}

func verbs(calls [][]string) []string {
	var out []string
	for _, c := range calls {
		out = append(out, c[0])
	}
	return out
}

func TestEnsureVMBaseImportsOnFirstUse(t *testing.T) {
	o := vmProxmox(t)
	fakeNextVmid(t, "9000")
	fakeClusterGuests(t, `[]`, nil)
	calls := fakeQM(t, importReply)

	vmid, err := o.ensureVMBase(context.Background(), testImage("aa"), noLog)
	if err != nil || vmid != "9000" {
		t.Fatalf("%q %v", vmid, err)
	}
	if got := verbs(*calls); !slices.Equal(got, []string{"create", "importdisk", "set", "template"}) {
		t.Fatalf("verbs %v", got)
	}
	create := strings.Join((*calls)[0], " ")
	for _, want := range []string{"--tags ownship-base", "--agent enabled=1", "--net0 virtio,bridge=vmbr0", "--scsihw virtio-scsi-single", "--serial0 socket"} {
		if !strings.Contains(create, want) {
			t.Errorf("create misses %q: %s", want, create)
		}
	}
	if !slices.Equal((*calls)[1], []string{"importdisk", "9000", "/var/lib/ownship/vm-images/debian-12.qcow2", "local-lvm", "--format", "raw"}) {
		t.Fatalf("importdisk %q", (*calls)[1])
	}
	if set := strings.Join((*calls)[2], " "); !strings.Contains(set, "--scsi0 local-lvm:vm-9000-disk-0,discard=on") || !strings.Contains(set, "--ide2 local-lvm:cloudinit") {
		t.Fatalf("set %s", set)
	}
	if b, ok := o.vms.base("debian-12"); !ok || b.Vmid != "9000" || b.Sha256 != "aa" {
		t.Fatalf("base %+v", b)
	}
}

func TestEnsureVMBaseReusesAnExistingTemplate(t *testing.T) {
	o := vmProxmox(t)
	_ = o.vms.setBase("debian-12", vmBase{Vmid: "9000", Sha256: "aa"})
	fakeClusterGuests(t, `[{"vmid":9000,"type":"qemu","tags":"ownship-base"}]`, nil)
	calls := fakeQM(t, importReply)
	vmid, err := o.ensureVMBase(context.Background(), testImage("aa"), noLog)
	if err != nil || vmid != "9000" || len(*calls) != 0 {
		t.Fatalf("%q %v %v", vmid, err, *calls)
	}
}

func TestEnsureVMBaseRebuildsAfterTheFileChanged(t *testing.T) {
	o := vmProxmox(t)
	_ = o.vms.setBase("debian-12", vmBase{Vmid: "9000", Sha256: "aa"})
	fakeNextVmid(t, "9001")
	fakeClusterGuests(t, `[{"vmid":9000,"type":"qemu","tags":"ownship-base"}]`, nil)
	calls := fakeQM(t, importReply)
	vmid, err := o.ensureVMBase(context.Background(), testImage("bb"), noLog)
	if err != nil || vmid != "9001" {
		t.Fatalf("%q %v", vmid, err)
	}
	last := (*calls)[len(*calls)-1]
	if !slices.Equal(last, []string{"destroy", "9000", "--purge"}) {
		t.Fatalf("old template not removed: %v", *calls)
	}
	if b, _ := o.vms.base("debian-12"); b.Vmid != "9001" || b.Sha256 != "bb" {
		t.Fatalf("base %+v", b)
	}
}

// The old template's vmid now names something else: never destroy it.
func TestEnsureVMBaseNeverDestroysARecycledVmid(t *testing.T) {
	o := vmProxmox(t)
	_ = o.vms.setBase("debian-12", vmBase{Vmid: "9000", Sha256: "aa"})
	fakeNextVmid(t, "9001")
	for _, cluster := range []string{
		`[{"vmid":9000,"type":"lxc","tags":"ownship-base"}]`,
		`[{"vmid":9000,"type":"qemu","tags":"ownship"}]`,
		`[{"vmid":9000,"type":"qemu"}]`,
	} {
		fakeClusterGuests(t, cluster, nil)
		calls := fakeQM(t, importReply)
		if _, err := o.ensureVMBase(context.Background(), testImage("bb"), noLog); err != nil {
			t.Fatal(err)
		}
		for _, c := range *calls {
			if c[0] == "destroy" {
				t.Fatalf("destroyed %v with cluster %s", c, cluster)
			}
		}
		_ = o.vms.setBase("debian-12", vmBase{Vmid: "9000", Sha256: "aa"})
	}
}

func TestEnsureVMBaseCleansUpAFailedImport(t *testing.T) {
	o := vmProxmox(t)
	fakeNextVmid(t, "9000")
	fakeClusterGuests(t, `[]`, nil)
	calls := fakeQM(t, func(args []string) ([]byte, error) {
		if args[0] == "importdisk" {
			return []byte("storage full"), errors.New("exit 1")
		}
		return nil, nil
	})
	if _, err := o.ensureVMBase(context.Background(), testImage("aa"), noLog); err == nil || !strings.Contains(err.Error(), "storage full") {
		t.Fatalf("err %v", err)
	}
	if last := (*calls)[len(*calls)-1]; !slices.Equal(last, []string{"destroy", "9000", "--purge"}) {
		t.Fatalf("half-built template left: %v", *calls)
	}
	if _, ok := o.vms.base("debian-12"); ok {
		t.Fatal("a failed import must not be recorded")
	}
}
