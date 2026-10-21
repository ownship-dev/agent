package vmfetch

import (
	"bufio"
	"bytes"
	"fmt"
	"path"
	"regexp"
	"strings"
)

// Expected is the digest a checksum file gives for one image.
type Expected struct {
	Algo string // "sha256" or "sha512"
	Hex  string
}

var (
	// GNU coreutils: "<hex>  name" or "<hex> *name".
	reGNU = regexp.MustCompile(`^([0-9a-fA-F]{64}|[0-9a-fA-F]{128})\s+\*?(\S+)$`)
	// BSD tagged: "SHA256 (name) = <hex>", as Fedora, Rocky, AlmaLinux and CentOS publish.
	reBSD = regexp.MustCompile(`^(SHA256|SHA512)\s*\((.+)\)\s*=\s*([0-9a-fA-F]+)$`)
)

// FindExpected returns the digest the checksum file lists for name (the image URL's last path
// element). A file listing it twice with different digests is refused.
func FindExpected(file []byte, name string) (Expected, error) {
	var found *Expected
	sc := bufio.NewScanner(bytes.NewReader(file))
	sc.Buffer(make([]byte, 64<<10), 64<<10)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		var algo, listed, sum string
		if m := reBSD.FindStringSubmatch(line); m != nil {
			algo, listed, sum = strings.ToLower(m[1]), m[2], m[3]
			if (algo == "sha256" && len(sum) != 64) || (algo == "sha512" && len(sum) != 128) {
				continue
			}
		} else if m := reGNU.FindStringSubmatch(line); m != nil {
			sum, listed = m[1], m[2]
			algo = "sha256"
			if len(sum) == 128 {
				algo = "sha512"
			}
		} else {
			continue
		}
		if path.Base(strings.TrimPrefix(listed, "./")) != name {
			continue
		}
		e := Expected{Algo: algo, Hex: strings.ToLower(sum)}
		if found != nil && *found != e {
			return Expected{}, fmt.Errorf("the checksum file lists %s twice with different digests", name)
		}
		found = &e
	}
	if err := sc.Err(); err != nil {
		return Expected{}, fmt.Errorf("unreadable checksum file: %v", err)
	}
	if found == nil {
		return Expected{}, fmt.Errorf("the checksum file has no line for %s", name)
	}
	return *found, nil
}

// Matches reports whether the download's sums satisfy e.
func (e Expected) Matches(s Sums) bool {
	if e.Algo == "sha512" {
		return s.SHA512 == e.Hex
	}
	return s.SHA256 == e.Hex
}
