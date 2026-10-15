package executors

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestWriteFileAtomicSetsContentAndPerm(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env")
	if err := writeFileAtomic(path, []byte("SECRET=1\n"), 0o600); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "SECRET=1\n" {
		t.Fatalf("content = %q", got)
	}
	// Secrets land here, so the 0600 must survive the temp-file + rename dance —
	// CreateTemp makes the file 0600 already, but perm is the contract callers rely on.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("perm = %o, want 600", info.Mode().Perm())
	}
}

// The whole point of the randomised staging name: concurrent writers must not
// interleave into one temp file and produce a torn result.
func TestWriteFileAtomicConcurrentWritersNeverTear(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "env")
	a := []byte("AAAAAAAAAAAAAAAA\n")
	b := []byte("BBBB\n")

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			data := a
			if i%2 == 0 {
				data = b
			}
			if err := writeFileAtomic(path, data, 0o600); err != nil {
				t.Errorf("writeFileAtomic: %v", err)
			}
		}()
	}
	wg.Wait()

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(a) && string(got) != string(b) {
		t.Fatalf("torn write: %q", got)
	}
	// No staging files may survive a clean run.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("leftover staging files: %v", entries)
	}
}
