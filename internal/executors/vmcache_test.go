package executors

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
	"github.com/ownship/agent/internal/vmfetch"
)

const catalogURL = "https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-amd64.qcow2"

func debianSource() *agentv1.VmImageSource {
	return &agentv1.VmImageSource{
		Url:         catalogURL,
		ChecksumUrl: "https://cloud.debian.org/images/cloud/bookworm/latest/SHA512SUMS",
		Label:       "Debian 12",
		Family:      "debian",
	}
}

func sha512Hex(b []byte) string {
	s := sha512.Sum512(b)
	return hex.EncodeToString(s[:])
}

// fakeVendor serves a checksum file and an image body and counts downloads.
type fakeVendor struct {
	mu        sync.Mutex
	image     []byte
	checksums string
	downloads int
	urls      []string
}

func (v *fakeVendor) publish(image []byte) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.image = image
	v.checksums = sha512Hex(image) + "  debian-12-genericcloud-amd64.qcow2\n" + strings.Repeat("0", 128) + "  other.qcow2\n"
}

func installVendor(t *testing.T, v *fakeVendor) {
	t.Helper()
	small, dl := fetchSmall, fetchDownload
	t.Cleanup(func() { fetchSmall, fetchDownload = small, dl })
	fetchSmall = func(_ context.Context, raw string, _ int64) ([]byte, error) {
		v.mu.Lock()
		defer v.mu.Unlock()
		v.urls = append(v.urls, raw)
		return []byte(v.checksums), nil
	}
	fetchDownload = func(_ context.Context, raw string, _ vmfetch.Policy, w io.Writer, _ int64, progress func(int64, int64)) (vmfetch.Sums, error) {
		v.mu.Lock()
		body := append([]byte{}, v.image...)
		v.downloads++
		v.urls = append(v.urls, raw)
		v.mu.Unlock()
		if _, err := w.Write(body); err != nil {
			return vmfetch.Sums{}, err
		}
		if progress != nil {
			progress(int64(len(body)), int64(len(body)))
		}
		s256 := sha256.Sum256(body)
		return vmfetch.Sums{SHA256: hex.EncodeToString(s256[:]), SHA512: sha512Hex(body), Bytes: int64(len(body))}, nil
	}
}

func pinNow(t *testing.T, at time.Time) *time.Time {
	t.Helper()
	orig := cacheNow
	now := at
	cacheNow = func() time.Time { return now }
	t.Cleanup(func() { cacheNow = orig })
	return &now
}

func cacheProxmox(t *testing.T) *ProxmoxLXC {
	t.Helper()
	o := newTestProxmox(t)
	imageDir(t) // the test user owns the temp dirs, so trust them
	fakeQemuImg(t, nil)
	return o
}

func TestCatalogImageIsFetchedVerifiedAndCached(t *testing.T) {
	o := cacheProxmox(t)
	v := &fakeVendor{}
	v.publish([]byte("debian 12 image"))
	installVendor(t, v)
	pinNow(t, time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC))

	img, err := o.resolveVMImage(context.Background(), "debian-12", debianSource(), noLog)
	if err != nil {
		t.Fatal(err)
	}
	if !img.Info.GetUsable() || img.Info.GetSource() != "catalog" || img.Info.GetLabel() != "Debian 12" || img.Info.GetFamily() != "debian" {
		t.Fatalf("image = %+v", img.Info)
	}
	if got := time.Unix(img.Info.GetExpiresAt(), 0).UTC(); got != time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC) {
		t.Fatalf("expires %v, want 15 days later", got)
	}
	if _, err := o.resolveVMImage(context.Background(), "debian-12", debianSource(), noLog); err != nil || v.downloads != 1 {
		t.Fatalf("a fresh copy is reused: downloads=%d err=%v", v.downloads, err)
	}
	// Reported with the rest, and without a source the cached copy still serves.
	if _, err := o.resolveVMImage(context.Background(), "debian-12", nil, noLog); err != nil {
		t.Fatal(err)
	}
	r := o.VMReport(context.Background())
	if len(r.Images) != 1 || r.Images[0].GetSource() != "catalog" {
		t.Fatalf("report = %v", r.Images)
	}
}

func TestCatalogImageThatDoesNotMatchIsRefusedAndRemoved(t *testing.T) {
	o := cacheProxmox(t)
	v := &fakeVendor{}
	v.publish([]byte("good"))
	installVendor(t, v)
	v.image = []byte("tampered on the wire")

	if _, err := o.resolveVMImage(context.Background(), "debian-12", debianSource(), noLog); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("err = %v", err)
	}
	entries, _ := os.ReadDir(o.vmCacheDir())
	if len(entries) != 0 {
		t.Fatalf("a refused download left files behind: %v", entries)
	}
}

func TestBadFormatIsRefusedAfterTheHash(t *testing.T) {
	o := cacheProxmox(t)
	v := &fakeVendor{}
	v.publish([]byte("img"))
	installVendor(t, v)
	orig := qemuImgInfo
	qemuImgInfo = func(context.Context, string) ([]byte, error) {
		return []byte(`{"format":"qcow2","backing-filename":"/etc/shadow"}`), nil
	}
	t.Cleanup(func() { qemuImgInfo = orig })
	if _, err := o.resolveVMImage(context.Background(), "debian-12", debianSource(), noLog); err == nil || !strings.Contains(err.Error(), "backing file") {
		t.Fatalf("err = %v", err)
	}
	if entries, _ := os.ReadDir(o.vmCacheDir()); len(entries) != 0 {
		t.Fatalf("left behind: %v", entries)
	}
}

func TestExpiredCopyIsRenewedOrRefetched(t *testing.T) {
	o := cacheProxmox(t)
	v := &fakeVendor{}
	v.publish([]byte("v1"))
	installVendor(t, v)
	now := pinNow(t, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	if _, err := o.resolveVMImage(context.Background(), "debian-12", debianSource(), noLog); err != nil {
		t.Fatal(err)
	}

	*now = now.Add(16 * 24 * time.Hour)
	// The report drops it at once; nothing is downloaded until a deploy needs it.
	if r := o.VMReport(context.Background()); len(r.Images) != 0 {
		t.Fatalf("an expired copy is not reported: %v", r.Images)
	}
	if _, err := o.resolveVMImage(context.Background(), "debian-12", nil, noLog); err == nil {
		t.Fatal("expired, with no source to ask, must fail")
	}

	v.publish([]byte("v1"))
	_, err := o.resolveVMImage(context.Background(), "debian-12", debianSource(), noLog)
	if err != nil || v.downloads != 2 {
		t.Fatalf("swept copy is downloaded again: downloads=%d err=%v", v.downloads, err)
	}

	v.publish([]byte("v2"))
	*now = now.Add(16 * 24 * time.Hour)
	img, err := o.resolveVMImage(context.Background(), "debian-12", debianSource(), noLog)
	if err != nil || img.Info.GetSha256() != sumOf("v2") {
		t.Fatalf("changed upstream fetches the new image: %+v %v", img.Info, err)
	}
}

func TestRenewWithoutDownloadWhenUnchanged(t *testing.T) {
	o := cacheProxmox(t)
	v := &fakeVendor{}
	v.publish([]byte("same"))
	installVendor(t, v)
	now := pinNow(t, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	if _, err := o.resolveVMImage(context.Background(), "debian-12", debianSource(), noLog); err != nil {
		t.Fatal(err)
	}
	// Past expiry but locked out of the sweep, as during a deploy of the same image.
	*now = now.Add(15*24*time.Hour + time.Minute)
	dir := o.vmCacheDir()
	m, err := readCacheMeta(dir, "debian-12")
	if err != nil {
		t.Fatal(err)
	}
	img, err := o.fetchCatalog(context.Background(), dir, "debian-12", debianSource(), &m, noLog, nil)
	if err != nil || v.downloads != 1 {
		t.Fatalf("renew: downloads=%d err=%v", v.downloads, err)
	}
	if time.Unix(img.Info.GetExpiresAt(), 0).Before(now.Add(14 * 24 * time.Hour)) {
		t.Fatal("expiry was not pushed out")
	}
}

func TestHandPlacedImageWinsOverTheCatalogue(t *testing.T) {
	o := cacheProxmox(t)
	dir := imageDir(t)
	put(t, dir, "debian-12.qcow2", "mine", 0o644)
	o.cfg.VMImageDir = dir
	v := &fakeVendor{}
	v.publish([]byte("vendor"))
	installVendor(t, v)
	img, err := o.resolveVMImage(context.Background(), "debian-12", debianSource(), noLog)
	if err != nil || img.Info.GetSha256() != sumOf("mine") || v.downloads != 0 {
		t.Fatalf("the owner's file must win: %+v %v downloads=%d", img.Info, err, v.downloads)
	}
}

func TestCatalogueHostOutsideTheAllowlistIsRefused(t *testing.T) {
	o := cacheProxmox(t)
	v := &fakeVendor{}
	v.publish([]byte("x"))
	installVendor(t, v)
	src := debianSource()
	src.Url = "https://evil.example/debian-12-genericcloud-amd64.qcow2"
	if _, err := o.resolveVMImage(context.Background(), "debian-12", src, noLog); err == nil || !strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("err = %v", err)
	}
	if len(v.urls) != 0 {
		t.Fatalf("nothing may be fetched: %v", v.urls)
	}
}

func linkCommand(body []byte) *agentv1.FetchVmImage {
	s := sha256.Sum256(body)
	return &agentv1.FetchVmImage{ImageId: "rhel-9", Label: "RHEL 9", Family: "rhel",
		Url: "https://access.cdn.redhat.com/rhel-9.qcow2?_auth_=TOKEN", Sha256: hex.EncodeToString(s[:])}
}

type statusLog struct{ states []string }

func (s *statusLog) fn(state string, _, _ int64, err error) {
	if err != nil {
		state += ": " + err.Error()
	}
	s.states = append(s.states, state)
}

func TestLinkImageChecksTheHashBeforeQemuImg(t *testing.T) {
	o := cacheProxmox(t)
	v := &fakeVendor{image: []byte("not what you said")}
	installVendor(t, v)
	var qemuCalls int
	orig := qemuImgInfo
	qemuImgInfo = func(context.Context, string) ([]byte, error) {
		qemuCalls++
		return []byte(`{"format":"qcow2"}`), nil
	}
	t.Cleanup(func() { qemuImgInfo = orig })

	var log statusLog
	err := o.FetchLinkImage(context.Background(), linkCommand([]byte("the real image")), log.fn)
	if err == nil || qemuCalls != 0 {
		t.Fatalf("a wrong hash must stop before qemu-img: err=%v qemu=%d", err, qemuCalls)
	}
	if last := log.states[len(log.states)-1]; !strings.HasPrefix(last, "failed") || strings.Contains(last, "TOKEN") {
		t.Fatalf("status = %v", log.states)
	}
}

func TestLinkImageIsKeptWithoutItsURL(t *testing.T) {
	o := cacheProxmox(t)
	body := []byte("the real image")
	v := &fakeVendor{image: body}
	installVendor(t, v)
	var log statusLog
	if err := o.FetchLinkImage(context.Background(), linkCommand(body), log.fn); err != nil {
		t.Fatal(err)
	}
	if log.states[len(log.states)-1] != "done" {
		t.Fatalf("status = %v", log.states)
	}
	meta, err := os.ReadFile(filepath.Join(o.vmCacheDir(), "rhel-9.meta.json"))
	if err != nil || bytes.Contains(meta, []byte("TOKEN")) || bytes.Contains(meta, []byte("redhat")) {
		t.Fatalf("metadata must not keep the link: %s %v", meta, err)
	}
	pinNow(t, time.Now().Add(365*24*time.Hour))
	r := o.VMReport(context.Background())
	if len(r.Images) != 1 || r.Images[0].GetSource() != "link" || r.Images[0].GetExpiresAt() != 0 || r.Images[0].GetFamily() != "rhel" {
		t.Fatalf("a link image never expires: %v", r.Images)
	}
	if err := o.FetchLinkImage(context.Background(), linkCommand(body), (&statusLog{}).fn); err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("a second fetch under the same id must be refused: %v", err)
	}
}

func TestLinkImageRefusesBadInput(t *testing.T) {
	o := cacheProxmox(t)
	installVendor(t, &fakeVendor{})
	for name, mut := range map[string]func(*agentv1.FetchVmImage){
		"id":     func(c *agentv1.FetchVmImage) { c.ImageId = "../x" },
		"sha":    func(c *agentv1.FetchVmImage) { c.Sha256 = "abc" },
		"http":   func(c *agentv1.FetchVmImage) { c.Url = "http://access.cdn.redhat.com/x.qcow2" },
		"port":   func(c *agentv1.FetchVmImage) { c.Url = "https://example.com:8443/x.qcow2" },
		"userpw": func(c *agentv1.FetchVmImage) { c.Url = "https://u:p@example.com/x.qcow2" },
	} {
		cmd := linkCommand([]byte("x"))
		mut(cmd)
		if err := o.FetchLinkImage(context.Background(), cmd, (&statusLog{}).fn); err == nil {
			t.Errorf("%s: expected a refusal", name)
		}
	}
}

func TestRemoveVMImageDeletesFilesAndTemplate(t *testing.T) {
	o := cacheProxmox(t)
	dir := imageDir(t)
	o.cfg.VMImageDir = dir
	put(t, dir, "debian-12.qcow2", "mine", 0o644)
	put(t, dir, "debian-12.qcow2.sha256", sumOf("mine")+"  debian-12.qcow2\n", 0o644)
	put(t, dir, "debian-12.json", `{"label":"Debian"}`, 0o644)
	if err := o.vms.setBase("debian-12", vmBase{Vmid: "9000", Sha256: sumOf("mine")}); err != nil {
		t.Fatal(err)
	}
	fakeClusterGuests(t, `[{"vmid":9000,"type":"qemu","tags":"ownship-base"}]`, nil)
	calls := fakeQM(t, func([]string) ([]byte, error) { return nil, nil })

	if err := o.RemoveVMImage(context.Background(), "debian-12"); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"debian-12.qcow2", "debian-12.qcow2.sha256", "debian-12.json"} {
		if _, err := os.Lstat(filepath.Join(dir, f)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still there", f)
		}
	}
	if _, ok := o.vms.base("debian-12"); ok {
		t.Fatal("template mapping kept")
	}
	if len(*calls) != 1 || (*calls)[0][0] != "destroy" || (*calls)[0][1] != "9000" {
		t.Fatalf("qm calls = %v", *calls)
	}
	if err := o.RemoveVMImage(context.Background(), "debian-12"); err == nil {
		t.Fatal("a second remove has nothing to delete")
	}
	if err := o.RemoveVMImage(context.Background(), "../../etc"); err == nil {
		t.Fatal("hostile id")
	}
}

func TestRemoveWaitsForNoFetch(t *testing.T) {
	o := cacheProxmox(t)
	release := o.imageLocks.lock("debian-12")
	defer release()
	if err := o.RemoveVMImage(context.Background(), "debian-12"); err == nil || !strings.Contains(err.Error(), "being downloaded") {
		t.Fatalf("err = %v", err)
	}
}

func TestSweepSkipsLinkImagesAndLockedCopies(t *testing.T) {
	o := cacheProxmox(t)
	v := &fakeVendor{}
	v.publish([]byte("catalog"))
	installVendor(t, v)
	now := pinNow(t, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if _, err := o.resolveVMImage(context.Background(), "debian-12", debianSource(), noLog); err != nil {
		t.Fatal(err)
	}
	v.image = []byte("rhel")
	if err := o.FetchLinkImage(context.Background(), linkCommand([]byte("rhel")), (&statusLog{}).fn); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(20 * 24 * time.Hour)
	release := o.imageLocks.lock("debian-12")
	if o.SweepVMImages() {
		t.Fatal("a copy in use must not be swept")
	}
	release()
	if !o.SweepVMImages() {
		t.Fatal("the expired copy is swept")
	}
	if _, err := os.Stat(filepath.Join(o.vmCacheDir(), "debian-12.img")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("image file kept")
	}
	if _, err := os.Stat(filepath.Join(o.vmCacheDir(), "rhel-9.img")); err != nil {
		t.Fatal("a link image is never swept")
	}
}

func TestCatalogImageIsFetchedAheadOfAVM(t *testing.T) {
	o := cacheProxmox(t)
	v := &fakeVendor{}
	v.publish([]byte("debian 12 image"))
	installVendor(t, v)
	var states []string
	status := func(state string, done, total int64, err error) { states = append(states, state) }

	if err := o.FetchCatalogImage(context.Background(), "debian-12", debianSource(), status); err != nil {
		t.Fatal(err)
	}
	if states[len(states)-1] != "done" || v.downloads != 1 {
		t.Fatalf("states=%v downloads=%d", states, v.downloads)
	}
	if r := o.VMReport(context.Background()); len(r.Images) != 1 || r.Images[0].GetSource() != "catalog" {
		t.Fatalf("report = %v", r.Images)
	}
	// The deploy that follows reuses the copy.
	if _, err := o.resolveVMImage(context.Background(), "debian-12", debianSource(), noLog); err != nil || v.downloads != 1 {
		t.Fatalf("downloads=%d err=%v", v.downloads, err)
	}

	v.publish([]byte("tampered"))
	v.checksums = strings.Repeat("0", 128) + "  debian-12-genericcloud-amd64.qcow2\n"
	states = nil
	if err := o.FetchCatalogImage(context.Background(), "debian-13", debianSource(), status); err == nil || states[len(states)-1] != "failed" {
		t.Fatalf("a mismatch fails: states=%v err=%v", states, err)
	}
}
