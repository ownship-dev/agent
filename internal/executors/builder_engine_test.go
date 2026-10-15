package executors

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
)

func TestBuildStepsPerEngine(t *testing.T) {
	img := "ownship/w:abc"
	eq := func(name string, got, want [][]string) {
		t.Helper()
		if !slices.EqualFunc(got, want, slices.Equal[[]string]) {
			t.Errorf("%s: got %v want %v", name, got, want)
		}
	}
	eq("docker dockerfile", buildSteps(NewDockerEngine(), img, true), [][]string{{"docker", "build", "-t", img, "."}})
	// Docker keeps nixpacks driving its own build, exactly as before.
	eq("docker nixpacks", buildSteps(NewDockerEngine(), img, false), [][]string{{"nixpacks", "build", ".", "--name", img}})
	eq("podman dockerfile", buildSteps(NewPodmanEngine(), img, true), [][]string{{"podman", "build", "-t", img, "."}})
	eq("podman nixpacks", buildSteps(NewPodmanEngine(), img, false), [][]string{
		{"nixpacks", "build", ".", "--out", "."},
		{"podman", "build", "-f", ".nixpacks/Dockerfile", "-t", img, "."},
	})
}

// A rootless build never runs as root inside a directory the rootless user controls:
// every step, nixpacks included, carries the rootless credential.
func TestRootlessBuildRunsEveryStepAsTheUser(t *testing.T) {
	for _, dockerfile := range []bool{true, false} {
		for _, cmd := range buildCommands(context.Background(), rootlessForTest(), "/stage", "ownship/w:abc", dockerfile) {
			if cmd.SysProcAttr == nil || cmd.SysProcAttr.Credential == nil || cmd.SysProcAttr.Credential.Uid != 990 {
				t.Fatalf("%v runs without the rootless credential", cmd.Args)
			}
			if cmd.Dir != "/stage" {
				t.Fatalf("%v runs in %q", cmd.Args, cmd.Dir)
			}
		}
	}
}

func TestRootfulBuildKeepsAgentIdentity(t *testing.T) {
	for _, cmd := range buildCommands(context.Background(), NewPodmanEngine(), "/repo", "ownship/w:abc", false) {
		if cmd.SysProcAttr != nil && cmd.SysProcAttr.Credential != nil {
			t.Fatalf("%v must run as the agent", cmd.Args)
		}
	}
}

// The checkout reaches the rootless user as a copy it extracts itself; root only reads.
func TestRootlessStagingCommands(t *testing.T) {
	read, write := stageCommands(context.Background(), rootlessForTest(), "/repo", "/var/lib/ownship-podman/b1")
	if read.SysProcAttr != nil && read.SysProcAttr.Credential != nil {
		t.Fatal("the reader is root, reading the agent-owned checkout")
	}
	if !slices.Equal(read.Args, []string{"tar", "-C", "/repo", "-cf", "-", "."}) {
		t.Fatalf("read %v", read.Args)
	}
	if write.SysProcAttr == nil || write.SysProcAttr.Credential.Uid != 990 {
		t.Fatal("the writer must be the rootless user")
	}
	if !slices.Equal(write.Args, []string{"tar", "-C", "/var/lib/ownship-podman/b1", "-xf", "-", "--no-same-owner"}) {
		t.Fatalf("write %v", write.Args)
	}
}

// Runs the real tar pipeline as the current user (setuid to oneself needs no privilege).
func TestStageForRootlessCopiesWithoutFollowingSymlinks(t *testing.T) {
	src, home := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "app.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/hosts", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	e := &Engine{Kind: KindRootless, Bin: "podman", Rootless: true, home: home,
		cred: &syscall.Credential{Uid: uint32(os.Getuid()), Gid: uint32(os.Getgid()), NoSetGroups: true}}
	staged, cleanup, err := stageForRootless(context.Background(), e, src)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(staged, "app.txt")); string(b) != "hi" {
		t.Fatalf("file not copied: %q", b)
	}
	if fi, err := os.Lstat(filepath.Join(staged, "link")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("a symlink must stay a symlink, never its target's content")
	}
	cleanup()
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Fatal("cleanup must remove the staged copy")
	}
}
