//go:build !unix

package client

func diskFreeBytes() uint64 { return 0 }
