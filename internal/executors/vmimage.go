package executors

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

// DefaultVMImageDir is where the owner puts cloud images; the agent never downloads one.
const DefaultVMImageDir = "/var/lib/ownship/vm-images"

const vmImageMaxBytes = 64 << 30

var vmImageExts = []string{".qcow2", ".img", ".raw"}

// vmImageOwnerUID is who must own the directory and every file in it; tests set their own.
var vmImageOwnerUID = uint32(0)

// qemuImgInfo reports an image's format and backing chain without locking it.
var qemuImgInfo = func(ctx context.Context, path string) ([]byte, error) {
	return exec.CommandContext(ctx, "qemu-img", "info", "--force-share", "--output=json", "--", path).Output()
}

var reVMFamily = regexp.MustCompile(`^[a-z0-9-]{1,24}$`)

// vmImageFile is a discovered image with its trust verdict.
type vmImageFile struct {
	Info *agentv1.VmImage
	Path string
	// qemu-img's detected format, which import passes on explicitly.
	Format string
}

// hashCache keeps a file's sha256 while its size and mtime are unchanged, since the report
// re-scans the directory and an image is hundreds of megabytes.
type hashCache struct {
	mu sync.Mutex
	m  map[string]hashEntry
}

type hashEntry struct {
	size  int64
	mtime time.Time
	sum   string
}

var vmImageHashes = &hashCache{m: map[string]hashEntry{}}

func (c *hashCache) sum(path string, info os.FileInfo) (string, error) {
	c.mu.Lock()
	e, ok := c.m[path]
	c.mu.Unlock()
	if ok && e.size == info.Size() && e.mtime.Equal(info.ModTime()) {
		return e.sum, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	c.mu.Lock()
	c.m[path] = hashEntry{size: info.Size(), mtime: info.ModTime(), sum: sum}
	c.mu.Unlock()
	return sum, nil
}

// trustedPath refuses anything a non-root user could have placed or changed: the image
// directory is root's, and a bad file here would be read by root on the host.
func trustedPath(path string, wantDir bool) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		return nil, fmt.Errorf("is a symlink")
	case wantDir && !info.IsDir():
		return nil, fmt.Errorf("is not a directory")
	case !wantDir && !info.Mode().IsRegular():
		return nil, fmt.Errorf("is not a regular file")
	case info.Mode().Perm()&0o022 != 0:
		return nil, fmt.Errorf("is writable by group or others (chmod go-w)")
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != vmImageOwnerUID {
		return nil, fmt.Errorf("is not owned by root (chown root:root)")
	}
	return info, nil
}

// scanVMImages lists every candidate file, usable or not, so the owner sees what to fix.
// dirErr is set when the directory itself can't be trusted.
func scanVMImages(ctx context.Context, dir string) (images []vmImageFile, dirErr error) {
	if _, err := trustedPath(dir, true); err != nil {
		return nil, fmt.Errorf("%s %v", dir, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	byID := map[string]int{}
	for _, e := range entries {
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if !isVMImageExt(ext) {
			continue
		}
		id := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		img := inspectVMImage(ctx, dir, e.Name(), id)
		byID[id]++
		images = append(images, img)
	}
	for _, img := range images {
		if byID[img.Info.Id] > 1 {
			img.Info.Usable, img.Info.Reason = false, "two files share this image id; keep one"
		}
	}
	sort.Slice(images, func(i, j int) bool { return images[i].Info.Id < images[j].Info.Id })
	return images, nil
}

func isVMImageExt(ext string) bool {
	for _, e := range vmImageExts {
		if ext == e {
			return true
		}
	}
	return false
}

func inspectVMImage(ctx context.Context, dir, name, id string) vmImageFile {
	path := filepath.Join(dir, name)
	img := vmImageFile{Path: path, Info: &agentv1.VmImage{Id: id, Label: id, Family: "other"}}
	fail := func(reason string) vmImageFile {
		img.Info.Usable, img.Info.Reason = false, reason
		return img
	}
	if !validVMImageID(id) {
		img.Info.Id = sanitizeReportedID(id)
		return fail("file name must be lowercase letters, digits, '.', '_' or '-' (at most 48) before the extension")
	}
	info, err := trustedPath(path, false)
	if err != nil {
		return fail("the file " + err.Error())
	}
	img.Info.SizeBytes = uint64(info.Size())
	if info.Size() > vmImageMaxBytes {
		return fail("larger than 64 GiB")
	}
	if err := applyImageMeta(dir, id, img.Info); err != nil {
		return fail(err.Error())
	}
	format, err := checkImageFormat(ctx, path)
	if err != nil {
		return fail(err.Error())
	}
	img.Format = format
	sum, err := vmImageHashes.sum(path, info)
	if err != nil {
		return fail("unreadable: " + err.Error())
	}
	img.Info.Sha256 = sum
	if err := checkSidecarSum(path, sum); err != nil {
		return fail(err.Error())
	}
	img.Info.Usable = true
	return img
}

// sanitizeReportedID keeps a bad name displayable without passing it on as an id.
func sanitizeReportedID(id string) string {
	clean := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, id)
	if len(clean) > 64 {
		clean = clean[:64]
	}
	return clean
}

// applyImageMeta reads the optional <id>.json label/family, held to the same trust rules.
func applyImageMeta(dir, id string, info *agentv1.VmImage) error {
	path := filepath.Join(dir, id+".json")
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return nil
	}
	if _, err := trustedPath(path, false); err != nil {
		return fmt.Errorf("%s.json %v", id, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) > 4096 {
		return fmt.Errorf("%s.json is unreadable", id)
	}
	var meta struct {
		Label  string `json:"label"`
		Family string `json:"family"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return fmt.Errorf("%s.json is not valid JSON", id)
	}
	if l := strings.TrimSpace(sanitizeReportedID(meta.Label)); l != "" {
		info.Label = l
	}
	if reVMFamily.MatchString(meta.Family) {
		info.Family = meta.Family
	}
	return nil
}

// checkImageFormat refuses a backing or data file: either would make the guest read another
// host file, chosen by whoever wrote the image header.
func checkImageFormat(ctx context.Context, path string) (string, error) {
	out, err := qemuImgInfo(ctx, path)
	if err != nil {
		return "", fmt.Errorf("qemu-img cannot read it: %v", execErrDetail(err))
	}
	var info struct {
		Format            string `json:"format"`
		BackingFilename   string `json:"backing-filename"`
		FullBackingFile   string `json:"full-backing-filename"`
		BackingFileFormat string `json:"backing-filename-format"`
		FormatSpecific    struct {
			Data struct {
				DataFile string `json:"data-file"`
			} `json:"data"`
		} `json:"format-specific"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return "", fmt.Errorf("qemu-img gave unparseable output")
	}
	if info.Format != "qcow2" && info.Format != "raw" {
		return "", fmt.Errorf("format %q is not qcow2 or raw", info.Format)
	}
	if info.BackingFilename != "" || info.FullBackingFile != "" || info.BackingFileFormat != "" {
		return "", fmt.Errorf("has a backing file; flatten it with qemu-img convert")
	}
	if info.FormatSpecific.Data.DataFile != "" {
		return "", fmt.Errorf("has an external data file; flatten it with qemu-img convert")
	}
	return info.Format, nil
}

// checkSidecarSum enforces <file>.sha256 when present (sha256sum's format).
func checkSidecarSum(path, sum string) error {
	side := path + ".sha256"
	if _, err := os.Lstat(side); os.IsNotExist(err) {
		return nil
	}
	if _, err := trustedPath(side, false); err != nil {
		return fmt.Errorf("%s %v", filepath.Base(side), err)
	}
	data, err := os.ReadFile(side)
	if err != nil || len(data) > 4096 {
		return fmt.Errorf("%s is unreadable", filepath.Base(side))
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 || !regexp.MustCompile(`^[0-9a-fA-F]{64}$`).MatchString(fields[0]) {
		return fmt.Errorf("%s is not in sha256sum format", filepath.Base(side))
	}
	if !strings.EqualFold(fields[0], sum) {
		return fmt.Errorf("sha256 does not match %s", filepath.Base(side))
	}
	return nil
}

// findVMImage re-validates one image at deploy time: a file removed or changed since the
// last report fails the deploy rather than building from whatever is there now.
func findVMImage(ctx context.Context, dir, id string) (vmImageFile, error) {
	if !validVMImageID(id) {
		return vmImageFile{}, fmt.Errorf("invalid VM image id %q", id)
	}
	images, err := scanVMImages(ctx, dir)
	if err != nil {
		return vmImageFile{}, fmt.Errorf("VM image directory: %w", err)
	}
	for _, img := range images {
		if img.Info.Id != id {
			continue
		}
		if !img.Info.GetUsable() {
			return vmImageFile{}, fmt.Errorf("VM image %s is not usable: %s", id, img.Info.GetReason())
		}
		return img, nil
	}
	return vmImageFile{}, fmt.Errorf("VM image %s is not in %s", id, dir)
}

// reImportedVolid finds the volume qm importdisk created ("unused0:local-lvm:vm-9000-disk-0").
var reImportedVolid = regexp.MustCompile(`unused\d+:([A-Za-z0-9][A-Za-z0-9._:/-]*)`)

// ensureVMBase returns the template for img, importing it on first use. A changed file (new
// sha256) gets a new template and the old one is removed: clones are full, so none needs it.
func (o *ProxmoxLXC) ensureVMBase(ctx context.Context, img vmImageFile, logLine LogFunc) (string, error) {
	id := img.Info.GetId()
	defer o.baseLocks.lock(id)()

	all, err := clusterGuests(ctx)
	if err != nil {
		return "", err
	}
	old, hadOld := o.vms.base(id)
	if hadOld && old.Sha256 == img.Info.GetSha256() {
		if g, ok := all[old.Vmid]; ok && g.is(GuestVM) && g.Base {
			return old.Vmid, nil
		}
		logLine(agentv1.LogStream_LOG_STREAM_SYSTEM_WARN, "the template for "+id+" is gone from Proxmox; importing it again")
	}
	if !reBridge.MatchString(o.cfg.Bridge) || !validPctArg(o.cfg.Storage, reNode) {
		return "", fmt.Errorf("invalid Proxmox bridge or storage in the agent config")
	}

	o.vmidMu.Lock()
	vmid, err := nextVmid(ctx)
	if err != nil {
		o.vmidMu.Unlock()
		return "", err
	}
	logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "importing VM image "+id+" as template "+vmid+" (first use)")
	out, err := qmOutput(ctx, "create", vmid,
		"--name", "ownship-base-"+strings.ReplaceAll(id, "_", "-"),
		"--tags", ownshipBaseTag,
		"--ostype", "l26",
		"--cores", "1",
		"--memory", "1024",
		"--net0", "virtio,bridge="+o.cfg.Bridge,
		"--agent", "enabled=1",
		"--serial0", "socket",
		"--vga", "serial0",
		"--scsihw", "virtio-scsi-single")
	o.vmidMu.Unlock() // vmid is claimed once qm create ran
	if err != nil {
		return "", qmErr("create VM template", out, err)
	}
	// From here a failure leaves a half-built template; remove it.
	fail := func(e error) (string, error) {
		_, _ = qmOutput(context.Background(), "destroy", vmid, "--purge")
		return "", e
	}
	out, err = qmOutput(ctx, "importdisk", vmid, img.Path, o.cfg.Storage, "--format", "raw")
	if err != nil {
		return fail(qmErr("import VM image", out, err))
	}
	m := reImportedVolid.FindSubmatch(out)
	if m == nil {
		return fail(fmt.Errorf("import VM image: could not find the imported disk in qm's output"))
	}
	if out, err := qmOutput(ctx, "set", vmid,
		"--scsi0", string(m[1])+",discard=on",
		"--boot", "order=scsi0",
		"--ide2", o.cfg.Storage+":cloudinit"); err != nil {
		return fail(qmErr("attach VM template disk", out, err))
	}
	if out, err := qmOutput(ctx, "template", vmid); err != nil {
		return fail(qmErr("convert VM template", out, err))
	}
	if err := o.vms.setBase(id, vmBase{Vmid: vmid, Sha256: img.Info.GetSha256()}); err != nil {
		return fail(err)
	}
	if hadOld && old.Vmid != vmid {
		o.destroyBase(ctx, all, old.Vmid, logLine)
	}
	return vmid, nil
}

// destroyBase removes a template this agent imported, and nothing else.
func (o *ProxmoxLXC) destroyBase(ctx context.Context, all map[string]guestInfo, vmid string, logLine LogFunc) {
	g, ok := all[vmid]
	if !ok || !g.is(GuestVM) || !g.Base || g.Tagged {
		return
	}
	if out, err := qmOutput(ctx, "destroy", vmid, "--purge"); err != nil {
		logLine(agentv1.LogStream_LOG_STREAM_SYSTEM_WARN, "could not remove the old template "+vmid+": "+strings.TrimSpace(string(out)))
		return
	}
	logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "removed the old template "+vmid)
}
