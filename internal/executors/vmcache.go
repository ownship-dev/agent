package executors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
	"github.com/ownship/agent/internal/vmfetch"
)

// DefaultVMImageCacheDir holds what the agent fetched: catalogue copies and images from a link.
const DefaultVMImageCacheDir = "/var/lib/ownship/vm-image-cache"

// catalogTTL bounds how stale a catalogue copy may get before the vendor is asked again.
const catalogTTL = 15 * 24 * time.Hour

// Free space a fetch needs beyond the image itself, so a full disk fails before it starts.
const fetchMinFreeBytes = 2 << 30

const (
	sourceLocal   = "local"
	sourceLink    = "link"
	sourceCatalog = "catalog"
)

var reSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// cacheNow is time.Now unless a test pins it.
var cacheNow = time.Now

// The network side of a fetch; tests replace these.
var (
	fetchSmall    = vmfetch.Small
	fetchDownload = vmfetch.Download
)

// diskFree reports the bytes available to root in dir.
var diskFree = func(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}

// cacheMeta sits next to each cached image as <id>.meta.json. A link's URL is never written.
type cacheMeta struct {
	Source    string    `json:"source"`
	Label     string    `json:"label"`
	Family    string    `json:"family"`
	SHA256    string    `json:"sha256"`
	SHA512    string    `json:"sha512,omitempty"`
	URL       string    `json:"url,omitempty"`
	FetchedAt time.Time `json:"fetchedAt"`
	ExpiresAt time.Time `json:"expiresAt,omitzero"`
}

func (m cacheMeta) expired(now time.Time) bool {
	return !m.ExpiresAt.IsZero() && !now.Before(m.ExpiresAt)
}

func (o *ProxmoxLXC) vmCacheDir() string {
	if o.cfg.VMImageCacheDir != "" {
		return o.cfg.VMImageCacheDir
	}
	return DefaultVMImageCacheDir
}

// ensureCacheDir creates the cache root-owned and refuses one anyone else could write to.
func (o *ProxmoxLXC) ensureCacheDir() (string, error) {
	dir := o.vmCacheDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("VM image cache: %w", err)
	}
	if _, err := trustedPath(dir, true); err != nil {
		return "", fmt.Errorf("VM image cache %s %v", dir, err)
	}
	return dir, nil
}

func cachePaths(dir, id string) (img, meta string) {
	return filepath.Join(dir, id+".img"), filepath.Join(dir, id+".meta.json")
}

func readCacheMeta(dir, id string) (cacheMeta, error) {
	_, metaPath := cachePaths(dir, id)
	if _, err := trustedPath(metaPath, false); err != nil {
		return cacheMeta{}, err
	}
	data, err := os.ReadFile(metaPath)
	if err != nil || len(data) > 16<<10 {
		return cacheMeta{}, fmt.Errorf("unreadable metadata")
	}
	var m cacheMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return cacheMeta{}, fmt.Errorf("metadata is not valid JSON")
	}
	if m.Source != sourceLink && m.Source != sourceCatalog {
		return cacheMeta{}, fmt.Errorf("metadata names an unknown source")
	}
	return m, nil
}

func writeCacheMeta(dir, id string, m cacheMeta) error {
	_, metaPath := cachePaths(dir, id)
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return writeFileAtomic(metaPath, data, 0o644)
}

// inspectCached validates a cached image like a hand-placed one and fills in its metadata.
func inspectCached(ctx context.Context, dir, id string, m cacheMeta) vmImageFile {
	img := inspectVMImage(ctx, dir, id+".img", id)
	img.Info.Source = m.Source
	if m.Label != "" {
		img.Info.Label = m.Label
	}
	if reVMFamily.MatchString(m.Family) {
		img.Info.Family = m.Family
	}
	if !m.ExpiresAt.IsZero() {
		img.Info.ExpiresAt = m.ExpiresAt.Unix()
	}
	if img.Info.GetUsable() && img.Info.GetSha256() != m.SHA256 {
		img.Info.Usable, img.Info.Reason = false, "the file changed since it was fetched; delete it and fetch it again"
	}
	return img
}

// scanCachedImages lists the cache's live images; expired catalogue copies are left out.
func scanCachedImages(ctx context.Context, dir string, now time.Time) []vmImageFile {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []vmImageFile
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".meta.json")
		if !ok || !validVMImageID(id) {
			continue
		}
		m, err := readCacheMeta(dir, id)
		if err != nil || m.expired(now) {
			continue
		}
		out = append(out, inspectCached(ctx, dir, id, m))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Info.Id < out[j].Info.Id })
	return out
}

// SweepVMImages removes expired catalogue copies and says whether any went.
func (o *ProxmoxLXC) SweepVMImages() bool {
	dir := o.vmCacheDir()
	if _, err := trustedPath(dir, true); err != nil {
		return false
	}
	return o.sweepExpired(dir, cacheNow())
}

// sweepExpired removes catalogue copies past their expiry that no fetch or import is using.
func (o *ProxmoxLXC) sweepExpired(dir string, now time.Time) (removed bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".meta.json")
		if !ok || !validVMImageID(id) {
			continue
		}
		release, ok := o.imageLocks.tryLock(id)
		if !ok {
			continue
		}
		if m, err := readCacheMeta(dir, id); err == nil && m.Source == sourceCatalog && m.expired(now) {
			imgPath, metaPath := cachePaths(dir, id)
			_ = os.Remove(imgPath)
			removed = os.Remove(metaPath) == nil || removed
		}
		release()
	}
	return removed
}

// allVMImages is the report: hand-placed images, then cached ones whose id no file took.
func (o *ProxmoxLXC) allVMImages(ctx context.Context) (images []vmImageFile, dirErr error) {
	local, dirErr := scanVMImages(ctx, o.vmImageDir())
	if dirErr != nil && errors.Is(dirErr, os.ErrNotExist) {
		dirErr = nil
	}
	seen := map[string]bool{}
	for _, img := range local {
		img.Info.Source = sourceLocal
		seen[img.Info.Id] = true
		images = append(images, img)
	}
	if dir, err := o.ensureCacheDir(); err == nil {
		o.sweepExpired(dir, cacheNow())
		for _, img := range scanCachedImages(ctx, dir, cacheNow()) {
			if !seen[img.Info.Id] {
				images = append(images, img)
			}
		}
	}
	return images, dirErr
}

// resolveVMImage finds the image a VM boots from: the owner's file wins, then the cache, then
// a fetch from src. A catalogue copy is re-checked against the vendor once it expired.
func (o *ProxmoxLXC) resolveVMImage(ctx context.Context, id string, src *agentv1.VmImageSource, logLine LogFunc) (vmImageFile, error) {
	return o.resolveVMImageWith(ctx, id, src, logLine, nil)
}

func (o *ProxmoxLXC) resolveVMImageWith(ctx context.Context, id string, src *agentv1.VmImageSource, logLine LogFunc, progress func(done, total int64)) (vmImageFile, error) {
	if !validVMImageID(id) {
		return vmImageFile{}, fmt.Errorf("invalid VM image id %q", id)
	}
	if local, err := scanVMImages(ctx, o.vmImageDir()); err == nil {
		for _, img := range local {
			if img.Info.Id != id {
				continue
			}
			if !img.Info.GetUsable() {
				return vmImageFile{}, fmt.Errorf("VM image %s is not usable: %s", id, img.Info.GetReason())
			}
			return img, nil
		}
	}
	dir, err := o.ensureCacheDir()
	if err != nil {
		return vmImageFile{}, err
	}
	o.sweepExpired(dir, cacheNow())
	defer o.imageLocks.lock(id)()
	m, metaErr := readCacheMeta(dir, id)
	if metaErr == nil && (m.Source == sourceLink || (!m.expired(cacheNow()) && (src == nil || src.GetUrl() == m.URL))) {
		img := inspectCached(ctx, dir, id, m)
		if !img.Info.GetUsable() {
			return vmImageFile{}, fmt.Errorf("VM image %s is not usable: %s", id, img.Info.GetReason())
		}
		return img, nil
	}
	if src == nil || src.GetUrl() == "" {
		return vmImageFile{}, fmt.Errorf("VM image %s is not on this machine", id)
	}
	var prev *cacheMeta
	if metaErr == nil && m.Source == sourceCatalog {
		prev = &m
	}
	return o.fetchCatalog(ctx, dir, id, src, prev, logLine, progress)
}

// fetchCatalog proves the checksum file first, then downloads only if the cached copy no
// longer matches it, and moves the image into the cache once hash and format check out.
func (o *ProxmoxLXC) fetchCatalog(ctx context.Context, dir, id string, src *agentv1.VmImageSource, prev *cacheMeta, logLine LogFunc, progress func(done, total int64)) (vmImageFile, error) {
	u, err := vmfetch.CheckURL(src.GetUrl(), vmfetch.Mirror)
	if err != nil {
		return vmImageFile{}, fmt.Errorf("VM image %s: %w", id, err)
	}
	name := path.Base(u.Path)
	if name == "" || name == "/" || name == "." {
		return vmImageFile{}, fmt.Errorf("VM image %s: the URL names no file", id)
	}
	// Caught before a multi-gigabyte download that could never boot here.
	if a := normalizeArch(src.GetArch()); a != "" && a != vmHostArch {
		return vmImageFile{}, fmt.Errorf("VM image %s is built for %s; this host is %s", id, archLabel(a), archLabel(vmHostArch))
	}
	if why := foreignArch(vmHostArch, name); why != "" {
		return vmImageFile{}, fmt.Errorf("VM image %s is %s", id, why)
	}
	logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "checking "+name+" against "+u.Hostname()+"'s checksum file")
	sums, err := fetchSmall(ctx, src.GetChecksumUrl(), 1<<20)
	if err != nil {
		return vmImageFile{}, fmt.Errorf("VM image %s checksum file: %w", id, err)
	}
	var sig []byte
	if src.GetSignatureUrl() != "" {
		if sig, err = fetchSmall(ctx, src.GetSignatureUrl(), 64<<10); err != nil {
			return vmImageFile{}, fmt.Errorf("VM image %s signature: %w", id, err)
		}
	}
	text, err := vmfetch.Verified(sums, sig, src.GetSigningKey())
	if err != nil {
		return vmImageFile{}, fmt.Errorf("VM image %s: %w", id, err)
	}
	want, err := vmfetch.FindExpected(text, name)
	if err != nil {
		return vmImageFile{}, fmt.Errorf("VM image %s: %w", id, err)
	}
	meta := cacheMeta{Source: sourceCatalog, Label: cleanLabel(src.GetLabel(), id), Family: cleanFamily(src.GetFamily()),
		URL: src.GetUrl(), FetchedAt: cacheNow(), ExpiresAt: cacheNow().Add(catalogTTL)}
	if prev != nil && prev.URL == src.GetUrl() && want.Matches(vmfetch.Sums{SHA256: prev.SHA256, SHA512: prev.SHA512}) {
		meta.SHA256, meta.SHA512 = prev.SHA256, prev.SHA512
		img := inspectCached(ctx, dir, id, meta)
		if img.Info.GetUsable() {
			if err := writeCacheMeta(dir, id, meta); err != nil {
				return vmImageFile{}, err
			}
			logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "VM image "+id+" is unchanged upstream; keeping the cached copy for 15 more days")
			return img, nil
		}
	}
	logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "downloading VM image "+id+" from "+u.Hostname())
	got, err := downloadInto(ctx, dir, id, src.GetUrl(), vmfetch.Mirror, func(sums vmfetch.Sums) error {
		if !want.Matches(sums) {
			return fmt.Errorf("its %s does not match the vendor's checksum file", want.Algo)
		}
		return nil
	}, progressLogger(logLine, id, progress))
	if err != nil {
		return vmImageFile{}, fmt.Errorf("VM image %s: %w", id, err)
	}
	meta.SHA256, meta.SHA512 = got.SHA256, got.SHA512
	if err := writeCacheMeta(dir, id, meta); err != nil {
		return vmImageFile{}, err
	}
	img := inspectCached(ctx, dir, id, meta)
	if !img.Info.GetUsable() {
		return vmImageFile{}, fmt.Errorf("VM image %s is not usable: %s", id, img.Info.GetReason())
	}
	logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, "VM image "+id+" downloaded and verified")
	return img, nil
}

// downloadInto writes to a private temp file, runs verify on the sums, checks the format,
// and only then renames it to <id>.img. Any failure removes the temp file.
func downloadInto(ctx context.Context, dir, id, raw string, p vmfetch.Policy, verify func(vmfetch.Sums) error, progress func(done, total int64)) (vmfetch.Sums, error) {
	if free, err := diskFree(dir); err == nil && free < fetchMinFreeBytes {
		return vmfetch.Sums{}, fmt.Errorf("only %d MiB free in %s", free>>20, dir)
	}
	tmp, err := os.CreateTemp(dir, ".fetch-"+id+"-*")
	if err != nil {
		return vmfetch.Sums{}, err
	}
	tmpName := tmp.Name()
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return vmfetch.Sums{}, err
	}
	sums, err := fetchDownload(ctx, raw, p, tmp, vmImageMaxBytes, progress)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return vmfetch.Sums{}, err
	}
	if err := verify(sums); err != nil {
		return vmfetch.Sums{}, err
	}
	if _, err := checkImageFormat(ctx, tmpName); err != nil {
		return vmfetch.Sums{}, err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return vmfetch.Sums{}, err
	}
	imgPath, _ := cachePaths(dir, id)
	if err := os.Rename(tmpName, imgPath); err != nil {
		return vmfetch.Sums{}, err
	}
	keep = true
	return sums, nil
}

// progressLogger logs every tenth of a download, or every 256 MiB when the size is unknown.
func progressLogger(logLine LogFunc, id string, progress func(done, total int64)) func(done, total int64) {
	var next int64
	return func(done, total int64) {
		if progress != nil {
			progress(done, total)
		}
		step := int64(256 << 20)
		if total > 0 {
			step = total / 10
		}
		if done < next {
			return
		}
		next = done + step
		if total > 0 {
			logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, fmt.Sprintf("VM image %s: %d%% of %d MiB", id, done*100/total, total>>20))
		} else {
			logLine(agentv1.LogStream_LOG_STREAM_SYSTEM, fmt.Sprintf("VM image %s: %d MiB", id, done>>20))
		}
	}
}

func cleanLabel(label, id string) string {
	if l := strings.TrimSpace(sanitizeReportedID(label)); l != "" {
		return l
	}
	return id
}

func cleanFamily(f string) string {
	if reVMFamily.MatchString(f) {
		return f
	}
	return "other"
}

// FetchStatus reports a link fetch's progress to the control plane.
type FetchStatus func(state string, done, total int64, err error)

// FetchLinkImage downloads a user's image: their sha256 is checked before qemu-img reads it.
// The URL is used once and never written anywhere, the logs included.
func (o *ProxmoxLXC) FetchLinkImage(ctx context.Context, cmd *agentv1.FetchVmImage, status FetchStatus) error {
	id := cmd.GetImageId()
	want := strings.ToLower(strings.TrimSpace(cmd.GetSha256()))
	fail := func(err error) error {
		status("failed", 0, 0, err)
		return err
	}
	if !validVMImageID(id) {
		return fail(fmt.Errorf("invalid image id"))
	}
	if !reSHA256.MatchString(want) {
		return fail(fmt.Errorf("the sha256 must be 64 hexadecimal characters"))
	}
	u, err := vmfetch.CheckURL(cmd.GetUrl(), vmfetch.Open)
	if err != nil {
		return fail(err)
	}
	if why := foreignArch(vmHostArch, path.Base(u.Path), cmd.GetLabel(), id); why != "" {
		return fail(fmt.Errorf("this image is %s", why))
	}
	if exists, err := o.imageExists(ctx, id); err != nil || exists {
		if err == nil {
			err = fmt.Errorf("an image named %s is already on this machine; delete it first", id)
		}
		return fail(err)
	}
	dir, err := o.ensureCacheDir()
	if err != nil {
		return fail(err)
	}
	defer o.imageLocks.lock(id)()
	if _, err := readCacheMeta(dir, id); err == nil {
		return fail(fmt.Errorf("an image named %s is already on this machine; delete it first", id))
	}
	status("downloading", 0, 0, nil)
	sums, err := downloadInto(ctx, dir, id, u.String(), vmfetch.Open, func(s vmfetch.Sums) error {
		status("verifying", s.Bytes, s.Bytes, nil)
		if s.SHA256 != want {
			return fmt.Errorf("the download's sha256 is %s, not the one you gave", s.SHA256)
		}
		return nil
	}, func(done, total int64) { status("downloading", done, total, nil) })
	if err != nil {
		return fail(err)
	}
	meta := cacheMeta{Source: sourceLink, Label: cleanLabel(cmd.GetLabel(), id), Family: cleanFamily(cmd.GetFamily()),
		SHA256: sums.SHA256, SHA512: sums.SHA512, FetchedAt: cacheNow()}
	if err := writeCacheMeta(dir, id, meta); err != nil {
		imgPath, _ := cachePaths(dir, id)
		_ = os.Remove(imgPath)
		return fail(err)
	}
	status("done", sums.Bytes, sums.Bytes, nil)
	return nil
}

// FetchCatalogImage downloads a catalogue image ahead of the first VM that needs it, checked
// exactly as a deploy would; a copy still matching upstream just gets its 15 days back.
func (o *ProxmoxLXC) FetchCatalogImage(ctx context.Context, id string, src *agentv1.VmImageSource, status FetchStatus) error {
	if src.GetUrl() == "" {
		err := fmt.Errorf("no catalogue source for %s", id)
		status("failed", 0, 0, err)
		return err
	}
	status("downloading", 0, 0, nil)
	img, err := o.resolveVMImageWith(ctx, id, src, func(agentv1.LogStream, string) {},
		func(done, total int64) { status("downloading", done, total, nil) })
	if err != nil {
		status("failed", 0, 0, err)
		return err
	}
	size := int64(img.Info.GetSizeBytes())
	status("done", size, size, nil)
	return nil
}

// imageExists is true for a hand-placed file of that id, usable or not.
func (o *ProxmoxLXC) imageExists(ctx context.Context, id string) (bool, error) {
	local, _ := scanVMImages(ctx, o.vmImageDir())
	for _, img := range local {
		if img.Info.Id == id {
			return true, nil
		}
	}
	return false, nil
}

// RemoveVMImage deletes an image of any source and the template imported from it. A template
// still used by a VM's clone is fine to remove: clones are full copies.
func (o *ProxmoxLXC) RemoveVMImage(ctx context.Context, id string) error {
	if !validVMImageID(id) {
		return fmt.Errorf("invalid image id")
	}
	release, ok := o.imageLocks.tryLock(id)
	if !ok {
		return fmt.Errorf("VM image %s is being downloaded or imported; try again when it finishes", id)
	}
	defer release()
	removed := false
	localDir := o.vmImageDir()
	for _, ext := range vmImageExts {
		p := filepath.Join(localDir, id+ext)
		if info, err := os.Lstat(p); err == nil && info.Mode().IsRegular() {
			if err := os.Remove(p); err != nil {
				return err
			}
			_ = os.Remove(p + ".sha256")
			removed = true
		}
	}
	if removed {
		_ = os.Remove(filepath.Join(localDir, id+".json"))
	}
	cacheDir := o.vmCacheDir()
	imgPath, metaPath := cachePaths(cacheDir, id)
	if _, err := os.Lstat(metaPath); err == nil {
		_ = os.Remove(imgPath)
		if err := os.Remove(metaPath); err != nil {
			return err
		}
		removed = true
	}
	if !removed {
		return fmt.Errorf("VM image %s is not on this machine", id)
	}
	o.removeBaseFor(ctx, id)
	return nil
}

func (o *ProxmoxLXC) removeBaseFor(ctx context.Context, id string) {
	defer o.baseLocks.lock(id)()
	b, ok := o.vms.base(id)
	if !ok {
		return
	}
	if all, err := clusterGuests(ctx); err == nil {
		o.destroyBase(ctx, all, b.Vmid, func(agentv1.LogStream, string) {})
	}
	_ = o.vms.removeBase(id)
}
