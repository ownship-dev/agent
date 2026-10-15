package executors

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

// Base images may lack a locale; glibc's built-in C.UTF-8 stops builds crashing on output.
var buildLocaleEnv = []string{"LANG=C.UTF-8", "LC_ALL=C.UTF-8"}

// buildImage uses a Dockerfile when present or requested, else Nixpacks.
func buildImage(ctx context.Context, eng *Engine, dir, image string, build *agentv1.BuildConfig, onLine func(agentv1.LogStream, string)) error {
	dockerfile := useDockerfile(dir, build.GetType())
	if eng.Rootless {
		staged, cleanup, err := stageForRootless(ctx, eng, dir)
		if err != nil {
			return fmt.Errorf("hand the checkout to %s: %w", RootlessUser, err)
		}
		defer cleanup()
		dir = staged
	}
	for _, cmd := range buildCommands(ctx, eng, dir, image, dockerfile) {
		if err := runStreamingCmd(cmd, onLine); err != nil {
			return err
		}
	}
	return nil
}

// buildCommands runs every rootless step as that user, so root never executes in a
// directory the user controls.
func buildCommands(ctx context.Context, eng *Engine, dir, image string, dockerfile bool) []*exec.Cmd {
	var out []*exec.Cmd
	for _, argv := range buildSteps(eng, image, dockerfile) {
		var cmd *exec.Cmd
		switch {
		case argv[0] == eng.Bin:
			cmd = eng.Cmd(ctx, argv[1:]...)
		case eng.Rootless:
			cmd = eng.Raw(ctx, argv...)
		default:
			cmd = exec.CommandContext(ctx, argv[0], argv[1:]...)
			cmd.Env = os.Environ()
		}
		cmd.Dir = dir
		cmd.Env = append(cmd.Env, buildLocaleEnv...)
		out = append(out, cmd)
	}
	return out
}

// stageForRootless copies the checkout into a directory the rootless user owns; root only
// reads, and tar doesn't follow symlinks.
func stageForRootless(ctx context.Context, eng *Engine, dir string) (string, func(), error) {
	out, err := eng.Raw(ctx, "mktemp", "-d", "-p", eng.home, "ownship-build-XXXXXX").Output()
	if err != nil {
		return "", nil, fmt.Errorf("mktemp: %w", err)
	}
	staged := strings.TrimSpace(string(out))
	cleanup := func() { _ = eng.Raw(context.Background(), "rm", "-rf", "--", staged).Run() }
	read, write := stageCommands(ctx, eng, dir, staged)
	pipe, err := read.StdoutPipe()
	if err != nil {
		cleanup()
		return "", nil, err
	}
	write.Stdin = pipe
	var writeErr strings.Builder
	write.Stderr = &writeErr
	if err := read.Start(); err != nil {
		cleanup()
		return "", nil, err
	}
	werr := write.Run()
	rerr := read.Wait()
	if rerr != nil || werr != nil {
		cleanup()
		return "", nil, fmt.Errorf("copy checkout: read=%v write=%v %s", rerr, werr, strings.TrimSpace(writeErr.String()))
	}
	return staged, cleanup, nil
}

func stageCommands(ctx context.Context, eng *Engine, src, dst string) (read, write *exec.Cmd) {
	read = exec.CommandContext(ctx, "tar", "-C", src, "-cf", "-", ".")
	write = eng.Raw(ctx, "tar", "-C", dst, "-xf", "-", "--no-same-owner")
	return read, write
}

// buildSteps: Docker lets nixpacks build; other engines build the plan nixpacks writes, so
// the image lands where it runs.
func buildSteps(eng *Engine, image string, dockerfile bool) [][]string {
	if dockerfile {
		return [][]string{{eng.Bin, "build", "-t", image, "."}}
	}
	if eng.Kind == KindDocker {
		return [][]string{{"nixpacks", "build", ".", "--name", image}}
	}
	return [][]string{
		{"nixpacks", "build", ".", "--out", "."},
		{eng.Bin, "build", "-f", ".nixpacks/Dockerfile", "-t", image, "."},
	}
}

func useDockerfile(dir string, buildType agentv1.BuildType) bool {
	return chooseDockerfile(buildType, func() bool {
		_, err := os.Stat(filepath.Join(dir, "Dockerfile"))
		return err == nil
	})
}

// chooseDockerfile: an explicit type wins, else hasDockerfile decides.
func chooseDockerfile(buildType agentv1.BuildType, hasDockerfile func() bool) bool {
	switch buildType {
	case agentv1.BuildType_BUILD_TYPE_DOCKERFILE:
		return true
	case agentv1.BuildType_BUILD_TYPE_NIXPACKS:
		return false
	default:
		return hasDockerfile()
	}
}
