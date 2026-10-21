package executors

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

// vmCapableHost fakes a Proxmox 8.2 x86_64 host with KVM and VM storage local-lvm.
func vmCapableHost(t *testing.T, o *ProxmoxLXC) {
	t.Helper()
	pv, lp, kvm, arch, st := pveVersionOutput, vmLookPath, vmKVMDevice, vmHostArch, vmImageStorages
	pveVersionOutput = func(context.Context) ([]byte, error) { return []byte("pve-manager/8.2.4/abc"), nil }
	vmLookPath = func(name string) (string, error) { return "/usr/bin/" + name, nil }
	vmKVMDevice = filepath.Join(t.TempDir(), "kvm")
	if err := os.WriteFile(vmKVMDevice, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	vmHostArch = "amd64"
	vmImageStorages = func(context.Context, string) ([]byte, error) {
		return []byte(`[{"storage":"local-lvm"},{"storage":"ceph"}]`), nil
	}
	o.nodeName = "pve1"
	t.Cleanup(func() {
		pveVersionOutput, vmLookPath, vmKVMDevice, vmHostArch, vmImageStorages = pv, lp, kvm, arch, st
	})
}

func vmImageInDir(t *testing.T, o *ProxmoxLXC) {
	t.Helper()
	dir := imageDir(t)
	put(t, dir, "debian-12.qcow2", "disk", 0o644)
	fakeQemuImg(t, nil)
	o.cfg.VMImageDir = dir
}

func vmSpec(mut func(*RunSpec)) RunSpec {
	s := RunSpec{WorkloadID: "proj-vm", GuestKind: GuestVM, VmImage: "debian-12", Hostname: "app",
		Resources: &agentv1.Resources{Vcpu: 2, MemoryMb: 2048}, LogLine: noLog}
	if mut != nil {
		mut(&s)
	}
	return s
}

func TestVMCapabilityNamesWhatIsMissing(t *testing.T) {
	cases := []struct {
		name   string
		break_ func(o *ProxmoxLXC)
		want   string
	}{
		{"capable", func(*ProxmoxLXC) {}, ""},
		{"arm", func(*ProxmoxLXC) { vmHostArch = "arm64" }, "x86_64"},
		{"old pve", func(*ProxmoxLXC) {
			pveVersionOutput = func(context.Context) ([]byte, error) { return []byte("pve-manager/7.4.3/x"), nil }
		}, "Proxmox VE 8.0 or newer; this host runs 7.4.3"},
		{"no qemu-img", func(*ProxmoxLXC) {
			vmLookPath = func(name string) (string, error) {
				if name == "qemu-img" {
					return "", errors.New("not found")
				}
				return name, nil
			}
		}, "qemu-img is not installed"},
		{"no kvm", func(*ProxmoxLXC) { vmKVMDevice = "/nonexistent/kvm" }, "/dev/kvm"},
		{"storage", func(o *ProxmoxLXC) { o.cfg.Storage = "local" }, `"local" does not accept VM disks`},
		{"no image", func(o *ProxmoxLXC) {
			_ = os.Remove(filepath.Join(o.cfg.VMImageDir, "debian-12.qcow2"))
		}, "No usable VM image"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := vmProxmox(t)
			vmCapableHost(t, o)
			vmImageInDir(t, o)
			c.break_(o)
			images, reason := o.vmCapability(context.Background())
			if c.want == "" && reason != "" || !strings.Contains(reason, c.want) {
				t.Fatalf("reason = %q, want %q", reason, c.want)
			}
			if c.name == "capable" && (len(images) != 1 || !images[0].GetUsable()) {
				t.Fatalf("images = %v", images)
			}
		})
	}
}

func TestRunRefusesWhatAVMCannotDoBeforeTouchingProxmox(t *testing.T) {
	cases := map[string]func(*RunSpec){
		"golden template": func(s *RunSpec) { s.TemplateVmid = "9000" },
		"base template":   func(s *RunSpec) { s.BaseTemplate = "local:vztmpl/debian.tar.zst" },
		"gpu":             func(s *RunSpec) { s.Resources.Gpu = true },
		"memory":          func(s *RunSpec) { s.Resources.MemoryMb = 512 },
		"other node":      func(s *RunSpec) { s.TargetNode = "pve2" },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			o := vmProxmox(t)
			vmCapableHost(t, o)
			calls := fakeQM(t, func([]string) ([]byte, error) { return nil, nil })
			if _, err := o.Run(context.Background(), vmSpec(mut)); err == nil {
				t.Fatal("expected a refusal")
			}
			if len(*calls) > 0 {
				t.Fatalf("refused spec still ran qm: %v", *calls)
			}
		})
	}
	t.Run("host not capable", func(t *testing.T) {
		o := vmProxmox(t)
		vmCapableHost(t, o)
		vmHostArch = "arm64"
		if _, err := o.Run(context.Background(), vmSpec(nil)); err == nil || !strings.Contains(err.Error(), "x86_64") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestEnsureGuestCreatesAVMFromItsImage(t *testing.T) {
	fastVMWaits(t)
	o, _ := testVMGuest(t)
	vmCapableHost(t, o)
	vmImageInDir(t, o)
	fakeNextVmid(t, "9000", "201")
	fakeClusterGuests(t, `[]`, nil)
	calls := vmQM(t, testHostKey(t), func(args []string) ([]byte, error, bool) {
		if args[0] == "importdisk" {
			out, err := importReply(args)
			return out, err, true
		}
		return nil, nil, false
	})
	fakeSSHProbe(t, func([]string) error { return nil })

	ref, err := o.ensureGuest(context.Background(), vmSpec(nil))
	if err != nil {
		t.Fatal(err)
	}
	if ref.vmid != "201" || ref.g.Kind() != GuestVM {
		t.Fatalf("ref = %+v", ref)
	}
	if w, ok := o.vms.get("proj-vm"); !ok || w.Vmid != "201" {
		t.Fatalf("vm state = %+v %v", w, ok)
	}
	if _, inLXC := o.state.get("proj-vm"); inLXC {
		t.Fatal("a VM must never land in the LXC state")
	}
	var clone, set []string
	for _, c := range *calls {
		switch {
		case c[0] == "clone":
			clone = c
		case c[0] == "set" && len(c) > 1 && c[1] == "201" && slices.Contains(c, "--ciuser"):
			set = c
		}
	}
	if strings.Join(clone, " ") != "clone 9000 201 --name app --full 1 --storage local-lvm" {
		t.Fatalf("clone = %v", clone)
	}
	if !slices.Contains(set, "2048") || !slices.Contains(set, "ownship") {
		t.Fatalf("configure = %v", set)
	}
}

func TestEnsureGuestRefusesToSwitchKinds(t *testing.T) {
	o := proxmoxWithLXC(t, "proj-vm", "142")
	fakeClusterGuests(t, `[{"vmid":142,"type":"lxc","tags":"ownship"}]`, nil)
	_, err := o.ensureGuest(context.Background(), vmSpec(nil))
	if err == nil || !strings.Contains(err.Error(), "conversion") {
		t.Fatalf("err = %v", err)
	}
}

func TestCreateVMRemovesAVMThatNeverBooted(t *testing.T) {
	fastVMWaits(t)
	o, _ := testVMGuest(t)
	vmCapableHost(t, o)
	vmImageInDir(t, o)
	fakeNextVmid(t, "9000", "201")
	fakeClusterGuests(t, `[]`, nil)
	calls := vmQM(t, testHostKey(t), func(args []string) ([]byte, error, bool) {
		switch {
		case args[0] == "importdisk":
			out, err := importReply(args)
			return out, err, true
		case args[0] == "agent":
			return []byte("QEMU guest agent is not running"), errors.New("exit 255"), true
		}
		return nil, nil, false
	})

	if _, err := o.ensureGuest(context.Background(), vmSpec(nil)); err == nil {
		t.Fatal("expected the boot to fail")
	}
	destroyed := false
	for _, c := range *calls {
		if c[0] == "destroy" && c[1] == "201" {
			destroyed = true
		}
	}
	if !destroyed {
		t.Fatalf("the half-built VM was left behind: %v", verbs(*calls))
	}
	if _, ok := o.vms.get("proj-vm"); ok {
		t.Fatal("the failed VM is still tracked")
	}
}

func TestLocalDockerRefusesAVM(t *testing.T) {
	o := &LocalDocker{}
	if _, err := o.Run(context.Background(), RunSpec{WorkloadID: "w", GuestKind: GuestVM}); !errors.Is(err, errVMNeedsProxmox) {
		t.Fatalf("err = %v", err)
	}
}

func TestWakeHoldIsLongerForAVM(t *testing.T) {
	o := proxmoxWithLXC(t, "lxc-app", "142")
	if err := o.vms.set("vm-app", vmWorkload{Vmid: "201"}); err != nil {
		t.Fatal(err)
	}
	w := &Waker{orch: o}
	if w.holdFor("lxc-app") != wakeHoldMax || w.holdFor("vm-app") != wakeHoldVM {
		t.Fatalf("holds = %v %v", w.holdFor("lxc-app"), w.holdFor("vm-app"))
	}
}

func TestUninstallCleanupRemovesTemplatesAndCredentialsButNotImages(t *testing.T) {
	o, g := testVMGuest(t)
	vmImageInDir(t, o)
	if err := o.vms.setBase("debian-12", vmBase{Vmid: "9000", Sha256: "aa"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ensureVMKey(g.paths); err != nil {
		t.Fatal(err)
	}
	fakeClusterGuests(t, `[{"vmid":9000,"type":"qemu","template":1,"tags":"ownship-base"}]`, nil)
	calls := fakeQM(t, func([]string) ([]byte, error) { return nil, nil })

	if err := o.UninstallCleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 || strings.Join((*calls)[0], " ") != "destroy 9000 --purge" {
		t.Fatalf("qm calls = %v", *calls)
	}
	if len(o.vms.bases()) != 0 {
		t.Fatal("the template is still recorded")
	}
	if _, err := os.Stat(g.paths.Key); !os.IsNotExist(err) {
		t.Fatal("the VM SSH key survived the uninstall")
	}
	if _, err := os.Stat(filepath.Join(o.cfg.VMImageDir, "debian-12.qcow2")); err != nil {
		t.Fatal("the owner's image file must stay")
	}
}
