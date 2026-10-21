// Package vmfetch downloads VM cloud images and proves them against their vendor's checksum
// file. It knows nothing about Proxmox; the executors decide where an image goes.
package vmfetch

import "strings"

// Hosts a catalogue image or its checksum may come from. Must equal VM_IMAGE_HOSTS in
// @ownship/core (vm-image-hosts.test.ts reads this file).
var Hosts = []string{
	"cloud-images.ubuntu.com",
	"cloud.debian.org",
	"repo.almalinux.org",
	"dl.rockylinux.org",
	"cloud.centos.org",
	"dl.fedoraproject.org",
	"download.fedoraproject.org",
}

// AllowedHost reports whether h is on the catalogue allowlist.
func AllowedHost(h string) bool {
	h = strings.ToLower(h)
	for _, a := range Hosts {
		if h == a {
			return true
		}
	}
	return false
}
