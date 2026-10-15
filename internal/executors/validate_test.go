package executors

import (
	"regexp"
	"testing"
)

func TestSafeVolumeHostPath(t *testing.T) {
	ok := map[string]string{
		"data":     "/opt/ownship-volumes/data",
		"my-vol-1": "/opt/ownship-volumes/my-vol-1",
		"a/b":      "/opt/ownship-volumes/a/b",
	}
	for name, want := range ok {
		got, err := safeVolumeHostPath(name)
		if err != nil {
			t.Errorf("safeVolumeHostPath(%q) unexpected error: %v", name, err)
			continue
		}
		if got != want {
			t.Errorf("safeVolumeHostPath(%q) = %q, want %q", name, got, want)
		}
	}

	// Escaping the root would be a host filesystem escape (agent runs as root).
	// Absolute names are not escapes: filepath.Join keeps them under the root.
	bad := []string{
		"../etc",
		"../../etc/shadow",
		"..",
		"",
		"foo/../../bar",
	}
	for _, name := range bad {
		if got, err := safeVolumeHostPath(name); err == nil {
			t.Errorf("safeVolumeHostPath(%q) = %q, want error", name, got)
		}
	}
}

func TestValidPctArg(t *testing.T) {
	ok := map[string]*regexp.Regexp{
		"9000":      reVmid,
		"":          reVmid, // empty allowed (optional field)
		"pve-node1": reNode,
		"local:vztmpl/debian-12-standard.tar.zst": reVolid,
		"debian-12":    reBaseOS,
		"ubuntu-24.04": reBaseOS,
	}
	for v, re := range ok {
		if !validPctArg(v, re) {
			t.Errorf("validPctArg(%q) = false, want true", v)
		}
	}
	// Flag smuggling / malformed — must all be rejected.
	bad := map[string]*regexp.Regexp{
		"-9000":         reVmid,
		"9000; rm":      reVmid,
		"--target":      reNode,
		"-evil":         reNode,
		"-x/vztmpl/foo": reVolid,
		"-debian":       reBaseOS,
		"Debian-12":     reBaseOS,
		"debian_12":     reBaseOS,
	}
	for v, re := range bad {
		if validPctArg(v, re) {
			t.Errorf("validPctArg(%q) = true, want false", v)
		}
	}
}
