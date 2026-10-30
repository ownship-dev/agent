package executors

import (
	"regexp"
	"strings"
)

// Mirrors archFromName in packages/core/src/arch.ts: a name only counts when the arch is a
// whole token, so "farm64x" says nothing.
var (
	reArchArm = regexp.MustCompile(`(^|[^a-z0-9])(aarch64|arm64)([^a-z0-9]|$)`)
	reArchX86 = regexp.MustCompile(`(^|[^a-z0-9])(x86_64|amd64)([^a-z0-9]|$)`)
)

// archFromName returns "arm64" or "amd64" when a file or template name says which, else "".
func archFromName(name string) string {
	s := strings.ToLower(name)
	switch {
	case reArchArm.MatchString(s):
		return "arm64"
	case reArchX86.MatchString(s):
		return "amd64"
	}
	return ""
}

// normalizeArch maps uname -m and Go names to Go's; "" for anything else.
func normalizeArch(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "amd64", "x86_64", "x86-64":
		return "amd64"
	case "arm64", "aarch64":
		return "arm64"
	}
	return ""
}

func archLabel(a string) string {
	if a == "amd64" {
		return "x86_64"
	}
	return a
}

// foreignArch says why something built for another architecture can't run on `host`, or "".
// A name that names no architecture passes: the owner placed it, and qm will say if it is wrong.
func foreignArch(host string, names ...string) string {
	for _, n := range names {
		if a := archFromName(n); a != "" && host != "" && a != host {
			return "built for " + archLabel(a) + "; this host is " + archLabel(host)
		}
	}
	return ""
}

// isArm64 is true on hosts where Proxmox guests boot only through UEFI (AAVMF) on the virt machine.
func isArm64(host string) bool { return host == "arm64" }

// vmBaseCreateArgs are qm create's options for an image's template. On arm64 Proxmox boots
// guests only through UEFI from AAVMF, so the template gets OVMF and an EFI vars disk.
func vmBaseCreateArgs(id, bridge, storage, host string) []string {
	args := []string{
		"--name", "ownship-base-" + strings.ReplaceAll(id, "_", "-"),
		"--tags", ownshipBaseTag,
		"--ostype", "l26",
		"--cores", "1",
		"--memory", "1024",
		"--net0", "virtio,bridge=" + bridge,
		"--agent", "enabled=1",
		"--serial0", "socket",
		"--vga", "serial0",
		"--scsihw", "virtio-scsi-single",
	}
	if isArm64(host) {
		args = append(args, "--bios", "ovmf", "--efidisk0", storage+":1,efitype=4m,pre-enrolled-keys=0")
	}
	return args
}

// vmCloudInitSlot is where the cloud-init drive goes: IDE on x86, SCSI on arm64's virt machine,
// which has no IDE bus.
func vmCloudInitSlot(host string) string {
	if isArm64(host) {
		return "--scsi1"
	}
	return "--ide2"
}
