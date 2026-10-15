package executors

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// A rootless podman reads its env and compose files itself, so they must live in a directory
// that user created, written by that user: root's 0600 temp files are unreadable to it.
func TestRootlessScratchIsCreatedAndWrittenByTheUser(t *testing.T) {
	var cmds []*exec.Cmd
	restore := runEngineCmd
	t.Cleanup(func() { runEngineCmd = restore })
	runEngineCmd = func(cmd *exec.Cmd) ([]byte, error) {
		cmds = append(cmds, cmd)
		if strings.Contains(strings.Join(cmd.Args, " "), "mktemp") {
			return []byte("/var/lib/ownship-podman/ownship-run-abc\n"), nil
		}
		return nil, nil
	}
	e := rootlessForTest()
	dir, cleanup, err := scratchDir(context.Background(), e)
	if err != nil || dir != "/var/lib/ownship-podman/ownship-run-abc" {
		t.Fatalf("dir=%q err=%v", dir, err)
	}
	if err := writeScratch(context.Background(), e, dir+"/env", "SECRET=1\n"); err != nil {
		t.Fatal(err)
	}
	cleanup()
	for _, c := range cmds {
		if c.SysProcAttr == nil || c.SysProcAttr.Credential == nil || c.SysProcAttr.Credential.Uid != 990 {
			t.Fatalf("%v must run as the rootless user", c.Args)
		}
	}
	if len(cmds) != 3 {
		t.Fatalf("mktemp, write, cleanup expected; got %d", len(cmds))
	}
	if !strings.Contains(strings.Join(cmds[1].Args, " "), dir+"/env") || cmds[1].Stdin == nil {
		t.Fatalf("write: %v", cmds[1].Args)
	}
}

// Rootful engines keep the agent's own private temp files.
func TestRootfulScratchIsAPrivateTempDir(t *testing.T) {
	dir, cleanup, err := scratchDir(context.Background(), NewPodmanEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if err := writeScratch(context.Background(), NewPodmanEngine(), dir+"/env", "A=1\n"); err != nil {
		t.Fatal(err)
	}
}

// Root reads only the agent-owned checkout; the rootless copy gets already-checked paths,
// so a symlink swapped into it is never followed by root.
func TestRebasedResolverPointsCheckedPathsAtTheRootlessCopy(t *testing.T) {
	resolve := func(ctxRel, dfRel string) (string, string, error) {
		return "/srv/checkout/api", "/srv/checkout/api/Dockerfile.prod", nil
	}
	r := rebaseResolver("/srv/checkout", "/var/lib/ownship-podman/b1", resolve)
	c, d, err := r("api", "Dockerfile.prod")
	if err != nil || c != "/var/lib/ownship-podman/b1/api" || d != "/var/lib/ownship-podman/b1/api/Dockerfile.prod" {
		t.Fatalf("c=%q d=%q err=%v", c, d, err)
	}
	if got := rebasePath("/srv/checkout", "/var/lib/ownship-podman/b1", "/srv/checkout"); got != "/var/lib/ownship-podman/b1" {
		t.Fatalf("root %q", got)
	}
}
