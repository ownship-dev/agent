package executors

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

// dockerLoginHost passes the password via stdin, never argv or logs.
func dockerLoginHost(ctx context.Context, eng *Engine, auth *agentv1.RegistryAuth) error {
	// -- stops a host starting with "-" being parsed as a flag.
	cmd := eng.Cmd(ctx, "login",
		"-u", auth.GetUsername(), "--password-stdin", "--", auth.GetHost())
	cmd.Stdin = strings.NewReader(auth.GetPassword())
	if out, err := cmd.CombinedOutput(); err != nil {
		// Login output can include the host but never the password.
		return fmt.Errorf("%s login %s: %s", eng.Name(), auth.GetHost(), strings.TrimSpace(string(out)))
	}
	return nil
}

// dockerLogoutHost removes the login, since the daemon is shared by every project here.
func dockerLogoutHost(ctx context.Context, eng *Engine, host string) {
	_ = eng.Cmd(ctx, "logout", "--", host).Run()
}

// dockerConfigJSON is the minimal config.json used inside LXCs (0600, removed after the pull).
func dockerConfigJSON(auth *agentv1.RegistryAuth) string {
	blob, _ := json.Marshal(map[string]any{
		"auths": map[string]any{
			auth.GetHost(): map[string]string{
				"auth": base64.StdEncoding.EncodeToString([]byte(auth.GetUsername() + ":" + auth.GetPassword())),
			},
		},
	})
	return string(blob)
}
