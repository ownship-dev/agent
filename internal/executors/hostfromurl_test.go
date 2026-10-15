package executors

import "testing"

func TestHostFromURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"http://10.0.0.7:8080", "10.0.0.7"},
		{"http://db.internal:5432", "db.internal"},
		{"10.0.0.7:8080", "10.0.0.7"},
		{"http://10.0.0.7", "10.0.0.7"},
		// A LastIndex(":") split returned "[fd00::1" here, which is not an address.
		{"http://[fd00::1]:8080", "fd00::1"},
		{"http://[fd00::1]", "fd00::1"},
	}
	for _, c := range cases {
		if got := hostFromURL(c.in); got != c.want {
			t.Errorf("hostFromURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
