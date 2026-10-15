//go:build !linux

package linkbridge

import "syscall"

// Only Linux has IP_FREEBIND; elsewhere (development) the address must already exist.
func freebind(_, _ string, _ syscall.RawConn) error { return nil }
