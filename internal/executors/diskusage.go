package executors

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

func prettyExecError(what string, out []byte, err error) error {
	if msg := strings.TrimSpace(string(out)); msg != "" {
		return fmt.Errorf("%s: %s", what, msg)
	}
	return fmt.Errorf("%s: %w", what, err)
}

// dockerDFRow is one `docker system df` line; sizes are human strings like "1.5GB".
type dockerDFRow struct {
	Type        string `json:"Type"`
	Size        string `json:"Size"`
	Reclaimable string `json:"Reclaimable"`
}

func parseDockerDF(out string, report *agentv1.DiskUsageReport) {
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var row dockerDFRow
		if json.Unmarshal([]byte(line), &row) != nil {
			continue
		}
		// Reclaimable reads like "1.2GB (50%)".
		reclaim := parseBytes(strings.Fields(row.Reclaimable + " 0")[0])
		switch row.Type {
		case "Images":
			report.ImagesBytes += parseBytes(row.Size)
			report.ReclaimableBytes += reclaim
		case "Containers":
			report.ContainersBytes += parseBytes(row.Size)
		case "Local Volumes":
			report.VolumesBytes += parseBytes(row.Size)
		case "Build Cache":
			report.BuildCacheBytes += parseBytes(row.Size)
			report.ReclaimableBytes += reclaim
		}
	}
}

// ownshipVolume splits ownship-<workloadID>-<name>; ids are UUIDs, so the split is fixed.
func ownshipVolume(volumeName string) (workloadID, name string, ok bool) {
	const uuidLen = 36
	rest, found := strings.CutPrefix(volumeName, "ownship-")
	if !found || len(rest) < uuidLen+2 || rest[uuidLen] != '-' {
		return "", "", false
	}
	return rest[:uuidLen], rest[uuidLen+1:], true
}

func (o *LocalDocker) DiskUsage(ctx context.Context) (*agentv1.DiskUsageReport, error) {
	report := &agentv1.DiskUsageReport{}
	var firstErr error
	answered := false
	for _, eng := range o.state.Engines() {
		out, err := runEngineCmd(eng.Cmd(ctx, "system", "df", "--format", "{{json .}}"))
		if err != nil {
			firstErr = err
			continue
		}
		answered = true
		parseDockerDF(string(out), report)
		report.Volumes = append(report.Volumes, verboseVolumes(ctx, eng)...)
	}
	if !answered && firstErr != nil {
		return nil, firstErr
	}
	return report, nil
}

// verboseVolumes omits the breakdown when the engine's format can't be parsed.
func verboseVolumes(ctx context.Context, eng *Engine) []*agentv1.VolumeUsage {
	vout, err := runEngineCmd(eng.Cmd(ctx, "system", "df", "-v", "--format", "{{json .}}"))
	if err != nil {
		return nil
	}
	var verbose struct {
		Volumes []struct {
			Name string `json:"Name"`
			Size string `json:"Size"`
		} `json:"Volumes"`
	}
	if json.Unmarshal(vout, &verbose) != nil {
		return nil
	}
	var out []*agentv1.VolumeUsage
	for _, v := range verbose.Volumes {
		if wl, name, ok := ownshipVolume(v.Name); ok {
			out = append(out, &agentv1.VolumeUsage{WorkloadId: wl, Name: name, SizeBytes: parseBytes(v.Size)})
		}
	}
	return out
}

// Prune never touches volumes; the argv is fixed in PruneArgs.
func (o *LocalDocker) Prune(ctx context.Context) error {
	for _, eng := range o.state.Engines() {
		for _, args := range eng.PruneArgs() {
			if out, err := runEngineCmd(eng.Cmd(ctx, args...)); err != nil {
				return prettyExecError(eng.Name()+" "+strings.Join(args[:2], " "), out, err)
			}
		}
	}
	return nil
}

func (o *LocalDocker) ImageDigest(ctx context.Context, workloadID, image string) (string, error) {
	out, err := o.eng(ctx, workloadID).Cmd(ctx, "image", "inspect",
		"--format", "{{join .RepoDigests \"\\n\"}}", "--", image).Output()
	if err != nil {
		return "", prettyExecError("docker image inspect", out, err)
	}
	return digestFromRepoDigests(string(out)), nil
}

func (o *ProxmoxLXC) DiskUsage(ctx context.Context) (*agentv1.DiskUsageReport, error) {
	report := &agentv1.DiskUsageReport{}
	for workloadID, ref := range o.trackedGuests() {
		if out, err := o.guestEngine(ctx, workloadID, ref).Cmd(ctx, "system", "df", "--format", "{{json .}}").Output(); err == nil {
			parseDockerDF(string(out), report)
		}
		// Fixed script with no user input.
		out, err := ref.cmd(ctx, "sh", "-c", "du -sb /opt/ownship-volumes/* 2>/dev/null || true").Output()
		if err != nil {
			continue
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 2 {
				continue
			}
			size := parseUint(fields[0])
			name := fields[1][strings.LastIndex(fields[1], "/")+1:]
			report.Volumes = append(report.Volumes, &agentv1.VolumeUsage{
				WorkloadId: workloadID,
				Name:       name,
				SizeBytes:  size,
			})
			report.VolumesBytes += size
		}
	}
	return report, nil
}

func (o *ProxmoxLXC) Prune(ctx context.Context) error {
	for workloadID, ref := range o.trackedGuests() {
		eng := o.guestEngine(ctx, workloadID, ref)
		for _, args := range eng.PruneArgs() {
			_ = eng.Cmd(ctx, args...).Run()
		}
	}
	return nil
}

func (o *ProxmoxLXC) ImageDigest(ctx context.Context, workloadID, image string) (string, error) {
	ref, ok := o.guestFor(workloadID)
	if !ok {
		return "", nil
	}
	out, err := o.guestEngine(ctx, workloadID, ref).Cmd(ctx, "image", "inspect",
		"--format", "{{join .RepoDigests \"\\n\"}}", "--", image).Output()
	if err != nil {
		return "", prettyExecError("image inspect in lxc", out, err)
	}
	return digestFromRepoDigests(string(out)), nil
}

// digestFromRepoDigests is empty when the image was never pulled.
func digestFromRepoDigests(out string) string {
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if _, digest, ok := strings.Cut(line, "@"); ok {
			return strings.TrimSpace(digest)
		}
	}
	return ""
}
