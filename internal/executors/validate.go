package executors

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// Payload values that reach pct argv: no leading "-" and no shell-hostile characters.
var (
	reVmid   = regexp.MustCompile(`^[0-9]+$`)
	reNode   = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._-]*$`)
	reVolid  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]*$`)
	reBaseOS = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*$`)
)

// validPctArg allows empty, since callers only pass optional fields.
func validPctArg(s string, re *regexp.Regexp) bool {
	return s == "" || re.MatchString(s)
}

// volumeHostRoot is the only directory Proxmox volume bind-mounts may resolve under.
const volumeHostRoot = "/opt/ownship-volumes"

// safeVolumeHostPath keeps a bind-mount inside volumeHostRoot; the agent runs as root.
func safeVolumeHostPath(name string) (string, error) {
	host := filepath.Clean(filepath.Join(volumeHostRoot, name))
	if host != volumeHostRoot && !strings.HasPrefix(host, volumeHostRoot+"/") {
		return "", fmt.Errorf("volume name escapes %s: %q", volumeHostRoot, name)
	}
	if host == volumeHostRoot {
		return "", fmt.Errorf("empty volume name")
	}
	return host, nil
}
