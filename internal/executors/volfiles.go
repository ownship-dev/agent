package executors

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

// Volume browsing is confined by path validation, argv-only exec and namespace-scoped
// resolution.
const (
	volumeListMaxEntries = 1000
	volumeReadMaxBytes   = 25 * 1024 * 1024
	volumeChunkBytes     = 256 * 1024
)

// validRelPath: empty means the volume root.
func validRelPath(p string) error {
	if p == "" {
		return nil
	}
	if strings.HasPrefix(p, "/") || len(p) > 1024 {
		return fmt.Errorf("invalid path")
	}
	for _, part := range strings.Split(p, "/") {
		// '|' and newlines would corrupt the parsed stat output.
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "\x00\n\r|") {
			return fmt.Errorf("invalid path segment")
		}
	}
	return nil
}

func (d *Deployer) ListVolumeFiles(ctx context.Context, cmd *agentv1.ListVolumeFiles, emit Emit) error {
	reply := func(entries []*agentv1.VolumeFileEntry, errMsg string) error {
		return emit(&agentv1.Event{Payload: &agentv1.Event_VolumeFileList{VolumeFileList: &agentv1.VolumeFileList{
			RequestId: cmd.GetRequestId(), Entries: entries, Error: errMsg,
		}}})
	}
	if err := validRelPath(cmd.GetPath()); err != nil {
		return reply(nil, err.Error())
	}
	listCmd, err := d.orch.VolumeListCmd(ctx, cmd.GetWorkloadId(), cmd.GetVolumeName(), cmd.GetPath())
	if err != nil {
		return reply(nil, err.Error())
	}
	out, err := listCmd.Output()
	if err != nil {
		return reply(nil, firstLine(string(out), err))
	}
	return reply(parseVolumeList(string(out)), "")
}

// parseVolumeList reads `stat -c '%n|%s|%Y|%F'` lines.
func parseVolumeList(out string) []*agentv1.VolumeFileEntry {
	var entries []*agentv1.VolumeFileEntry
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		parts := strings.Split(line, "|")
		if len(parts) != 4 {
			continue
		}
		name := parts[0][strings.LastIndex(parts[0], "/")+1:]
		if name == "" {
			continue
		}
		size, _ := strconv.ParseUint(parts[1], 10, 64)
		mtime, _ := strconv.ParseInt(parts[2], 10, 64)
		entries = append(entries, &agentv1.VolumeFileEntry{
			Name:      name,
			SizeBytes: size,
			IsDir:     strings.Contains(parts[3], "directory"),
			Mtime:     mtime,
		})
		if len(entries) >= volumeListMaxEntries {
			break
		}
	}
	return entries
}

func (d *Deployer) ReadVolumeFile(ctx context.Context, cmd *agentv1.ReadVolumeFile, emit Emit) error {
	fail := func(errMsg string) error {
		return emit(&agentv1.Event{Payload: &agentv1.Event_VolumeFileChunk{VolumeFileChunk: &agentv1.VolumeFileChunk{
			RequestId: cmd.GetRequestId(), Eof: true, Error: errMsg,
		}}})
	}
	if err := validRelPath(cmd.GetPath()); err != nil {
		return fail(err.Error())
	}
	if cmd.GetPath() == "" {
		return fail("not a file")
	}
	readCmd, err := d.orch.VolumeReadCmd(ctx, cmd.GetWorkloadId(), cmd.GetVolumeName(), cmd.GetPath())
	if err != nil {
		return fail(err.Error())
	}
	stdout, err := readCmd.StdoutPipe()
	if err != nil {
		return fail(err.Error())
	}
	var stderr strings.Builder
	readCmd.Stderr = &stderr
	if err := readCmd.Start(); err != nil {
		return fail(err.Error())
	}

	// One byte past the cap tells an oversized file from an exact fit.
	reader := bufio.NewReader(io.LimitReader(stdout, volumeReadMaxBytes+1))
	var seq, total uint64
	buf := make([]byte, volumeChunkBytes)
	for {
		n, readErr := io.ReadFull(reader, buf)
		if n > 0 {
			total += uint64(n)
			if total > volumeReadMaxBytes {
				_ = readCmd.Process.Kill()
				_ = readCmd.Wait()
				return fail(fmt.Sprintf("file exceeds the %d MiB download cap", volumeReadMaxBytes/(1024*1024)))
			}
			seq++
			if err := emit(&agentv1.Event{Payload: &agentv1.Event_VolumeFileChunk{VolumeFileChunk: &agentv1.VolumeFileChunk{
				RequestId: cmd.GetRequestId(), Seq: seq, Data: buf[:n],
			}}}); err != nil {
				_ = readCmd.Process.Kill()
				_ = readCmd.Wait()
				return err
			}
		}
		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			break
		}
		if readErr != nil {
			_ = readCmd.Wait()
			return fail(readErr.Error())
		}
	}
	if err := readCmd.Wait(); err != nil {
		return fail(firstLine(stderr.String(), err))
	}
	return emit(&agentv1.Event{Payload: &agentv1.Event_VolumeFileChunk{VolumeFileChunk: &agentv1.VolumeFileChunk{
		RequestId: cmd.GetRequestId(), Seq: seq + 1, Eof: true,
	}}})
}
