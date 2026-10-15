// Package token stores the homelab token, the agent's only durable credential.
package token

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func Save(path, token string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create token dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		return fmt.Errorf("write token: %w", err)
	}
	return nil
}

func Load(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read token: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}
