package executors

import (
	"context"
	"os"
	"path/filepath"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

// gitSync updates an existing workspace in place, else does a fresh shallow clone.
func gitSync(ctx context.Context, repo *agentv1.RepoSource, dest string, onLine func(agentv1.LogStream, string)) error {
	if _, err := os.Stat(filepath.Join(dest, ".git")); err == nil {
		if err := gitUpdate(ctx, repo, dest, onLine); err == nil {
			return nil
		}
		onLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "workspace update failed, falling back to fresh clone")
		if err := os.RemoveAll(dest); err != nil {
			return err
		}
	}
	return gitClone(ctx, repo, dest, onLine)
}

// gitClone passes the token through a credential helper reading env, never argv.
func gitClone(ctx context.Context, repo *agentv1.RepoSource, dest string, onLine func(agentv1.LogStream, string)) error {
	args, env, cleanup, err := hostGitAuth(repo, dest)
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

	return runStreamingEnv(ctx, "", "git", args, env, onLine)
}

func gitUpdate(ctx context.Context, repo *agentv1.RepoSource, dir string, onLine func(agentv1.LogStream, string)) error {
	ref := repo.GetRef()
	if ref == "" {
		ref = "HEAD"
	}
	// --end-of-options stops a ref like --upload-pack=... being read as an option.
	auth, env, cleanup, err := hostGitAuth(repo, dir)
	if err != nil {
		return err
	}
	defer cleanup()
	fetch := append(auth, "fetch", "--depth", "1", "--end-of-options", "origin", ref)
	if err := runStreamingEnv(ctx, dir, "git", fetch, env, onLine); err != nil {
		return err
	}
	if err := runStreaming(ctx, dir, "git", []string{"reset", "--hard", "FETCH_HEAD"}, onLine); err != nil {
		return err
	}
	// Remove untracked files so the build context matches the commit.
	return runStreaming(ctx, dir, "git", []string{"clean", "-fd"}, onLine)
}
