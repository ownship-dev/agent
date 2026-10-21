package executors

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

// Red Hat organization ids are numeric and activation keys are names; neither may carry a
// newline, since each is one line of the script's stdin.
var (
	reRhelOrg = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	reRhelKey = regexp.MustCompile(`^[A-Za-z0-9._-]{1,255}$`)
)

// The credentials arrive on stdin so they never reach ssh's argv on the host. Already
// registered (a redeploy, a reused VM) is left alone.
const rhelRegisterScript = `read -r org; read -r key
if subscription-manager identity >/dev/null 2>&1; then exit 0; fi
exec subscription-manager register --org "$org" --activationkey "$key"`

// Best effort, so a destroy frees the subscription; a VM without subscription-manager no-ops.
const rhelUnregisterScript = `command -v subscription-manager >/dev/null 2>&1 || exit 0
subscription-manager identity >/dev/null 2>&1 || exit 0
subscription-manager unregister`

// vmRunStdin runs argv with stdin; tests replace it.
var vmRunStdin = func(ctx context.Context, argv []string, stdin []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = bytes.NewReader(stdin)
	return cmd.CombinedOutput()
}

// registerRHEL gives a RHEL VM its repositories before anything installs packages in it.
func (g *vmGuest) registerRHEL(ctx context.Context, vmid string, sub *agentv1.RhelSubscription, logLine LogFunc) error {
	org, key := sub.GetOrgId(), sub.GetActivationKey()
	if !reRhelOrg.MatchString(org) || !reRhelKey.MatchString(key) {
		return fmt.Errorf("the Red Hat organization id or activation key has characters Red Hat does not use")
	}
	p, err := g.ExecPrefix(ctx, vmid, false)
	if err != nil {
		return err
	}
	logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "registering VM "+vmid+" with Red Hat (organization "+org+")")
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	out, err := vmRunStdin(ctx, p.Wrap("sh", "-c", rhelRegisterScript), []byte(org+"\n"+key+"\n"))
	if err != nil {
		// subscription-manager never echoes the key, so its output is safe to show.
		return fmt.Errorf("Red Hat registration failed: %s", strings.TrimSpace(redactKey(string(out), key)))
	}
	return nil
}

// unregisterRHEL runs before a VM is destroyed; failures only cost a stale entry at Red Hat.
func (g *vmGuest) unregisterRHEL(ctx context.Context, vmid string) {
	if !g.Running(ctx, vmid) {
		return
	}
	p, err := g.ExecPrefix(ctx, vmid, false)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	_, _ = vmRunStdin(ctx, p.Wrap("sh", "-c", rhelUnregisterScript), nil)
}

func redactKey(s, key string) string {
	if key == "" {
		return s
	}
	return strings.ReplaceAll(s, key, "[activation key]")
}
