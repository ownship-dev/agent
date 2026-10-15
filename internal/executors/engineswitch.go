package executors

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

type volumeCopier interface {
	TarCmd(ctx context.Context, eng *Engine, volume string) *exec.Cmd
	UntarCmd(ctx context.Context, eng *Engine, volume string) *exec.Cmd
}

// engineVolumeCopier mounts the volume in a throwaway busybox on each engine.
type engineVolumeCopier struct{}

func (engineVolumeCopier) TarCmd(ctx context.Context, eng *Engine, volume string) *exec.Cmd {
	return eng.Cmd(ctx, "run", "--rm", "-i", "-v", volume+":/data:ro", "--", "busybox",
		"tar", "czf", "-", "-C", "/data", ".")
}

func (engineVolumeCopier) UntarCmd(ctx context.Context, eng *Engine, volume string) *exec.Cmd {
	return eng.Cmd(ctx, "run", "--rm", "-i", "-v", volume+":/data", "--", "busybox",
		"sh", "-c", "find /data -mindepth 1 -delete && tar xzf - -C /data")
}

// migrateVolumes copies each volume; the old copy stays until FinalizeSwitch.
func migrateVolumes(ctx context.Context, c volumeCopier, from, to *Engine, volumes []string, logLine LogFunc) error {
	for _, v := range volumes {
		logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, fmt.Sprintf("copying volume %q from %s to %s", v, from.Name(), to.Name()))
		if err := pipeCmds(c.TarCmd(ctx, from, v), c.UntarCmd(ctx, to, v)); err != nil {
			return fmt.Errorf("volume %q: %w", v, err)
		}
	}
	return nil
}

// pipeCmds runs src | dst and reports which side failed, with its stderr.
func pipeCmds(src, dst *exec.Cmd) error {
	pipe, err := src.StdoutPipe()
	if err != nil {
		return err
	}
	dst.Stdin = pipe
	var srcErr, dstErr bytes.Buffer
	src.Stderr, dst.Stderr = &srcErr, &dstErr
	if err := dst.Start(); err != nil {
		return err
	}
	if err := src.Start(); err != nil {
		_ = dst.Process.Kill()
		_ = dst.Wait()
		return err
	}
	serr := src.Wait()
	derr := dst.Wait()
	if serr != nil {
		return fmt.Errorf("read: %v %s", serr, strings.TrimSpace(srcErr.String()))
	}
	if derr != nil {
		return fmt.Errorf("write: %v %s", derr, strings.TrimSpace(dstErr.String()))
	}
	return nil
}

// copyImage moves a local-only tag (a build or rollback target) between engines.
func copyImage(ctx context.Context, from, to *Engine, image string) error {
	if err := pipeCmds(from.Cmd(ctx, "save", "--", image), to.Cmd(ctx, "load")); err != nil {
		return fmt.Errorf("copy image %s from %s to %s: %w", image, from.Name(), to.Name(), err)
	}
	return nil
}

// switchOps are injectable for tests.
type switchOps struct {
	ensureImage func() error
	stopOld     func() error
	copyVolumes func() error
	runNew      func() (Target, error)
	removeNew   func()
	startOld    func()
}

// runSwitch restores the old container on any failure.
func runSwitch(ops switchOps) (Target, error) {
	if err := ops.ensureImage(); err != nil {
		return Target{}, err
	}
	if err := ops.stopOld(); err != nil {
		ops.startOld()
		return Target{}, err
	}
	if err := ops.copyVolumes(); err != nil {
		ops.startOld()
		return Target{}, err
	}
	t, err := ops.runNew()
	if err != nil {
		ops.removeNew()
		ops.startOld()
		return Target{}, err
	}
	return t, nil
}
