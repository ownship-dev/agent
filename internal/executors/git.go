package executors

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

// gitHost is where a checkout lives: the agent's own host, or inside a workload's LXC. The
// clone, fetch and fallback logic is written once against it.
type gitHost interface {
	// git returns argv to run git with these args; env is only set on the host.
	git(ctx context.Context, args []string, env []string) *exec.Cmd
	// auth returns the credential args and env for repo, and removes what it wrote.
	auth(ctx context.Context, repo *agentv1.RepoSource, dest string) (args, env []string, cleanup func(), err error)
	hasCheckout(ctx context.Context, dest string) bool
	removeAll(ctx context.Context, dest string) error
}

// gitSync updates an existing checkout in place, else does a fresh shallow clone.
func gitSync(ctx context.Context, h gitHost, repo *agentv1.RepoSource, dest string, cache bool, onLine LogFunc) error {
	if cache && h.hasCheckout(ctx, dest) {
		if err := gitUpdate(ctx, h, repo, dest, onLine); err == nil {
			return nil
		}
		onLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "workspace update failed, falling back to fresh clone")
	}
	if err := h.removeAll(ctx, dest); err != nil {
		return err
	}
	return gitClone(ctx, h, repo, dest, onLine)
}

func gitClone(ctx context.Context, h gitHost, repo *agentv1.RepoSource, dest string, onLine LogFunc) error {
	args, env, cleanup, err := h.auth(ctx, repo, dest)
	if err != nil {
		return err
	}
	defer cleanup()
	args = append(args, "clone", "--depth", "1")
	if ref := repo.GetRef(); ref != "" {
		args = append(args, "--branch", ref)
	}
	// --end-of-options stops the url or dest being read as options.
	args = append(args, "--end-of-options", repo.GetCloneUrl(), dest)
	return runStreamingCmd(h.git(ctx, args, env), onLine)
}

func gitUpdate(ctx context.Context, h gitHost, repo *agentv1.RepoSource, dir string, onLine LogFunc) error {
	ref := repo.GetRef()
	if ref == "" {
		ref = "HEAD"
	}
	auth, env, cleanup, err := h.auth(ctx, repo, dir)
	if err != nil {
		return err
	}
	defer cleanup()
	// --end-of-options stops a ref like --upload-pack=... being read as an option.
	fetch := append(auth, "-C", dir, "fetch", "--depth", "1", "--end-of-options", "origin", ref)
	if err := runStreamingCmd(h.git(ctx, fetch, env), onLine); err != nil {
		return err
	}
	if err := runStreamingCmd(h.git(ctx, []string{"-C", dir, "reset", "--hard", "FETCH_HEAD"}, nil), onLine); err != nil {
		return err
	}
	// Remove untracked files so the build context matches the commit.
	return runStreamingCmd(h.git(ctx, []string{"-C", dir, "clean", "-fd"}, nil), onLine)
}

// localGit runs git on the agent's host; the token rides env through a credential helper.
type localGit struct{}

func (localGit) git(ctx context.Context, args []string, env []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	return cmd
}

func (localGit) auth(_ context.Context, repo *agentv1.RepoSource, dest string) ([]string, []string, func(), error) {
	return hostGitAuth(repo, dest)
}

func (localGit) hasCheckout(_ context.Context, dest string) bool {
	_, err := os.Stat(filepath.Join(dest, ".git"))
	return err == nil
}

func (localGit) removeAll(_ context.Context, dest string) error { return os.RemoveAll(dest) }
