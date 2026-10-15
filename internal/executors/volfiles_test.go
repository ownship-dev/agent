package executors

import (
	"strings"
	"testing"
)

func TestValidRelPath(t *testing.T) {
	valid := []string{"", "file.txt", "dir/sub", "dir/sub/file.tar.gz", "with space/ok", "unicode-ñé/f"}
	for _, p := range valid {
		if err := validRelPath(p); err != nil {
			t.Errorf("%q should be valid: %v", p, err)
		}
	}
	invalid := []string{
		"..",
		"../etc/passwd",
		"a/../b",
		"dir/..",
		"/abs",
		"a//b",
		".",
		"a/./b",
		"a|b",
		"a\nb",
		"a\x00b",
		strings.Repeat("a/", 600) + "f",
	}
	for _, p := range invalid {
		if err := validRelPath(p); err == nil {
			t.Errorf("%q should be rejected", p)
		}
	}
}

func TestParseVolumeList(t *testing.T) {
	out := "/data/config.yaml|1024|1700000000|regular file\n" +
		"/data/db|4096|1700000001|directory\n" +
		"garbage line without delimiters\n"
	entries := parseVolumeList(out)
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].GetName() != "config.yaml" || entries[0].GetSizeBytes() != 1024 || entries[0].GetIsDir() {
		t.Fatalf("bad file entry: %+v", entries[0])
	}
	if entries[1].GetName() != "db" || !entries[1].GetIsDir() {
		t.Fatalf("bad dir entry: %+v", entries[1])
	}
}
