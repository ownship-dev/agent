package executors

import (
	"context"
	"os/exec"
	"time"
)

// systemctl runs system units as root and a rootless user's units as that user.
func (e *Engine) systemctl(ctx context.Context, args ...string) *exec.Cmd {
	argv := []string{"systemctl"}
	if e.Rootless {
		argv = append(argv, "--user")
	}
	return e.Raw(ctx, append(argv, args...)...)
}

// Containers may be started well after the agent, so Resume runs in rounds.
var resumeDelays = []time.Duration{0, 15 * time.Second, 45 * time.Second, 90 * time.Second, 3 * time.Minute}

func resumeRounds(ctx context.Context, n int, sleep func(time.Duration), round func()) {
	for i := 0; i < n && ctx.Err() == nil; i++ {
		if i < len(resumeDelays) {
			sleep(resumeDelays[i])
		}
		round()
	}
}

func sleepCtx(ctx context.Context) func(time.Duration) {
	return func(d time.Duration) {
		select {
		case <-ctx.Done():
		case <-time.After(d):
		}
	}
}

// Resume puts slept Podman workloads back to sleep after a reboot woke them.
func (o *LocalDocker) Resume(ctx context.Context) {
	for _, e := range o.state.Engines() {
		if e.Kind != KindDocker {
			_, _ = runEngineCmd(e.systemctl(ctx, "enable", "--now", "podman-restart.service"))
		}
	}
	go resumeRounds(ctx, len(resumeDelays), sleepCtx(ctx), func() { o.resumeRound(ctx) })
}

func (o *LocalDocker) resumeRound(ctx context.Context) {
	for _, id := range o.state.Asleep() {
		if eng := o.eng(ctx, id); eng.Kind != KindDocker {
			_ = o.stopOn(ctx, eng, id)
		}
	}
}

// Resume does the same for Podman apps in an LXC that booted again.
func (o *ProxmoxLXC) Resume(ctx context.Context) {
	go resumeRounds(ctx, len(resumeDelays), sleepCtx(ctx), func() { o.resumeRound(ctx) })
}

func (o *ProxmoxLXC) resumeRound(ctx context.Context) {
	for _, id := range o.engines.Asleep() {
		vmid, ok := o.state.get(id)
		if !ok {
			continue
		}
		if eng := o.lxcEngine(ctx, id, vmid); eng.Kind != KindDocker {
			_, _ = runEngineCmd(eng.Cmd(ctx, "stop", lxcAppContainer))
		}
	}
}
