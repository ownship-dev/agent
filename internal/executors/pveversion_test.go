package executors

import "testing"

func TestGPUPassthroughNeedsProxmox81(t *testing.T) {
	v, major, minor, ok := parsePveVersion("pve-manager/8.2.4/faa83925c9641325 (running kernel: 6.8.12-1-pve)")
	if !ok || v != "8.2.4" || major != 8 || minor != 2 {
		t.Fatalf("parsed %q %d.%d ok=%v", v, major, minor, ok)
	}
	for version, want := range map[string]bool{
		"8.1": true, "8.1.3": true, "8.2.4": true, "9.0.1": true,
		"8.0.4": false, "7.4.17": false, "": false, "garbage": false,
	} {
		if got := supportsGPUPassthrough(version); got != want {
			t.Errorf("%q: got %v, want %v", version, got, want)
		}
	}
	if err := gpuPassthroughRefusal(""); err == nil || err.Error() != "GPU passthrough needs Proxmox VE 8.1 or newer; this host runs an unknown version" {
		t.Fatalf("refusal = %v", err)
	}
}
