package executors

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

// git already refuses ext:: by default; kept so a future git default change can't reopen it.
func TestProtocolAllowlistBlocksExtTransport(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "pwned")
	repo := &agentv1.RepoSource{CloneUrl: "ext::sh -c touch% " + marker}
	args, env, cleanup, err := hostGitAuth(repo, filepath.Join(dir, "ws"))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	cmd := exec.CommandContext(context.Background(), "git",
		append(args, "clone", "--end-of-options", repo.GetCloneUrl(), filepath.Join(dir, "ws"))...)
	cmd.Env = append(os.Environ(), env...)
	if err := cmd.Run(); err == nil {
		t.Fatal("clone of an ext:: URL succeeded")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("ext:: transport ran a command")
	}
}

// file:// is allowed by stock git: without the allowlist a user-supplied URL could clone
// any repository on the agent's own disk.
func TestProtocolAllowlistBlocksLocalFileClones(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "secret-repo")
	if out, err := exec.Command("git", "init", "-q", src).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	run := func(args []string) error {
		return exec.Command("git", append(args, "clone", "-q", "--end-of-options", "file://"+src, filepath.Join(dir, "ws"))...).Run()
	}
	if err := run(nil); err != nil {
		t.Skipf("stock git refused file:// already (%v), nothing to prove here", err)
	}
	_ = os.RemoveAll(filepath.Join(dir, "ws"))
	args, _, cleanup, err := hostGitAuth(&agentv1.RepoSource{}, filepath.Join(dir, "ws"))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if err := run(args); err == nil {
		t.Fatal("file:// clone succeeded through the protocol allowlist")
	}
}

func TestHostGitAuthToken(t *testing.T) {
	args, env, cleanup, err := hostGitAuth(&agentv1.RepoSource{InstallationToken: "gho_SECRET123"}, t.TempDir()+"/ws")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "protocol.allow=never") || !strings.Contains(joined, "credential.helper=!f()") {
		t.Fatalf("args %q", joined)
	}
	if strings.Contains(joined, "gho_SECRET123") {
		t.Fatal("token leaked into argv")
	}
	if len(env) != 1 || env[0] != "OWNSHIP_GIT_TOKEN=gho_SECRET123" {
		t.Fatalf("env %v", env)
	}
}

func TestHostGitAuthDeployKey(t *testing.T) {
	ws := t.TempDir() + "/ws"
	args, _, cleanup, err := hostGitAuth(&agentv1.RepoSource{SshPrivateKey: "KEY"}, ws)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "StrictHostKeyChecking=accept-new") || !strings.Contains(joined, ws+".known_hosts") {
		t.Fatalf("args %q", joined)
	}
	keyPath := strings.Fields(joined[strings.Index(joined, " -i ")+4:])[0]
	info, err := os.Stat(keyPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key file %s: %v %v", keyPath, err, info)
	}
	cleanup()
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatal("deploy key outlived the clone")
	}
}

func TestHostGitAuthPinnedHostKey(t *testing.T) {
	ws := t.TempDir() + "/ws"
	args, _, cleanup, err := hostGitAuth(&agentv1.RepoSource{SshPrivateKey: "KEY", SshKnownHosts: "git.lan ssh-ed25519 AAAA"}, ws)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if !strings.Contains(strings.Join(args, " "), "StrictHostKeyChecking=yes") {
		t.Fatal("pinned host key not enforced")
	}
	if b, _ := os.ReadFile(ws + ".known_hosts"); !strings.Contains(string(b), "git.lan ssh-ed25519") {
		t.Fatalf("known_hosts = %q", b)
	}
}

func TestSSHCommandRejectsUnsafePaths(t *testing.T) {
	if _, err := sshCommandArg("/tmp/k; rm -rf /", "/tmp/kh", false); err == nil {
		t.Fatal("unsafe key path accepted")
	}
}
