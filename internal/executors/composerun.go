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

// checkoutFS reads an untrusted checkout, on the host or inside an LXC.
type checkoutFS interface {
	// realpath resolves every symlink in p and fails when p doesn't exist.
	realpath(p string) (string, error)
	readHead(p string, n int) ([]byte, error)
}

// inCheckout resolves p and refuses a result outside root: the agent runs as root and the
// repo is untrusted.
func inCheckout(fs checkoutFS, root, p string) (string, error) {
	full, err := fs.realpath(p)
	if err != nil {
		return "", fmt.Errorf("%s not found in the repository", strings.TrimPrefix(p, root+"/"))
	}
	if full != root && !strings.HasPrefix(full, root+"/") {
		return "", errors.New("a path resolves outside the repository")
	}
	return full, nil
}

// buildResolver pins build contexts and Dockerfiles to real paths inside root.
func buildResolver(fs checkoutFS, root, base string) func(string, string) (string, string, error) {
	return func(ctxRel, dfRel string) (string, string, error) {
		ctx, err := inCheckout(fs, root, path.Join(base, ctxRel))
		if err != nil {
			return "", "", fmt.Errorf("build context %s: %w", ctxRel, err)
		}
		if dfRel == "" {
			return ctx, "", nil
		}
		df, err := inCheckout(fs, root, path.Join(ctx, dfRel))
		if err != nil {
			return "", "", fmt.Errorf("dockerfile %s: %w", dfRel, err)
		}
		return ctx, df, nil
	}
}

// composeSource reads the compose file and picks the project directory, pinning every path
// inside the checkout at root; root is empty when the project has no repository.
func composeSource(fs checkoutFS, root string, c *ComposeRun) (src []byte, projectDir string, resolve func(string, string) (string, string, error), err error) {
	if root != "" {
		base := root
		if c.Inline == "" {
			if err := validComposePath(c.Path); err != nil {
				return nil, "", nil, err
			}
			if base, err = inCheckout(fs, root, path.Join(root, path.Dir(c.Path))); err != nil {
				return nil, "", nil, fmt.Errorf("compose file directory: %w", err)
			}
		}
		projectDir, resolve = base, buildResolver(fs, root, base)
	}
	if c.Inline != "" {
		return []byte(c.Inline), projectDir, resolve, nil
	}
	if root == "" {
		return nil, "", nil, errors.New("a compose path needs a repository")
	}
	if err := validComposePath(c.Path); err != nil {
		return nil, "", nil, err
	}
	file, err := inCheckout(fs, root, path.Join(root, c.Path))
	if err != nil {
		return nil, "", nil, fmt.Errorf("compose file: %w", err)
	}
	if src, err = fs.readHead(file, maxComposeBytes+1); err != nil {
		return nil, "", nil, fmt.Errorf("compose file %s could not be read", c.Path)
	}
	return src, projectDir, resolve, nil
}

type hostFS struct{}

func (hostFS) realpath(p string) (string, error) { return filepath.EvalSymlinks(p) }

func (hostFS) readHead(p string, n int) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, int64(n)))
}

// composeUpArgs replaces the repo's .env with an empty one, since COMPOSE_* settings in it
// would steer the CLI.
func composeUpArgs(workloadID, file, projectDir string) []string {
	return []string{"compose", "-p", composeProject(workloadID), "-f", file,
		"--project-directory", projectDir, "--env-file", "/dev/null",
		"up", "-d", "--build", "--remove-orphans"}
}
