package executors

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Proxmox guest commands live behind the guest interface, or a VM workload silently gets pct.
var rawGuestExec = regexp.MustCompile(`"(pct|qm|ssh)"|\b(pct|qm)\(`)

// Files that are the guest layer itself; a new entry needs its reason.
var guestExecFiles = map[string]string{
	"guest_lxc.go":    "the LXC strategy: pct",
	"guest_vm.go":     "the VM strategy: qm",
	"guest_vm_ssh.go": "the VM transport: ssh",
	"vmimage.go":      "template import from local images: qm importdisk",
	"vmcapability.go": "only checks that qm/ssh exist on the host, never runs them",
}

func TestNoRawPctOutsideGuest(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || guestExecFiles[f] != "" {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			if rawGuestExec.MatchString(line) {
				t.Errorf("%s:%d runs pct/qm/ssh directly; go through the workload's guest", f, i+1)
			}
		}
	}
}

func TestRawGuestExecPatternCatchesEachForm(t *testing.T) {
	for _, line := range []string{
		`exec.CommandContext(ctx, "pct", "exec")`,
		`[]string{"qm", "set"}`,
		`runStreaming(ctx, "", "ssh", args, l)`,
		`_ = pct(ctx, "stop", vmid).Run()`,
		`out, _ := qm(ctx, "status", vmid).Output()`,
	} {
		if !rawGuestExec.MatchString(line) {
			t.Errorf("not caught: %s", line)
		}
	}
	if rawGuestExec.MatchString(`ref.cmd(ctx, "ip", "addr")`) {
		t.Error("a guest call must pass")
	}
}
