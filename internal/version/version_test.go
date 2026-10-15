package version

import "testing"

func TestNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"1.2.3", "1.2.0", true},
		{"1.2.0", "1.2.3", false},
		{"1.2.0", "1.2.0", false},
		{"v2.0.0", "1.9.9", true},
		{"1.10.0", "1.9.0", true},
		// A numbered release always beats a legacy SHA, whatever its letters.
		{"1.768", "f8b2b7a", true},
		{"1.768", "a433c1c-dirty", true},
		{"f8b2b7a", "1.768", false},
		{"713fda6", "f8b2b7a", false},
		{"1.1000", "1.999", true},
	}
	for _, c := range cases {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestCompareNumericSegments(t *testing.T) {
	// The load-bearing case: 12.10-1 beats 12.7-1 numerically, not lexically.
	if Compare("12.10-1", "12.7-1") <= 0 {
		t.Errorf("Compare(12.10-1, 12.7-1) should be > 0")
	}
	if Compare("22.04-1", "22.04-1") != 0 {
		t.Errorf("Compare of equal versions should be 0")
	}
}
