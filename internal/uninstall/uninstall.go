// Package uninstall removes the agent, its service and local state from the machine.
package uninstall

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/ownship/agent/internal/config"
)

const unit = "ownship-agent"

// Script is the removal shell run outside the agent's own process tree, which stopping the
// service kills. Arguments: binary, token file, state dir.
const script = `sleep 2
systemctl disable --now ` + unit + ` 2>/dev/null
rm -f "/etc/systemd/system/` + unit + `.service" "$1" "$1.bak" "$1.new" "$2"
rm -rf "/etc/systemd/system/` + unit + `.service.d" /etc/ownship "$3"
systemctl daemon-reload 2>/dev/null
systemctl reset-failed ` + unit + ` 2>/dev/null
true`

// Run schedules the removal and returns; the caller must then let the process exit.
func Run(cfg config.Config) error {
	bin, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{"sh", "-c", script, "sh", bin, cfg.TokenPath, filepath.Dir(cfg.LxcStatePath)}
	var cmd *exec.Cmd
	if p, err := exec.LookPath("systemd-run"); err == nil {
		cmd = exec.Command(p, append([]string{"--collect", "--quiet"}, args...)...)
	} else {
		cmd = exec.Command(args[0], args[1:]...)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("schedule uninstall: %w", err)
	}
	return cmd.Process.Release()
}
