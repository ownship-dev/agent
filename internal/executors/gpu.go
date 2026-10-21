package executors

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ownship/agent/internal/compose"
	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

// gpuDevices are the GPU device nodes on the agent host or inside an LXC.
type gpuDevices struct {
	Nvidia      []string
	NvidiaNames []string
	Dri         []string
	Kfd         bool // AMD ROCm compute
}

func (g gpuDevices) any() bool { return len(g.Nvidia) > 0 || len(g.Dri) > 0 || g.Kfd }

// nodes returns every device node in a stable order.
func (g gpuDevices) nodes() []string {
	out := append(append([]string{}, g.Nvidia...), g.Dri...)
	if g.Kfd {
		out = append(out, "/dev/kfd")
	}
	sort.Strings(out)
	return out
}

// detectGPUs reads under root so tests can fake /dev.
func detectGPUs(ctx context.Context, root string) gpuDevices {
	var g gpuDevices
	glob := func(pattern string) []string {
		matches, _ := filepath.Glob(filepath.Join(root, pattern))
		out := make([]string, 0, len(matches))
		for _, m := range matches {
			rel, _ := filepath.Rel(root, m)
			out = append(out, "/"+filepath.ToSlash(rel))
		}
		return out
	}
	g.Nvidia = append(g.Nvidia, glob("dev/nvidia[0-9]*")...)
	if len(g.Nvidia) > 0 {
		for _, n := range []string{"dev/nvidiactl", "dev/nvidia-uvm", "dev/nvidia-uvm-tools", "dev/nvidia-modeset"} {
			if _, err := os.Stat(filepath.Join(root, n)); err == nil {
				g.Nvidia = append(g.Nvidia, "/"+n)
			}
		}
		if root == "/" {
			g.NvidiaNames = nvidiaNames(ctx)
		}
	}
	g.Dri = append(glob("dev/dri/renderD*"), glob("dev/dri/card*")...)
	if _, err := os.Stat(filepath.Join(root, "dev/kfd")); err == nil {
		g.Kfd = true
	}
	return g
}

// nvidiaNames parses `nvidia-smi -L`.
func nvidiaNames(ctx context.Context) []string {
	out, err := exec.CommandContext(ctx, "nvidia-smi", "-L").Output()
	if err != nil {
		return nil
	}
	return parseNvidiaSmi(string(out))
}

func parseNvidiaSmi(out string) []string {
	var names []string
	for _, line := range strings.Split(out, "\n") {
		_, rest, ok := strings.Cut(line, ": ")
		if !ok || !strings.HasPrefix(line, "GPU ") {
			continue
		}
		name, _, _ := strings.Cut(rest, " (UUID")
		names = append(names, strings.TrimSpace(name))
	}
	return names
}

const nvidiaToolkitHint = "NVIDIA GPU found but Docker has no nvidia runtime: install nvidia-container-toolkit and restart Docker"

func gpuInventory(g gpuDevices, node string, nvidiaRuntime bool) []*agentv1.Gpu {
	var out []*agentv1.Gpu
	if len(g.Nvidia) > 0 {
		names := g.NvidiaNames
		if len(names) == 0 {
			names = []string{"NVIDIA GPU"}
		}
		for _, n := range names {
			gpu := &agentv1.Gpu{Kind: "nvidia", Name: n, Node: node, Usable: nvidiaRuntime}
			if !nvidiaRuntime {
				gpu.Hint = nvidiaToolkitHint
			}
			out = append(out, gpu)
		}
	}
	for _, d := range g.Dri {
		if strings.Contains(d, "renderD") {
			out = append(out, &agentv1.Gpu{Kind: "dri", Name: "Render node " + filepath.Base(d), Node: node, Usable: true})
		}
	}
	if g.Kfd {
		out = append(out, &agentv1.Gpu{Kind: "kfd", Name: "AMD ROCm compute", Node: node, Usable: true})
	}
	return out
}

// composeGPU is the compose primary's GPU wiring; nil when the project doesn't request one.
func composeGPU(spec RunSpec, g gpuDevices, nvidiaRuntime func() bool) (*compose.GPU, error) {
	if !spec.Resources.GetGpu() {
		return nil, nil
	}
	nvidia := len(g.Nvidia) > 0 && nvidiaRuntime()
	if len(g.Nvidia) > 0 && !nvidia {
		spec.LogLine(agentv1.LogStream_LOG_STREAM_SYSTEM_WARN, nvidiaToolkitHint)
	}
	out := &compose.GPU{Nvidia: nvidia, GroupVideo: g.Kfd}
	if len(g.Dri) > 0 {
		out.Devices = append(out.Devices, "/dev/dri")
	}
	if g.Kfd {
		out.Devices = append(out.Devices, "/dev/kfd")
	}
	if !nvidia && len(out.Devices) == 0 {
		return nil, errors.New("this project asks for a GPU, but none usable was found on the host")
	}
	return out, nil
}
