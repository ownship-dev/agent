package executors

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ownship/agent/internal/compose"
	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

type ComposeRun struct {
	// Path inside the checkout, or Inline when the file was pasted.
	Path           string
	Inline         string
	PrimaryService string
	// Host checkout holding Path (LocalDocker); Proxmox clones inside the LXC.
	HostDir string
}

const (
	maxComposeBytes = 256 << 10
	// Next to the user's file, so relative build contexts resolve the same.
	preparedComposeName = ".ownship-compose.yml"
)

// composeProject names the project. Only the primary is ownship-<id>, so each workload
// still has exactly one tracked container.
func composeProject(workloadID string) string { return "osc-" + workloadID }

func composeEnv(env []*agentv1.EnvEntry, port uint32) map[string]string {
	out := map[string]string{"PORT": strconv.FormatUint(uint64(port), 10)}
	for _, e := range env {
		out[e.GetKey()] = e.GetValue()
	}
	return out
}

func prepareCompose(src []byte, o compose.Options, logLine LogFunc) ([]byte, error) {
	o.MaxFileBytes = maxComposeBytes
	r, err := compose.Prepare(src, o)
	if err != nil {
		return nil, err
	}
	for _, n := range r.Notes {
		logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "compose: "+n)
	}
	if len(r.Violations) > 0 {
		lines := make([]string, 0, len(r.Violations))
		for _, v := range r.Violations {
			logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "compose refused: "+v.String())
			lines = append(lines, v.String())
		}
		return nil, fmt.Errorf("compose file refused: %s", strings.Join(lines, "; "))
	}
	return r.File, nil
}

// validComposePath requires a plain relative path inside the checkout.
func validComposePath(rel string) error {
	clean := path.Clean(rel)
	if rel == "" || strings.HasPrefix(rel, "/") || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(rel, "\\") {
		return fmt.Errorf("compose path %q must be a relative path inside the repository", rel)
	}
	return nil
}

// insideRoot resolves every symlink in p and refuses a result outside root: the agent runs
// as root and the repo is untrusted.
func insideRoot(root, p string) (string, error) {
	full, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", fmt.Errorf("%s not found in the repository", strings.TrimPrefix(p, root+string(filepath.Separator)))
	}
	if inside, err := filepath.Rel(root, full); err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		return "", errors.New("a path resolves outside the repository")
	}
	return full, nil
}

// hostBuildResolver pins build contexts and Dockerfiles to real paths inside root.
func hostBuildResolver(root, base string) func(string, string) (string, string, error) {
	return func(ctxRel, dfRel string) (string, string, error) {
		ctx, err := insideRoot(root, filepath.Join(base, filepath.FromSlash(ctxRel)))
		if err != nil {
			return "", "", fmt.Errorf("build context %s: %w", ctxRel, err)
		}
		if dfRel == "" {
			return ctx, "", nil
		}
		df, err := insideRoot(root, filepath.Join(ctx, filepath.FromSlash(dfRel)))
		if err != nil {
			return "", "", fmt.Errorf("dockerfile %s: %w", dfRel, err)
		}
		return ctx, df, nil
	}
}

func readHostCompose(dir, rel string) ([]byte, error) {
	if err := validComposePath(rel); err != nil {
		return nil, err
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	full, err := insideRoot(root, filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return nil, fmt.Errorf("compose file: %w", err)
	}
	f, err := os.Open(full)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxComposeBytes+1))
	if err != nil {
		return nil, err
	}
	return data, nil
}

// composeUpArgs replaces the repo's .env with an empty one, since COMPOSE_* settings in it
// would steer the CLI.
func composeUpArgs(workloadID, file, projectDir string) []string {
	return []string{"compose", "-p", composeProject(workloadID), "-f", file,
		"--project-directory", projectDir, "--env-file", "/dev/null",
		"up", "-d", "--build", "--remove-orphans"}
}
