package executors

import (
	"context"
	"slices"
	"testing"
	"time"
)

func TestLocalResumeReStopsAsleepPodmanWorkloads(t *testing.T) {
	o := newTestLocal(t, NewDockerEngine(), NewPodmanEngine())
	_ = o.state.Set("sleepy", KindPodman)
	_ = o.state.SetAsleep("sleepy", true)
	_ = o.state.Set("awake", KindPodman)
	_ = o.state.Set("dockersleepy", KindDocker)
	_ = o.state.SetAsleep("dockersleepy", true)
	calls := fakeEngineCmds(t, func([]string) ([]byte, error) { return nil, nil })
	o.Resume(context.Background())
	o.resumeRound(context.Background())
	has := func(want ...string) bool {
		return slices.ContainsFunc(*calls, func(a []string) bool { return slices.Equal(a, want) })
	}
	if !has("systemctl", "enable", "--now", "podman-restart.service") {
		t.Fatalf("podman-restart must be enabled: %v", *calls)
	}
	if !has("podman", "stop", "ownship-sleepy") {
		t.Fatalf("slept podman workload must be re-stopped: %v", *calls)
	}
	for _, c := range *calls {
		if slices.Contains(c, "ownship-awake") || slices.Contains(c, "ownship-dockersleepy") {
			t.Fatalf("only slept podman workloads are touched (docker keeps unless-stopped): %v", c)
		}
	}
}

func TestRootlessSystemctlRunsAsTheUser(t *testing.T) {
	cmd := rootlessForTest().systemctl(context.Background(), "enable", "--now", "podman-restart.service")
	if !slices.Equal(cmd.Args, []string{"systemctl", "--user", "enable", "--now", "podman-restart.service"}) {
		t.Fatalf("args %v", cmd.Args)
	}
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.Credential.Uid != 990 {
		t.Fatal("a user unit is managed as its user")
	}
}

func TestProxmoxResumeReStopsAsleepPodmanApps(t *testing.T) {
	o := newTestProxmox(t)
	_ = o.state.set("sleepy", "105")
	_ = o.engines.Set("sleepy", KindPodman)
	_ = o.engines.SetAsleep("sleepy", true)
	calls := fakeEngineCmds(t, func([]string) ([]byte, error) { return nil, nil })
	o.resumeRound(context.Background())
	if !slices.ContainsFunc(*calls, func(a []string) bool {
		return slices.Equal(a, []string{"pct", "exec", "105", "--", "podman", "stop", "app"})
	}) {
		t.Fatalf("calls %v", *calls)
	}
}

// Older podman-restart units start only `always` containers, so Podman never relies on
// unless-stopped; slept apps are re-stopped by Resume instead.
func TestRestartPolicyPerEngine(t *testing.T) {
	if NewDockerEngine().RestartPolicy() != "unless-stopped" {
		t.Fatal("docker keeps unless-stopped")
	}
	if NewPodmanEngine().RestartPolicy() != "always" || rootlessForTest().RestartPolicy() != "always" {
		t.Fatal("podman uses always")
	}
}

// podman-restart (or pve-guests) may start containers after the agent: Resume repeats, and
// re-reads the sleep state each round so an app woken meanwhile is left alone.
func TestResumeRepeatsAndRespectsAWakeInBetween(t *testing.T) {
	o := newTestLocal(t, NewDockerEngine(), NewPodmanEngine())
	_ = o.state.Set("sleepy", KindPodman)
	_ = o.state.SetAsleep("sleepy", true)
	var stops int
	fakeEngineCmds(t, func(args []string) ([]byte, error) {
		if slices.Equal(args, []string{"podman", "stop", "ownship-sleepy"}) {
			stops++
		}
		return nil, nil
	})
	rounds := 0
	resumeRounds(context.Background(), 3, func(time.Duration) {}, func() {
		rounds++
		if rounds == 2 {
			_ = o.state.SetAsleep("sleepy", false) // the user woke it
		}
		o.resumeRound(context.Background())
	})
	if rounds != 3 || stops != 1 {
		t.Fatalf("rounds=%d stops=%d", rounds, stops)
	}
}
