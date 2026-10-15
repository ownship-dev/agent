package executors

import (
	"fmt"
	"os"
	"path/filepath"
)

// writeFileAtomic writes via a randomised temp file, fsync and rename, so a crash or power
// loss never leaves path truncated or empty.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("stage %s: %w", path, err)
	}
	staged := tmp.Name()
	// Remove the temp file on failure.
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(staged)
	}()

	if err := tmp.Chmod(perm); err != nil {
		return fmt.Errorf("chmod %s: %w", staged, err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", staged, err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", staged, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", staged, err)
	}
	if err := os.Rename(staged, path); err != nil {
		return fmt.Errorf("rename onto %s: %w", path, err)
	}
	return nil
}
