package executors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

// VMs rely on qm's cloud-init and guest-agent handling as shipped from Proxmox VE 8.0.
const minVMMajor = 8

var errVMNeedsProxmox = errors.New("VMs need a Proxmox host; this machine runs plain Docker")

var (
	vmLookPath      = exec.LookPath
	vmKVMDevice     = "/dev/kvm"
	vmHostArch      = runtime.GOARCH
	vmImageStorages = func(ctx context.Context, node string) ([]byte, error) {
		return exec.CommandContext(ctx, "pvesh", "get", "/nodes/"+node+"/storage",
			"--content", "images", "--output-format", "json").Output()
	}
)

func (o *ProxmoxLXC) vmImageDir() string {
	if o.cfg.VMImageDir != "" {
		return o.cfg.VMImageDir
	}
	return DefaultVMImageDir
}

// VMReport is read fresh on every Hello; image hashes are cached by size and mtime. A host
// with no image is still capable: the catalogue fills the gap on first use.
func (o *ProxmoxLXC) VMReport(ctx context.Context) VMReport {
	reason := o.vmHostBlockedReason(ctx)
	files, dirErr := o.allVMImages(ctx)
	images := make([]*agentv1.VmImage, 0, len(files))
	for _, f := range files {
		images = append(images, f.Info)
	}
	r := VMReport{Capable: reason == "", Convert: true, BlockedReason: reason, Images: images}
	if dirErr != nil {
		r.DirError = fmt.Sprintf("The VM image directory can't be used: %v.", dirErr)
	}
	return r
}

// vmHostBlockedReason checks the host alone; images only matter when a VM is created.
func (o *ProxmoxLXC) vmHostBlockedReason(ctx context.Context) string {
	if vmHostArch != "amd64" {
		return "VMs need an x86_64 host."
	}
	v := o.PveVersion(ctx)
	if _, major, _, ok := parsePveVersion("pve-manager/" + v); !ok || major < minVMMajor {
		if v == "" {
			v = "an unknown version"
		}
		return fmt.Sprintf("VMs need Proxmox VE %d.0 or newer; this host runs %s.", minVMMajor, v)
	}
	for _, bin := range []string{"qm", "ssh", "qemu-img"} {
		if _, err := vmLookPath(bin); err != nil {
			return bin + " is not installed on the host."
		}
	}
	if _, err := os.Stat(vmKVMDevice); err != nil {
		return "KVM is not available (no /dev/kvm); enable virtualization in the firmware."
	}
	out, err := vmImageStorages(ctx, o.localNodeName(ctx))
	if err != nil {
		return "Could not list the Proxmox storages: " + execErrDetail(err) + "."
	}
	var storages []struct {
		Storage string `json:"storage"`
	}
	if json.Unmarshal(out, &storages) != nil {
		return "Could not read the Proxmox storage list."
	}
	for _, s := range storages {
		if s.Storage == o.cfg.Storage {
			return ""
		}
	}
	return fmt.Sprintf("The storage %q does not accept VM disks; set PROXMOX_STORAGE to one that does.", o.cfg.Storage)
}

// vmMinMemoryMB matches VM_MIN_MEMORY_MB in @ownship/core; less can't boot a cloud image with Docker.
const vmMinMemoryMB, vmDefaultMemoryMB = 768, 1024

// WakeHold gives a sleeping VM the time a cold boot takes.
func (o *ProxmoxLXC) WakeHold(workload string) time.Duration {
	if ref, ok := o.guestFor(workload); ok && ref.g.Kind() == GuestVM {
		return wakeHoldVM
	}
	return wakeHoldMax
}

// UninstallCleanup runs once every workload is destroyed: it removes the templates this agent
// imported, its catalogue copies and VM credentials. The owner's images, links included, stay.
func (o *ProxmoxLXC) UninstallCleanup(ctx context.Context) error {
	if o.vms == nil {
		return nil
	}
	if bases := o.vms.bases(); len(bases) > 0 {
		all, err := clusterGuests(ctx)
		if err != nil {
			return fmt.Errorf("list guests before removing VM templates: %w", err)
		}
		for id, b := range bases {
			o.destroyBase(ctx, all, b.Vmid, func(agentv1.LogStream, string) {})
			_ = o.vms.removeBase(id)
		}
	}
	dir := o.vmCacheDir()
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			id, ok := strings.CutSuffix(e.Name(), ".meta.json")
			if m, err := readCacheMeta(dir, id); ok && err == nil && m.Source == sourceCatalog {
				img, meta := cachePaths(dir, id)
				_ = os.Remove(img)
				_ = os.Remove(meta)
			}
		}
	}
	if o.vm != nil {
		_ = os.Remove(o.vm.paths.Key)
		_ = os.Remove(o.vm.paths.KnownHosts)
		_ = os.RemoveAll(o.vm.paths.ControlDir)
	}
	return nil
}
