// Command agent pairs with the control plane, then holds an outbound stream to it.
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/ownship/agent/internal/client"
	"github.com/ownship/agent/internal/config"
	"github.com/ownship/agent/internal/executors"
	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
	"github.com/ownship/agent/internal/lanproxy"
	"github.com/ownship/agent/internal/logging"
	"github.com/ownship/agent/internal/telemetry"
	"github.com/ownship/agent/internal/token"
	"github.com/ownship/agent/internal/tsingress"
	"github.com/ownship/agent/internal/updater"
)

func main() {
	log := logging.New(os.Stderr, os.Getenv("LOG_FORMAT"), os.Getenv("LOG_LEVEL"), config.Version)

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	cfg, err := config.Load()
	if err != nil {
		log.Error("configuration error", "err", err)
		os.Exit(1)
	}

	switch os.Args[1] {
	case "pair":
		if len(os.Args) < 3 {
			usage()
			os.Exit(2)
		}
		if err := pair(cfg, os.Args[2], log); err != nil {
			log.Error("pairing failed", "err", err)
			os.Exit(1)
		}
	case "run":
		if err := run(cfg, log); err != nil {
			log.Error("agent stopped", "err", err)
			os.Exit(1)
		}
	case "update":
		if err := update(cfg, log); err != nil {
			log.Error("update failed", "err", err)
			os.Exit(1)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: ownship-agent <pair CODE | run | update>")
}

// update replaces this binary, skipping the download when the checksum already matches.
func update(cfg config.Config, log *slog.Logger) error {
	ctx := context.Background()
	need, err := updater.NeedsUpdate(ctx, cfg.OS, cfg.Arch)
	if err != nil {
		return err
	}
	if !need {
		fmt.Fprintln(os.Stderr, "Already on the latest version.")
		return nil
	}
	exe, sum, err := updater.SelfUpdate(ctx, cfg.OS, cfg.Arch)
	if err != nil {
		return err
	}
	log.Info("updated to latest", "binary", exe, "sha256", sum)
	fmt.Fprintln(os.Stderr, "Updated. Restart the service to run it:  systemctl restart ownship-agent")
	return nil
}

// pair exchanges a one-time code for the machine token over plain HTTP, which works through
// Cloudflare Tunnel.
func pair(cfg config.Config, code string, log *slog.Logger) error {
	body, err := proto.Marshal(&agentv1.PairRequest{
		Code:  code,
		Agent: &agentv1.AgentInfo{Version: config.Version, Os: cfg.OS, Arch: cfg.Arch},
	})
	if err != nil {
		return err
	}
	url := strings.TrimRight(cfg.ControlPlaneURL, "/") + "/pair"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("pairing rejected: %s: %s", resp.Status, strings.TrimSpace(string(respBody)))
	}
	var pr agentv1.PairResponse
	if err := proto.Unmarshal(respBody, &pr); err != nil {
		return err
	}
	if err := token.Save(cfg.TokenPath, pr.GetToken()); err != nil {
		return err
	}
	log.Info("paired", "machine_id", pr.GetMachineId(), "token_path", cfg.TokenPath)
	return nil
}

func run(cfg config.Config, log *slog.Logger) error {
	tok, err := token.Load(cfg.TokenPath)
	if err != nil {
		return fmt.Errorf("not paired (run `agent pair CODE` first): %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Telemetry failures only warn; they must never stop deploys.
	shutdownTelemetry, err := telemetry.Start(ctx, config.Version)
	if err != nil {
		log.Warn("tracing disabled", "err", err)
	} else {
		defer func() {
			flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = shutdownTelemetry(flushCtx)
		}()
	}

	orch, err := buildOrchestrator(cfg)
	if err != nil {
		return err
	}
	if ld, ok := orch.(*executors.LocalDocker); ok {
		if err := ld.EnableLinkBridge(cfg.LinkBridgeStatePath); err != nil {
			log.Warn("some cross-engine links did not reopen", "err", err)
		}
	}
	// Before logs are followed, put slept Podman apps back to sleep after a reboot.
	if r, ok := orch.(interface{ Resume(context.Context) }); ok {
		r.Resume(ctx)
	}
	rtl := executors.NewRuntimeLogs(orch)
	rtl.Resync(ctx) // re-attach followers to running workloads
	term := executors.NewTerminalManager(orch)
	waker := executors.NewWaker(orch, rtl)
	lan, err := lanproxy.New(cfg.LanDir)
	if err != nil {
		log.Warn("LAN ingress disabled", "err", err)
		lan = nil
	} else if err := lan.Resume(); err != nil {
		log.Warn("LAN routes not restored", "err", err)
	}
	ts := tsingress.New(cfg.TailscaleDir)
	go ts.Resume(ctx, func(msg string) { log.Warn(msg) })
	deployer := executors.NewDeployer(orch, rtl, waker, lan, ts, cfg.WorkspacesDir, cfg.ServiceStatePath)
	deployer.SetControlPlaneURL(cfg.ControlPlaneURL)
	c := client.New(cfg, tok, deployer, rtl, term, waker, executors.NewRelay(deployer.ServiceURL), log)
	log.Info("agent starting", "control_plane", cfg.ControlPlaneURL, "orchestrator", cfg.Orchestrator)
	// Hourly self-update, only for signed official builds.
	if updater.AutoUpdateEnabled() {
		go autoUpdateLoop(ctx, cfg, log)
	} else {
		log.Info("hourly auto-update disabled (no release signing key compiled in)")
	}
	return c.Run(ctx)
}

const autoUpdateInterval = time.Hour

// autoUpdateLoop re-execs into a newer release; safe because unacked commands are redelivered.
func autoUpdateLoop(ctx context.Context, cfg config.Config, log *slog.Logger) {
	ticker := time.NewTicker(autoUpdateInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			need, err := updater.NeedsUpdate(ctx, cfg.OS, cfg.Arch)
			if err != nil {
				log.Warn("auto-update check failed", "err", err)
				continue
			}
			if !need {
				continue
			}
			log.Info("newer agent release found, updating")
			exe, sum, err := updater.SelfUpdate(ctx, cfg.OS, cfg.Arch)
			if err != nil {
				log.Warn("auto-update failed", "err", err)
				continue
			}
			log.Info("updated, restarting into new binary", "binary", exe, "sha256", sum)
			if err := syscall.Exec(exe, os.Args, os.Environ()); err != nil {
				// Exit so systemd restarts on the new binary.
				log.Error("re-exec after update failed; exiting for service restart", "err", err)
				os.Exit(0)
			}
		}
	}
}

func buildOrchestrator(cfg config.Config) (executors.Orchestrator, error) {
	switch cfg.Orchestrator {
	case "proxmox":
		return executors.NewProxmoxLXC(executors.ProxmoxConfig{
			StatePath:       cfg.LxcStatePath,
			EngineStatePath: cfg.EngineStatePath,
			Storage:         cfg.ProxmoxStorage,
			Bridge:          cfg.ProxmoxBridge,
			RootfsGB:        cfg.ProxmoxRootfsGB,
			VMImageDir:      cfg.VMImageDir,
			VMImageCacheDir: cfg.VMImageCacheDir,
		})
	case "local":
		// Engines still starting are re-detected on use; meanwhile the inventory is incomplete.
		engines, _ := executors.DetectEngines(context.Background())
		return executors.NewLocalDocker(cfg.EngineStatePath, engines), nil
	default:
		return nil, fmt.Errorf("unknown orchestrator %q", cfg.Orchestrator)
	}
}
