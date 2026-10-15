package executors

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func fakeDev(t *testing.T, nodes ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, n := range nodes {
		p := filepath.Join(root, n)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestDetectGPUs(t *testing.T) {
	root := fakeDev(t, "dev/nvidia0", "dev/nvidiactl", "dev/nvidia-uvm", "dev/dri/renderD128", "dev/dri/card0", "dev/kfd")
	g := detectGPUs(context.Background(), root)
	want := []string{"/dev/dri/card0", "/dev/dri/renderD128", "/dev/kfd", "/dev/nvidia-uvm", "/dev/nvidia0", "/dev/nvidiactl"}
	if !reflect.DeepEqual(g.nodes(), want) {
		t.Fatalf("nodes = %v", g.nodes())
	}
	if none := detectGPUs(context.Background(), fakeDev(t)); none.any() {
		t.Fatal("found a GPU on an empty /dev")
	}
}

func TestGPURunArgs(t *testing.T) {
	g := gpuDevices{Nvidia: []string{"/dev/nvidia0"}, Dri: []string{"/dev/dri/renderD128"}, Kfd: true}
	args, _, err := NewDockerEngine().GPUArgs(g, true)
	if err != nil || strings.Join(args, " ") != "--gpus all --device /dev/dri --device /dev/kfd --group-add video" {
		t.Fatalf("args = %v err = %v", args, err)
	}
	// NVIDIA without the toolkit is explained, not silently ignored.
	if _, notes, err := NewDockerEngine().GPUArgs(gpuDevices{Nvidia: []string{"/dev/nvidia0"}}, false); err == nil || len(notes) != 1 {
		t.Fatalf("notes = %v err = %v", notes, err)
	}
	if _, _, err := NewDockerEngine().GPUArgs(gpuDevices{}, true); err == nil {
		t.Fatal("no GPU should refuse")
	}
}

func TestParseNvidiaSmi(t *testing.T) {
	out := "GPU 0: NVIDIA GeForce RTX 3090 (UUID: GPU-abc)\nGPU 1: NVIDIA A2000 (UUID: GPU-def)\n"
	if got := parseNvidiaSmi(out); !reflect.DeepEqual(got, []string{"NVIDIA GeForce RTX 3090", "NVIDIA A2000"}) {
		t.Fatalf("names = %v", got)
	}
}

func TestParseGPUDevEntriesKeepsUnrelatedDevices(t *testing.T) {
	cfg := "arch: amd64\ndev0: /dev/ttyUSB0\ndev1: /dev/dri/renderD128,mode=0666\ndev2: path=/dev/nvidia0\nmemory: 512\n"
	ours, used := parseGPUDevEntries(cfg)
	if !reflect.DeepEqual(ours, map[string]string{"dev1": "/dev/dri/renderD128", "dev2": "/dev/nvidia0"}) {
		t.Fatalf("ours = %v", ours)
	}
	if !used["dev0"] || len(used) != 3 {
		t.Fatalf("used = %v", used)
	}
}
