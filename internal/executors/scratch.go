package executors

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// scratchDir is a private directory for files the engine reads; rootless creates it in its
// own home.
func scratchDir(ctx context.Context, eng *Engine) (string, func(), error) {
	if !eng.Rootless {
		dir, err := os.MkdirTemp("", "ownship-run-")
		if err != nil {
			return "", nil, err
		}
		return dir, func() { _ = os.RemoveAll(dir) }, nil
	}
	out, err := runEngineCmd(eng.Raw(ctx, "mktemp", "-d", "-p", eng.home, "ownship-run-XXXXXX"))
	if err != nil {
		return "", nil, fmt.Errorf("scratch dir for %s: %s", eng.Name(), engineErr(err))
	}
	dir := strings.TrimSpace(string(out))
	return dir, func() { _, _ = runEngineCmd(eng.Raw(context.Background(), "rm", "-rf", "--", dir)) }, nil
}

// writeScratch writes a 0600 file; for rootless the user writes it, so root never writes
// into a directory that user controls.
func writeScratch(ctx context.Context, eng *Engine, path, content string) error {
	if !eng.Rootless {
		return os.WriteFile(path, []byte(content), 0o600)
	}
	cmd := eng.Raw(ctx, "sh", "-c", `umask 077 && cat > "$1"`, "sh", filepath.Clean(path))
	cmd.Stdin = strings.NewReader(content)
	if _, err := runEngineCmd(cmd); err != nil {
		return fmt.Errorf("write %s as %s: %s", filepath.Base(path), RootlessUser, engineErr(err))
	}
	return nil
}

func rebasePath(root, staged, p string) string {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return staged
	}
	return filepath.Join(staged, rel)
}

// rebaseResolver checks the agent-owned checkout and only points the result at the rootless
// copy.
func rebaseResolver(root, staged string, resolve func(string, string) (string, string, error)) func(string, string) (string, string, error) {
	return func(ctxRel, dfRel string) (string, string, error) {
		c, d, err := resolve(ctxRel, dfRel)
		if err != nil {
			return "", "", err
		}
		if d != "" {
			d = rebasePath(root, staged, d)
		}
		return rebasePath(root, staged, c), d, nil
	}
}
