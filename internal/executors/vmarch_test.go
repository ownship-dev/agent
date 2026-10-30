package executors

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestArchFromNameReadsWholeTokens(t *testing.T) {
	cases := map[string]string{
		"debian-12-standard_12.10-1_arm64.tar.zst":       "arm64",
		"noble-server-cloudimg-amd64.img":                "amd64",
		"Fedora-Cloud-Base-Generic-42-1.1.aarch64.qcow2": "arm64",
		"AlmaLinux-9-GenericCloud-latest.x86_64.qcow2":   "amd64",
		"debian-12-genericcloud.qcow2":                   "",
		"farm64x":                                        "",
	}
	for name, want := range cases {
		if got := archFromName(name); got != want {
			t.Errorf("archFromName(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestForeignArchOnlyRefusesWhatNamesTheOtherArch(t *testing.T) {
	if why := foreignArch("arm64", "noble-server-cloudimg-amd64.img"); why != "built for x86_64; this host is arm64" {
		t.Fatalf("why = %q", why)
	}
	if why := foreignArch("arm64", "my-image.qcow2", "Debian 13"); why != "" {
		t.Fatalf("an unnamed arch must pass, got %q", why)
	}
	if why := foreignArch("amd64", "debian-13-generic-amd64.qcow2"); why != "" {
		t.Fatalf("own arch refused: %q", why)
	}
	got := forArch([]string{"local:vztmpl/debian-12-standard_12.7-1_amd64.tar.zst", "local:vztmpl/debian-12-standard_12.7-1_arm64.tar.zst", "local:vztmpl/custom.tar.gz"}, "arm64")
	if !slices.Equal(got, []string{"local:vztmpl/debian-12-standard_12.7-1_arm64.tar.zst", "local:vztmpl/custom.tar.gz"}) {
		t.Fatalf("forArch = %v", got)
	}
}

func TestVMTemplateBootsThroughUEFIOnArm64(t *testing.T) {
	x86 := strings.Join(vmBaseCreateArgs("debian-13", "vmbr0", "local-lvm", "amd64"), " ")
	if strings.Contains(x86, "--bios") || strings.Contains(x86, "--efidisk0") {
		t.Fatalf("x86 template changed: %s", x86)
	}
	arm := strings.Join(vmBaseCreateArgs("debian-13", "vmbr0", "local-lvm", "arm64"), " ")
	if !strings.Contains(arm, "--bios ovmf") || !strings.Contains(arm, "--efidisk0 local-lvm:1,efitype=4m") {
		t.Fatalf("arm64 template must boot through UEFI: %s", arm)
	}
	if vmCloudInitSlot("amd64") != "--ide2" || vmCloudInitSlot("arm64") != "--scsi1" {
		t.Fatal("arm64's virt machine has no IDE bus; cloud-init goes on SCSI there")
	}
}

func TestArchOfNodeIsAskedOnceAndForgetsFailures(t *testing.T) {
	prev := nodeStatus
	t.Cleanup(func() { nodeStatus = prev })
	asked := 0
	fail := true
	nodeStatus = func(_ context.Context, node string) ([]byte, error) {
		asked++
		if fail {
			return nil, errors.New("node offline")
		}
		return []byte(`{"current-kernel":{"machine":"aarch64","release":"6.8"}}`), nil
	}
	o := &ProxmoxLXC{}
	if a := o.archOfNode(context.Background(), "pi"); a != "" {
		t.Fatalf("an unreadable node must be unknown, got %q", a)
	}
	fail = false
	for range 3 {
		if a := o.archOfNode(context.Background(), "pi"); a != "arm64" {
			t.Fatalf("arch = %q", a)
		}
	}
	if asked != 2 {
		t.Fatalf("asked %d times; a known arch is cached", asked)
	}
	if a := o.archOfNode(context.Background(), "bad node;"); a != "" || asked != 2 {
		t.Fatal("an invalid node name must never reach pvesh")
	}
}
