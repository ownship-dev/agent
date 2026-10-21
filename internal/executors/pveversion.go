package executors

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// LXC device passthrough (pct set --devN) arrived in Proxmox VE 8.1.
const minGPUPassthroughMajor, minGPUPassthroughMinor = 8, 1

var pveVersionRE = regexp.MustCompile(`pve-manager/(\d+)\.(\d+)(?:\.(\d+))?`)

// parsePveVersion reads e.g. "pve-manager/8.2.4/...".
func parsePveVersion(out string) (version string, major, minor int, ok bool) {
	m := pveVersionRE.FindStringSubmatch(out)
	if m == nil {
		return "", 0, 0, false
	}
	major, _ = strconv.Atoi(m[1])
	minor, _ = strconv.Atoi(m[2])
	version = m[1] + "." + m[2]
	if m[3] != "" {
		version += "." + m[3]
	}
	return version, major, minor, true
}

// supportsGPUPassthrough refuses an unknown version rather than guess.
func supportsGPUPassthrough(version string) bool {
	_, major, minor, ok := parsePveVersion("pve-manager/" + version)
	return ok && (major > minGPUPassthroughMajor || (major == minGPUPassthroughMajor && minor >= minGPUPassthroughMinor))
}

func gpuPassthroughRefusal(version string) error {
	if version == "" {
		version = "an unknown version"
	}
	return fmt.Errorf("GPU passthrough needs Proxmox VE %d.%d or newer; this host runs %s",
		minGPUPassthroughMajor, minGPUPassthroughMinor, version)
}

var pveVersionOutput = func(ctx context.Context) ([]byte, error) {
	return exec.CommandContext(ctx, "pveversion").Output()
}

// PveVersion is read on every call, so an upgrade shows up on the next report.
func (o *ProxmoxLXC) PveVersion(ctx context.Context) string {
	out, err := pveVersionOutput(ctx)
	if err != nil {
		return ""
	}
	v, _, _, _ := parsePveVersion(strings.TrimSpace(string(out)))
	return v
}
