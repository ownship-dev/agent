// Package config loads agent settings from the environment.
package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
)

// Set at build time via -ldflags, so they must be vars.
var (
	Version = "0.0.0"
	// ControlPlaneURL is baked in at build time and not configurable at runtime.
	ControlPlaneURL = "https://gateway.ownship.dev"
)

type Config struct {
	ControlPlaneURL string
	// Homelab token file (0600).
	TokenPath string
	OS        string
	Arch      string

	// "local" (Docker on this host) or "proxmox" (an LXC per project).
	Orchestrator string
	// pct create settings for Proxmox.
	ProxmoxStorage  string
	ProxmoxBridge   string
	ProxmoxRootfsGB int
	LxcStatePath    string
	// Per-project checkouts for cached builds.
	WorkspacesDir string
	// Workload id to service URL, for domain changes without a redeploy.
	ServiceStatePath string
	// Container engine per local workload.
	EngineStatePath string
	// Link bridge ports for cross-engine private links.
	LinkBridgeStatePath string
	LanDir              string
	TailscaleDir        string
}

func Load() (Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		// systemd units often lack $HOME; use a service dir rather than fail to start.
		home = "/var/lib/ownship"
	}

	tokenPath := os.Getenv("AGENT_TOKEN_PATH")
	if tokenPath == "" {
		tokenPath = filepath.Join(home, ".ownship", "token")
	}
	lxcStatePath := os.Getenv("AGENT_LXC_STATE_PATH")
	if lxcStatePath == "" {
		lxcStatePath = filepath.Join(home, ".ownship", "lxc-state.json")
	}
	workspacesDir := os.Getenv("AGENT_WORKSPACES_DIR")
	if workspacesDir == "" {
		workspacesDir = filepath.Join(home, ".ownship", "workspaces")
	}
	serviceStatePath := os.Getenv("AGENT_SERVICE_STATE_PATH")
	if serviceStatePath == "" {
		serviceStatePath = filepath.Join(home, ".ownship", "services.json")
	}
	stateDir := filepath.Join(home, ".ownship")
	engineStatePath := os.Getenv("AGENT_ENGINE_STATE_PATH")
	if engineStatePath == "" {
		engineStatePath = filepath.Join(stateDir, "engines.json")
	}
	orchestrator := os.Getenv("ORCHESTRATOR")
	if orchestrator == "" {
		orchestrator = "local"
	}

	rootfsGB, _ := strconv.Atoi(os.Getenv("PROXMOX_ROOTFS_GB"))

	return Config{
		ControlPlaneURL:     ControlPlaneURL,
		TokenPath:           tokenPath,
		OS:                  runtime.GOOS,
		Arch:                runtime.GOARCH,
		Orchestrator:        orchestrator,
		ProxmoxStorage:      os.Getenv("PROXMOX_STORAGE"),
		ProxmoxBridge:       os.Getenv("PROXMOX_BRIDGE"),
		ProxmoxRootfsGB:     rootfsGB,
		LxcStatePath:        lxcStatePath,
		WorkspacesDir:       workspacesDir,
		ServiceStatePath:    serviceStatePath,
		EngineStatePath:     engineStatePath,
		LinkBridgeStatePath: filepath.Join(stateDir, "linkbridge.json"),
		LanDir:              filepath.Join(stateDir, "lan"),
		TailscaleDir:        filepath.Join(stateDir, "tailscale"),
	}, nil
}
