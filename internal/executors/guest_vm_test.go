package executors

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

func fastVMWaits(t *testing.T) {
	t.Helper()
	rt, rk, mb := vmReadyTimeout, vmRekeyAfter, vmPollMaxBackoff
	vmReadyTimeout, vmRekeyAfter, vmPollMaxBackoff = 400*time.Millisecond, 50*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { vmReadyTimeout, vmRekeyAfter, vmPollMaxBackoff = rt, rk, mb })
}

func fakeSSHProbe(t *testing.T, answer func(argv []string) error) *[][]string {
	t.Helper()
	var calls [][]string
	orig := vmSSHProbe
	vmSSHProbe = func(_ context.Context, argv []string) error {
		calls = append(calls, argv)
		return answer(argv)
	}
	t.Cleanup(func() { vmSSHProbe = orig })
	return &calls
}

func testVMGuest(t *testing.T) (*ProxmoxLXC, *vmGuest) {
	t.Helper()
	o := vmProxmox(t)
	o.cfg.RootfsGB = 16
	g := newVMGuest(o)
	g.paths = newSSHPaths(t.TempDir())
	o.vm = g
	return o, g
}

const ifacesReply = `[{"name":"eth0","ip-addresses":[{"ip-address":"192.168.1.50","ip-address-type":"ipv4"}]}]`

// vmQM answers like a healthy Proxmox host with one running, tagged VM 201.
func vmQM(t *testing.T, hostKey string, override func(args []string) ([]byte, error, bool)) *[][]string {
	return fakeQM(t, func(args []string) ([]byte, error) {
		if override != nil {
			if out, err, ok := override(args); ok {
				return out, err
			}
		}
		switch {
		case args[0] == "status":
			return []byte("status: running\n"), nil
		case args[0] == "agent" && args[2] == "ping":
			return nil, nil
		case args[0] == "agent" && args[2] == "network-get-interfaces":
			return []byte(ifacesReply), nil
		case args[0] == "guest":
			return guestExecReply(hostKey, 0), nil
		case args[0] == "config":
			return []byte("cores: 1\nmemory: 1024\nnet0: virtio=BC:24:11:AA:BB:CC,bridge=vmbr0\nscsi0: local-lvm:vm-201-disk-0,discard=on,size=2G\n"), nil
		}
		return nil, nil
	})
}

func TestVMStartBootsWaitsForAgentPinsAndProbesSSH(t *testing.T) {
	fastVMWaits(t)
	_, g := testVMGuest(t)
	key := testHostKey(t)
	calls := vmQM(t, key, func(args []string) ([]byte, error, bool) {
		if args[0] == "status" {
			return []byte("status: stopped\n"), nil, true
		}
		return nil, nil, false
	})
	probes := fakeSSHProbe(t, func([]string) error { return nil })
	if err := g.Start(context.Background(), "201"); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(*calls, func(c []string) bool { return slices.Equal(c, []string{"start", "201"}) }) {
		t.Fatalf("no qm start: %v", *calls)
	}
	if !g.pinned("201") {
		t.Fatal("host key not pinned")
	}
	probe := strings.Join((*probes)[0], " ")
	if !strings.Contains(probe, "HostKeyAlias=ownship-vm-201") || !strings.HasSuffix(probe, "192.168.1.50 sudo -n -- 'true'") {
		t.Fatalf("probe %s", probe)
	}
}

func TestVMReadyNamesTheMissingGuestAgent(t *testing.T) {
	fastVMWaits(t)
	_, g := testVMGuest(t)
	vmQM(t, "", func(args []string) ([]byte, error, bool) {
		if args[0] == "agent" {
			return []byte("QEMU guest agent is not running"), errors.New("exit 255"), true
		}
		return nil, nil, false
	})
	err := g.Ready(context.Background(), "201")
	if err == nil || !strings.Contains(err.Error(), "qemu-guest-agent") {
		t.Fatalf("err %v", err)
	}
}

// After an agent reinstall the guest still has the old key: replace it through the guest agent.
func TestVMReadyRekeysThroughTheGuestAgent(t *testing.T) {
	fastVMWaits(t)
	_, g := testVMGuest(t)
	key := testHostKey(t)
	var installed bool
	calls := vmQM(t, key, func(args []string) ([]byte, error, bool) {
		if args[0] == "guest" && slices.Contains(args, "sh") {
			installed = true
			return guestExecReply("", 0), nil, true
		}
		return nil, nil, false
	})
	fakeSSHProbe(t, func([]string) error {
		if installed {
			return nil
		}
		return errors.New("Permission denied (publickey)")
	})
	if err := g.Ready(context.Background(), "201"); err != nil {
		t.Fatal(err)
	}
	if !installed {
		t.Fatalf("key not re-installed: %v", *calls)
	}
}

func TestVMReadyGivesUpWithTheImageRequirements(t *testing.T) {
	fastVMWaits(t)
	_, g := testVMGuest(t)
	vmQM(t, testHostKey(t), func(args []string) ([]byte, error, bool) {
		if args[0] == "guest" && slices.Contains(args, "sh") {
			return guestExecReply("", 1), nil, true
		}
		return nil, nil, false
	})
	fakeSSHProbe(t, func([]string) error { return errors.New("Permission denied") })
	err := g.Ready(context.Background(), "201")
	if err == nil || !strings.Contains(err.Error(), "cloud-init, openssh-server and sudo") {
		t.Fatalf("err %v", err)
	}
}

func TestVMStopAndDestroyNeverTouchAGuestThatIsNotOurs(t *testing.T) {
	_, g := testVMGuest(t)
	ctx := context.Background()
	for _, cluster := range []string{
		`[{"vmid":201,"type":"lxc","tags":"ownship"}]`,
		`[{"vmid":201,"type":"qemu"}]`,
		`[{"vmid":201,"type":"qemu","tags":"ownship-base"}]`,
		`[]`,
	} {
		fakeClusterGuests(t, cluster, nil)
		calls := vmQM(t, "", nil)
		if err := g.Stop(ctx, "201"); err == nil {
			t.Errorf("stop allowed with %s", cluster)
		}
		if err := g.Shutdown(ctx, "201"); err == nil {
			t.Errorf("shutdown allowed with %s", cluster)
		}
		if err := g.Destroy(ctx, "201"); err != nil {
			t.Errorf("destroy of a foreign vmid must only drop the mapping: %v", err)
		}
		if len(*calls) != 0 {
			t.Errorf("qm called with %s: %v", cluster, *calls)
		}
	}
	fakeClusterGuests(t, "", errors.New("pvesh down"))
	calls := vmQM(t, "", nil)
	_ = g.Destroy(ctx, "201")
	if len(*calls) != 0 {
		t.Fatalf("an unreadable cluster must fail closed: %v", *calls)
	}
}

func TestVMDestroyPurgesAndUnpins(t *testing.T) {
	_, g := testVMGuest(t)
	_ = pinHostKey(g.paths.KnownHosts, hostKeyAlias("201"), strings.TrimSpace(testHostKey(t)))
	fakeClusterGuests(t, `[{"vmid":201,"type":"qemu","tags":"ownship"}]`, nil)
	calls := vmQM(t, "", nil)
	if err := g.Destroy(context.Background(), "201"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal((*calls)[len(*calls)-1], []string{"destroy", "201", "--purge", "--destroy-unreferenced-disks", "1"}) {
		t.Fatalf("calls %v", *calls)
	}
	if g.pinned("201") {
		t.Fatal("pin left behind")
	}
}

func TestVMResizeRestartsOnlyWhenCoresOrMemoryChange(t *testing.T) {
	_, g := testVMGuest(t)
	ctx := context.Background()
	calls := vmQM(t, "", nil)
	restart, err := g.Resize(ctx, "201", &agentv1.Resources{Vcpu: 1, MemoryMb: 1024})
	if err != nil || restart {
		t.Fatalf("unchanged size: restart=%v err=%v", restart, err)
	}
	if !slices.ContainsFunc(*calls, func(c []string) bool { return slices.Equal(c, []string{"resize", "201", "scsi0", "16G"}) }) {
		t.Fatalf("disk not grown to RootfsGB: %v", *calls)
	}
	restart, err = g.Resize(ctx, "201", &agentv1.Resources{Vcpu: 2, MemoryMb: 2048})
	if err != nil || !restart {
		t.Fatalf("changed size: restart=%v err=%v", restart, err)
	}
	if !slices.ContainsFunc(*calls, func(c []string) bool {
		return slices.Equal(c, []string{"set", "201", "--cores", "2", "--memory", "2048"})
	}) {
		t.Fatalf("calls %v", *calls)
	}
}

func TestVMResizeNeverShrinksTheDisk(t *testing.T) {
	o, g := testVMGuest(t)
	o.cfg.RootfsGB = 1
	calls := vmQM(t, "", nil)
	if _, err := g.Resize(context.Background(), "201", &agentv1.Resources{}); err != nil {
		t.Fatal(err)
	}
	for _, c := range *calls {
		if c[0] == "resize" {
			t.Fatalf("shrink attempted: %v", c)
		}
	}
}

func TestVMSetNetworkKeepsTheMACAndUpdatesCloudInit(t *testing.T) {
	_, g := testVMGuest(t)
	calls := vmQM(t, "", nil)
	err := g.SetNetwork(context.Background(), "201", netSpec{Bridge: "vmbr0", Network: &agentv1.NetworkConfig{
		Mode: "static", IpCidr: "10.0.0.5/24", Gateway: "10.0.0.1"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"set", "201", "--net0", "virtio=BC:24:11:AA:BB:CC,bridge=vmbr0", "--ipconfig0", "ip=10.0.0.5/24,gw=10.0.0.1", "--delete", "nameserver"}
	if !slices.ContainsFunc(*calls, func(c []string) bool { return slices.Equal(c, want) }) {
		t.Fatalf("calls %v", *calls)
	}
	if !slices.Equal((*calls)[len(*calls)-1], []string{"cloudinit", "update", "201"}) {
		t.Fatalf("no cloudinit update: %v", *calls)
	}
}

func TestVMCreateAndConfigureUseNativeCloudInitOptions(t *testing.T) {
	_, g := testVMGuest(t)
	calls := vmQM(t, "", nil)
	var keyFile string
	req := createReq{Vmid: "201", BaseVmid: "9000", Hostname: "my-app", Storage: "local-lvm", Cores: "2", MemoryMB: "2048",
		VMImage: "debian-12", AuthorizedKey: "ssh-ed25519 AAAAC3 ownship-agent",
		Net:     netSpec{Bridge: "vmbr0", Network: &agentv1.NetworkConfig{Mode: "static", IpCidr: "10.0.0.5/24", Gateway: "10.0.0.1", Nameservers: []string{"1.1.1.1", "9.9.9.9"}}},
		LogLine: noLog}
	if err := g.Create(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal((*calls)[0], []string{"clone", "9000", "201", "--name", "my-app", "--full", "1", "--storage", "local-lvm"}) {
		t.Fatalf("clone %v", (*calls)[0])
	}
	orig := qmOutput
	qmOutput = func(ctx context.Context, args ...string) ([]byte, error) {
		if args[0] == "set" {
			i := slices.Index(args, "--sshkeys")
			keyFile = args[i+1]
			data, _ := os.ReadFile(keyFile)
			if string(data) != "ssh-ed25519 AAAAC3 ownship-agent\n" {
				t.Errorf("key file %q", data)
			}
		}
		return orig(ctx, args...)
	}
	if err := g.configure(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	set := strings.Join((*calls)[1], " ")
	for _, want := range []string{"--tags ownship", "--balloon 0", "--cpu host", "--agent enabled=1", "--onboot 1",
		"--net0 virtio,bridge=vmbr0", "--ipconfig0 ip=10.0.0.5/24,gw=10.0.0.1", "--ciuser ownship", "--nameserver 1.1.1.1 9.9.9.9",
		"--cores 2", "--memory 2048"} {
		if !strings.Contains(set, want) {
			t.Errorf("set misses %q: %s", want, set)
		}
	}
	if strings.Contains(set, "cipassword") || strings.Contains(set, "cicustom") {
		t.Fatalf("no password and no custom user-data: %s", set)
	}
	if _, err := os.Stat(keyFile); !os.IsNotExist(err) {
		t.Fatal("temporary key file left behind")
	}
	// With the snippet storage in place only vendor data is added, never user data.
	fakeStorage(t, `{"type":"dir","path":"DIR","content":"iso"}`)
	if err := g.configure(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	for _, c := range *calls {
		if c[0] == "set" && slices.Contains(c, "--ciuser") {
			set = strings.Join(c, " ")
		}
	}
	if !strings.Contains(set, "--cicustom vendor=local:snippets/ownship-vendor.yaml") || strings.Contains(set, "user=") {
		t.Fatalf("vendor snippet: %s", set)
	}
}

func TestVMConfigureRefusesHostileValues(t *testing.T) {
	_, g := testVMGuest(t)
	calls := vmQM(t, "", nil)
	base := createReq{Vmid: "201", Cores: "1", MemoryMB: "1024", AuthorizedKey: "ssh-ed25519 AAAA", Net: netSpec{Bridge: "vmbr0"}, LogLine: noLog}
	for _, mut := range []func(*createReq){
		func(r *createReq) { r.Cores = "1 --delete x" },
		func(r *createReq) { r.MemoryMB = "-1" },
		func(r *createReq) { r.AuthorizedKey = "ssh-ed25519 A\nssh-rsa B" },
		func(r *createReq) { r.Net.Bridge = "vmbr0,firewall=0" },
	} {
		r := base
		mut(&r)
		if err := g.configure(context.Background(), r); err == nil {
			t.Errorf("accepted %+v", r)
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("qm reached with hostile input: %v", *calls)
	}
	for _, r := range []createReq{
		{Vmid: "201 --x", BaseVmid: "9000", Hostname: "a", Storage: "s", LogLine: noLog},
		{Vmid: "201", BaseVmid: "9000", Hostname: "A_B", Storage: "s", LogLine: noLog},
		{Vmid: "201", BaseVmid: "9000", Hostname: "a", Storage: "-s", LogLine: noLog},
	} {
		if err := g.Create(context.Background(), r); err == nil {
			t.Errorf("create accepted %+v", r)
		}
	}
}

func TestVMApplyGPURefuses(t *testing.T) {
	_, g := testVMGuest(t)
	if err := g.ApplyGPU(context.Background(), nil, "201", true, gpuDevices{}, noLog); err == nil {
		t.Fatal("a VM must refuse GPU passthrough")
	}
	if err := g.ApplyGPU(context.Background(), nil, "201", false, gpuDevices{}, noLog); err != nil {
		t.Fatal(err)
	}
}

func TestDiskSizeAndMAC(t *testing.T) {
	for in, want := range map[string]int{"local-lvm:vm-1-disk-0,size=2G": 2, "x,size=1T": 1024, "x,size=512M": 0, "x": -1} {
		got, ok := diskSizeGB(in)
		if (want == -1) == ok || (ok && got != want) {
			t.Errorf("%s: %d %v", in, got, ok)
		}
	}
	if netMAC("virtio=BC:24:11:AA:BB:CC,bridge=vmbr0") != "BC:24:11:AA:BB:CC" || netMAC("virtio=zz,bridge=x") != "" {
		t.Fatal("netMAC")
	}
}
