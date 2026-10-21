package executors

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeQemuImg(t *testing.T, byName map[string]string) {
	t.Helper()
	orig := qemuImgInfo
	qemuImgInfo = func(_ context.Context, path string) ([]byte, error) {
		if out, ok := byName[filepath.Base(path)]; ok {
			return []byte(out), nil
		}
		return []byte(`{"format":"qcow2","virtual-size":2147483648}`), nil
	}
	t.Cleanup(func() { qemuImgInfo = orig })
}

func imageDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "vm-images")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	orig := vmImageOwnerUID
	vmImageOwnerUID = uint32(os.Getuid())
	t.Cleanup(func() { vmImageOwnerUID = orig })
	return dir
}

func put(t *testing.T, dir, name, body string, mode os.FileMode) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func sumOf(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func scanByID(t *testing.T, dir string) map[string]vmImageFile {
	t.Helper()
	images, err := scanVMImages(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]vmImageFile{}
	for _, img := range images {
		out[img.Info.Id] = img
	}
	return out
}

func TestScanVMImagesAcceptsGoodQcow2AndRaw(t *testing.T) {
	dir := imageDir(t)
	fakeQemuImg(t, map[string]string{"alpine.raw": `{"format":"raw"}`})
	put(t, dir, "debian-12.qcow2", "qcow", 0o644)
	put(t, dir, "debian-12.json", `{"label":"Debian 12","family":"debian"}`, 0o644)
	put(t, dir, "debian-12.qcow2.sha256", sumOf("qcow")+"  debian-12.qcow2\n", 0o644)
	put(t, dir, "alpine.raw", "raw", 0o600)
	put(t, dir, "notes.txt", "ignored", 0o644)

	got := scanByID(t, dir)
	if len(got) != 2 {
		t.Fatalf("got %d images: %v", len(got), got)
	}
	d := got["debian-12"]
	if !d.Info.Usable || d.Info.Label != "Debian 12" || d.Info.Family != "debian" || d.Info.Sha256 != sumOf("qcow") || d.Format != "qcow2" {
		t.Fatalf("debian %+v", d.Info)
	}
	a := got["alpine"]
	if !a.Info.Usable || a.Info.Label != "alpine" || a.Info.Family != "other" || a.Format != "raw" || a.Info.SizeBytes != 3 {
		t.Fatalf("alpine %+v", a.Info)
	}
}

func TestScanVMImagesRejectsEachUnsafeFile(t *testing.T) {
	dir := imageDir(t)
	fakeQemuImg(t, map[string]string{
		"backed.qcow2":   `{"format":"qcow2","backing-filename":"/etc/shadow"}`,
		"datafile.qcow2": `{"format":"qcow2","format-specific":{"type":"qcow2","data":{"data-file":"/dev/sda"}}}`,
		"vmdk.img":       `{"format":"vmdk"}`,
		"junk.img":       `garbage`,
	})
	put(t, dir, "backed.qcow2", "x", 0o644)
	put(t, dir, "datafile.qcow2", "x", 0o644)
	put(t, dir, "vmdk.img", "x", 0o644)
	put(t, dir, "junk.img", "x", 0o644)
	put(t, dir, "writable.qcow2", "x", 0o666)
	put(t, dir, "groupw.qcow2", "x", 0o664)
	put(t, dir, "Bad Name.qcow2", "x", 0o644)
	put(t, dir, "badsum.qcow2", "x", 0o644)
	put(t, dir, "badsum.qcow2.sha256", strings.Repeat("0", 64)+"  badsum.qcow2\n", 0o644)
	put(t, dir, "notsum.qcow2", "x", 0o644)
	put(t, dir, "notsum.qcow2.sha256", "hello\n", 0o644)
	put(t, dir, "wsum.qcow2", "x", 0o644)
	put(t, dir, "wsum.qcow2.sha256", sumOf("x")+"  wsum.qcow2\n", 0o666)
	put(t, dir, "dup.qcow2", "x", 0o644)
	put(t, dir, "dup.raw", "x", 0o644)
	put(t, dir, "badmeta.qcow2", "x", 0o644)
	put(t, dir, "badmeta.json", "{", 0o644)
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "link.qcow2")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "adir.qcow2"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := scanByID(t, dir)
	want := map[string]string{
		"backed":   "backing file",
		"datafile": "data file",
		"vmdk":     "not qcow2 or raw",
		"junk":     "unparseable",
		"writable": "writable by group or others",
		"groupw":   "writable by group or others",
		"Bad Name": "file name must be",
		"badsum":   "does not match",
		"notsum":   "sha256sum format",
		"wsum":     "writable by group or others",
		"dup":      "share this image id",
		"badmeta":  "not valid JSON",
		"link":     "symlink",
		"adir":     "not a regular file",
	}
	for id, reason := range want {
		img, ok := got[id]
		if !ok {
			t.Errorf("%s: not listed (unusable images must be reported, not skipped)", id)
			continue
		}
		if img.Info.Usable || !strings.Contains(img.Info.Reason, reason) {
			t.Errorf("%s: usable=%v reason %q, want %q", id, img.Info.Usable, img.Info.Reason, reason)
		}
	}
}

func TestScanVMImagesRefusesFilesNotOwnedByRoot(t *testing.T) {
	dir := imageDir(t)
	fakeQemuImg(t, nil)
	put(t, dir, "debian-12.qcow2", "x", 0o644)
	vmImageOwnerUID = uint32(os.Getuid()) + 1
	if _, err := scanVMImages(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "not owned by root") {
		t.Fatalf("directory owner: %v", err)
	}
}

func TestScanVMImagesRefusesAnUnsafeDirectory(t *testing.T) {
	dir := imageDir(t)
	_ = os.Chmod(dir, 0o777)
	if _, err := scanVMImages(context.Background(), dir); err == nil {
		t.Fatal("a world-writable image directory must not be trusted")
	}
	link := dir + "-link"
	_ = os.Chmod(dir, 0o755)
	_ = os.Symlink(dir, link)
	if _, err := scanVMImages(context.Background(), link); err == nil {
		t.Fatal("a symlinked image directory must not be trusted")
	}
	if _, err := scanVMImages(context.Background(), dir+"-missing"); err == nil {
		t.Fatal("missing directory")
	}
}

func TestFindVMImageRevalidatesAtDeployTime(t *testing.T) {
	dir := imageDir(t)
	fakeQemuImg(t, nil)
	put(t, dir, "debian-12.qcow2", "v1", 0o644)
	put(t, dir, "debian-12.qcow2.sha256", sumOf("v1")+"  debian-12.qcow2\n", 0o644)
	ctx := context.Background()
	if img, err := findVMImage(ctx, dir, "debian-12"); err != nil || img.Info.Sha256 != sumOf("v1") {
		t.Fatalf("%+v %v", img, err)
	}
	put(t, dir, "debian-12.qcow2", "v2-tampered", 0o644)
	if _, err := findVMImage(ctx, dir, "debian-12"); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("a changed file must fail: %v", err)
	}
	_ = os.Remove(filepath.Join(dir, "debian-12.qcow2"))
	if _, err := findVMImage(ctx, dir, "debian-12"); err == nil || !strings.Contains(err.Error(), "is not in") {
		t.Fatalf("a removed file must fail: %v", err)
	}
	if _, err := findVMImage(ctx, dir, "../etc/passwd"); err == nil {
		t.Fatal("a hostile id must fail")
	}
}
