//go:build unix

package client

import "syscall"

// diskFreeBytes reports free space where Docker stores data (or /); 0 on failure.
func diskFreeBytes() uint64 {
	for _, path := range []string{"/var/lib/docker", "/"} {
		var st syscall.Statfs_t
		if err := syscall.Statfs(path, &st); err == nil {
			return uint64(st.Bavail) * uint64(st.Bsize) //nolint:unconvert // Bavail/Bsize types differ per OS
		}
	}
	return 0
}
