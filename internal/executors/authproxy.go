package executors

import (
	"fmt"
	"strings"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
	"golang.org/x/crypto/bcrypt"
)

// The Basic Auth sidecar is Caddy, pinned to a major tag.
const (
	authSidecarSuffix = "-auth"
	authProxyImage    = "caddy:2-alpine"
	// On Proxmox the config lives on the LXC rootfs, since env can't cross pct exec.
	lxcCaddyfileDir  = "/opt/ownship-auth"
	lxcCaddyfilePath = "/opt/ownship-auth/Caddyfile"
)

// renderCaddyfile includes only the bcrypt hash, never the plaintext password.
func renderCaddyfile(auth *agentv1.BasicAuth, upstream string) (string, error) {
	if err := validCaddyToken(auth.GetUser()); err != nil {
		return "", fmt.Errorf("basic auth user: %w", err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(auth.GetPassword()), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	// bcrypt output has no whitespace or braces, so it's a safe token.
	return fmt.Sprintf(":80 {\n\tbasic_auth {\n\t\t%s %s\n\t}\n\treverse_proxy %s\n}\n",
		auth.GetUser(), string(hash), upstream), nil
}

// validCaddyToken rejects whitespace, braces and quotes; payloads are never trusted.
func validCaddyToken(s string) error {
	if s == "" {
		return fmt.Errorf("empty")
	}
	if strings.ContainsAny(s, " \t\r\n{}\"'`") {
		return fmt.Errorf("contains unsafe characters")
	}
	return nil
}

// caddyEntrypointScript writes CADDY_CONFIG from env on every start (local only).
const caddyEntrypointScript = `printf '%s' "$CADDY_CONFIG" > /tmp/Caddyfile && exec caddy run --config /tmp/Caddyfile --adapter caddyfile`
