// Package version compares dotted versions numerically, so "1.10" > "1.9".
package version

import (
	"strconv"
	"strings"
)

// Compare returns -1, 0 or 1; numeric segments compare numerically, others lexically.
func Compare(a, b string) int {
	as, bs := split(a), split(b)
	for i := 0; i < len(as) || i < len(bs); i++ {
		var av, bv string
		if i < len(as) {
			av = as[i]
		}
		if i < len(bs) {
			bv = bs[i]
		}
		if av == bv {
			continue
		}
		an, aErr := strconv.Atoi(av)
		bn, bErr := strconv.Atoi(bv)
		if aErr == nil && bErr == nil {
			if an != bn {
				if an < bn {
					return -1
				}
				return 1
			}
			continue
		}
		if av < bv {
			return -1
		}
		return 1
	}
	return 0
}

// Newer ignores a leading "v" on either side.
func Newer(a, b string) bool {
	return Compare(strings.TrimPrefix(a, "v"), strings.TrimPrefix(b, "v")) > 0
}

func split(v string) []string {
	return strings.FieldsFunc(v, func(r rune) bool {
		isDigit := r >= '0' && r <= '9'
		isAlpha := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		return !isDigit && !isAlpha
	})
}
