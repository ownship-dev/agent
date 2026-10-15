//go:build podman

package executors

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

// Needs a Linux host with both docker and podman (go test -tags podman). It exercises what
// unit tests fake: a link across engines, a switch that keeps a volume's data, sleep on Resume.
func TestPodmanEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	engines, _ := DetectEngines(ctx)
	o := NewLocalDocker(filepath.Join(t.TempDir(), "engines.json"), engines)
	if _, ok := o.state.Engine(KindPodman); !ok {
		t.Skip("podman not available")
	}
	if _, ok := o.state.Engine(KindDocker); !ok {
		t.Skip("docker not available")
	}
	if err := o.EnableLinkBridge(filepath.Join(t.TempDir(), "lb.json")); err != nil {
		t.Fatal(err)
	}
	log := func(_ agentv1.LogStream, line string) { t.Log(line) }
	const db, app = "00000000-0000-4000-8000-0000000000db", "00000000-0000-4000-8000-0000000000ap"
	t.Cleanup(func() {
		_ = o.Destroy(context.Background(), db, false)
		_ = o.Destroy(context.Background(), app, false)
	})

	redis := RunSpec{WorkloadID: db, Image: "docker.io/library/redis:7-alpine", Port: 6379, IsManagedService: true,
		Volumes: []*agentv1.VolumeMount{{Name: "data", MountPath: "/data"}}, Engine: KindDocker, LogLine: log}
	if _, err := o.Run(ctx, redis); err != nil {
		t.Fatalf("redis on docker: %v", err)
	}
	set, _ := o.AppExecCmd(ctx, db, "redis-cli set k survived && redis-cli save")
	if out, err := set.CombinedOutput(); err != nil {
		t.Fatalf("seed: %v %s", err, out)
	}

	// A podman app reaches the docker redis through the bridge.
	addr, err := o.LinkAddr(ctx, app, db, KindPodman, 6379)
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	host := strings.TrimPrefix(addr, "http://")
	if _, err := o.Run(ctx, RunSpec{WorkloadID: app, Image: "docker.io/library/redis:7-alpine", Port: 6379,
		AllowRoot: true, Engine: KindPodman, LogLine: log}); err != nil {
		t.Fatalf("app on podman: %v", err)
	}
	h, p, _ := strings.Cut(host, ":")
	ping, _ := o.AppExecCmd(ctx, app, "redis-cli -h "+h+" -p "+p+" get k")
	if out, err := ping.CombinedOutput(); err != nil || !strings.Contains(string(out), "survived") {
		t.Fatalf("cross-engine link: %v %s", err, out)
	}

	// Switching redis to podman copies its volume.
	redis.Engine = KindPodman
	if _, err := o.Run(ctx, redis); err != nil {
		t.Fatalf("switch: %v", err)
	}
	if err := o.FinalizeSwitch(ctx, db); err != nil {
		t.Fatal(err)
	}
	get, _ := o.AppExecCmd(ctx, db, "redis-cli get k")
	if out, err := get.CombinedOutput(); err != nil || !strings.Contains(string(out), "survived") {
		t.Fatalf("data after switch: %v %s", err, out)
	}

	// A slept podman workload stays stopped through Resume.
	if err := o.Stop(ctx, app); err != nil {
		t.Fatal(err)
	}
	o.Resume(ctx)
	running, _ := o.eng(ctx, app).Cmd(ctx, "ps", "-q", "--filter", "name=ownship-"+app).Output()
	if strings.TrimSpace(string(running)) != "" {
		t.Fatal("a slept app was woken")
	}
}

// Rootless reads its own env file, so the secret must reach the container.
func TestRootlessPodmanRunsAWorkload(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	engines, _ := DetectEngines(ctx)
	o := NewLocalDocker(filepath.Join(t.TempDir(), "engines.json"), engines)
	if _, ok := o.state.Engine(KindRootless); !ok {
		t.Skip("rootless podman not set up")
	}
	const id = "00000000-0000-4000-8000-00000000r001"
	t.Cleanup(func() { _ = o.Destroy(context.Background(), id, false) })
	_, err := o.Run(ctx, RunSpec{WorkloadID: id, Image: "docker.io/library/busybox:1.36", Port: 8080,
		Args: []string{"sleep", "600"}, Engine: KindRootless, AllowRoot: true,
		Env:     []*agentv1.EnvEntry{{Key: "OWNSHIP_PROBE", Value: "rootless-ok", Secret: true}},
		LogLine: func(_ agentv1.LogStream, line string) { t.Log(line) }})
	if err != nil {
		t.Fatalf("rootless run: %v", err)
	}
	probe, _ := o.AppExecCmd(ctx, id, "echo $OWNSHIP_PROBE")
	if out, err := probe.CombinedOutput(); err != nil || !strings.Contains(string(out), "rootless-ok") {
		t.Fatalf("env not delivered: %v %s", err, out)
	}
}
