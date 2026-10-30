package executors

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
	"github.com/ownship/agent/internal/version"
)

type baseOSFamily string

const (
	familyDebian baseOSFamily = "debian"
	familyUbuntu baseOSFamily = "ubuntu"
	familyRHEL   baseOSFamily = "rhel"
)

// baseOSSpec is a curated entry; Prefix matches the newest pveam appliance.
type baseOSSpec struct {
	ID     string
	Family baseOSFamily
	Prefix string
}

// baseOSCatalogue must stay in sync with packages/core/src/base-os.ts.
var baseOSCatalogue = map[string]baseOSSpec{
	"debian-12":        {ID: "debian-12", Family: familyDebian, Prefix: "debian-12-standard"},
	"debian-13":        {ID: "debian-13", Family: familyDebian, Prefix: "debian-13-standard"},
	"ubuntu-22.04":     {ID: "ubuntu-22.04", Family: familyUbuntu, Prefix: "ubuntu-22.04-standard"},
	"ubuntu-24.04":     {ID: "ubuntu-24.04", Family: familyUbuntu, Prefix: "ubuntu-24.04-standard"},
	"centos-9-stream":  {ID: "centos-9-stream", Family: familyRHEL, Prefix: "centos-9-stream-default"},
	"centos-10-stream": {ID: "centos-10-stream", Family: familyRHEL, Prefix: "centos-10-stream-default"},
}

// parsePveamAvailable keeps only the template names from the "system" section.
func parsePveamAvailable(out string) []string {
	var names []string
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			continue
		}
		if fields[0] != "system" {
			continue
		}
		names = append(names, fields[1])
	}
	return names
}

// newestMatching returns the highest version starting with prefix+"_", or "".
func newestMatching(names []string, prefix string) string {
	needle := strings.ToLower(prefix) + "_"
	var best, bestVersion string
	for _, name := range names {
		if !strings.HasPrefix(strings.ToLower(name), needle) {
			continue
		}
		ver := name[len(needle):]
		if best == "" || version.Compare(ver, bestVersion) > 0 {
			best = name
			bestVersion = ver
		}
	}
	return best
}

// matchVolid returns the newest volid whose filename starts with prefix+"_", or "".
func matchVolid(volids []string, prefix string) string {
	byFilename := make(map[string]string, len(volids))
	filenames := make([]string, 0, len(volids))
	for _, volid := range volids {
		_, filename, ok := strings.Cut(volid, "vztmpl/")
		if !ok {
			continue
		}
		byFilename[filename] = volid
		filenames = append(filenames, filename)
	}
	best := newestMatching(filenames, prefix)
	if best == "" {
		return ""
	}
	return byFilename[best]
}

// familyForTemplate infers the OS family by substring; unsupported distros return "".
func familyForTemplate(nameOrVolid string) baseOSFamily {
	lower := strings.ToLower(nameOrVolid)
	switch {
	case strings.Contains(lower, "debian"):
		return familyDebian
	case strings.Contains(lower, "ubuntu"):
		return familyUbuntu
	case strings.Contains(lower, "centos"),
		strings.Contains(lower, "rocky"),
		strings.Contains(lower, "alma"), // covers "almalinux" too
		strings.Contains(lower, "fedora"):
		return familyRHEL
	default:
		return ""
	}
}

// rankFallback prefers the newest Debian, then Ubuntu, then RHEL-family.
func rankFallback(volids []string) (string, error) {
	byFamily := map[baseOSFamily][]string{}
	var unsupported []string
	for _, volid := range volids {
		filename := volid
		if _, after, ok := strings.Cut(volid, "vztmpl/"); ok {
			filename = after
		}
		fam := familyForTemplate(filename)
		if fam == "" {
			unsupported = append(unsupported, filename)
			continue
		}
		byFamily[fam] = append(byFamily[fam], volid)
	}

	for _, fam := range []baseOSFamily{familyDebian, familyUbuntu, familyRHEL} {
		if volid := newestVolidByVersion(byFamily[fam]); volid != "" {
			return volid, nil
		}
	}

	if len(unsupported) > 0 {
		return "", fmt.Errorf("no supported base OS template found — only unsupported templates present: %s — pick a base OS when creating the project and the agent downloads it automatically", strings.Join(unsupported, ", "))
	}
	return "", fmt.Errorf("no base LXC template available — download one in Proxmox (CT Templates) or pick a base OS when creating the project (the agent downloads it automatically)")
}

// newestVolidByVersion compares the "_<version>_" segment of each filename.
func newestVolidByVersion(volids []string) string {
	var best, bestVersion string
	for _, volid := range volids {
		filename := volid
		if _, after, ok := strings.Cut(volid, "vztmpl/"); ok {
			filename = after
		}
		ver := versionOf(filename)
		if best == "" || version.Compare(ver, bestVersion) > 0 {
			best = volid
			bestVersion = ver
		}
	}
	return best
}

// versionOf returns e.g. "12.10-1" from "debian-12-standard_12.10-1_amd64.tar.zst".
func versionOf(filename string) string {
	parts := strings.Split(filename, "_")
	if len(parts) < 2 {
		return filename
	}
	return parts[1]
}

// resolveBaseOS maps a curated id to a volid on the node, downloading it when absent.
func (o *ProxmoxLXC) resolveBaseOS(ctx context.Context, id, targetNode string, logLine LogFunc) (string, error) {
	spec, ok := baseOSCatalogue[id]
	if !ok {
		return "", fmt.Errorf("unknown base OS %q — update the agent", id)
	}

	node := targetNode
	if node == "" {
		node = o.localNodeName(ctx)
	}

	// A template for the other architecture would create an LXC that can't start.
	arch := o.archOfNode(ctx, node)
	if arch == "" && node == o.localNodeName(ctx) {
		arch = vmHostArch
	}
	storages, sdiags := o.vztmplStoragesOn(ctx, node)
	volids, tdiags := o.templatesOn(ctx, node)
	volids = forArch(volids, arch)
	for _, d := range append(sdiags, tdiags...) {
		logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, d)
	}
	logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, fmt.Sprintf(
		"resolving base OS %q (template prefix %s) on node %s — storages %v: %d template(s) present",
		id, spec.Prefix, node, storages, len(volids)))

	if volid := matchVolid(volids, spec.Prefix); volid != "" {
		logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "using existing template "+volid)
		return volid, nil
	}

	if len(volids) > 0 {
		logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, fmt.Sprintf(
			"no template matches %s_* — present: %s — downloading via pveam",
			spec.Prefix, strings.Join(templateFilenames(volids), ", ")))
	} else {
		logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "no CT templates on this node yet — downloading via pveam")
	}

	// pveam downloads to the agent's node; another node can only use shared storage.
	storage := storages[0]
	if local := o.localNodeName(ctx); targetNode != "" && targetNode != local {
		shared, err := o.firstSharedVztmplStorage(ctx, node)
		if err != nil {
			return "", fmt.Errorf(
				"base OS %q is not available on node %s and the agent (on %s) can only download to a shared storage — none with vztmpl content found; download %s_* in Proxmox (node %s → local → CT Templates) or use a shared template storage",
				id, node, local, spec.Prefix, node)
		}
		storage = shared
	}

	if out, err := exec.CommandContext(ctx, "pveam", "update").CombinedOutput(); err != nil {
		logLine(agentv1.LogStream_LOG_STREAM_SYSTEM_WARN, "pveam update failed, continuing with cached index: "+strings.TrimSpace(string(out)))
	}

	out, err := exec.CommandContext(ctx, "pveam", "available", "--section", "system").Output()
	if err != nil {
		return "", fmt.Errorf(
			"pveam available failed on %s: %s — the appliance index is unreadable; run 'pveam update' on the node and retry (storages scanned: %v)",
			node, execErrDetail(err), storages)
	}
	name := newestMatching(forArch(parsePveamAvailable(string(out)), arch), spec.Prefix)
	if name == "" {
		return "", fmt.Errorf(
			"no downloadable template matching %q in the pveam index — run 'pveam update' on %s and check its appliance index; storages scanned: %v; templates present: %s",
			spec.Prefix, node, storages, strings.Join(templateFilenames(volids), ", "))
	}

	logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "downloading base OS template "+name+" to storage "+storage)
	if err := runStreaming(ctx, "", "pveam", []string{"download", storage, name}, logLine); err != nil {
		return "", fmt.Errorf("pveam download of %s to %s failed: %w — check the node's network access and the storage's free space", name, storage, err)
	}

	// Safe: storage comes from pvesh and name is anchored to the compiled-in catalogue.
	return storage + ":vztmpl/" + name, nil
}

// forArch drops the names built for another architecture; names that say none stay.
func forArch(names []string, arch string) []string {
	out := names[:0:0]
	for _, n := range names {
		if foreignArch(arch, n) == "" {
			out = append(out, n)
		}
	}
	return out
}

func templateFilenames(volids []string) []string {
	out := make([]string, 0, len(volids))
	for _, volid := range volids {
		if _, after, ok := strings.Cut(volid, "vztmpl/"); ok {
			out = append(out, after)
		} else {
			out = append(out, volid)
		}
	}
	return out
}

// execErrDetail surfaces the stderr an *exec.ExitError holds.
func execErrDetail(err error) string {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
		return strings.TrimSpace(string(exitErr.Stderr))
	}
	return err.Error()
}
