package executors

import (
	"slices"
	"testing"
)

func TestDockerUserArgs(t *testing.T) {
	cases := []struct {
		name      string
		imageUser string
		want      []string
	}{
		{"empty (image defaults to root)", "", []string{"-u", nonRootUser}},
		{"explicit root name", "root", []string{"-u", nonRootUser}},
		{"case-insensitive root name", "Root", []string{"-u", nonRootUser}},
		{"explicit root uid", "0", []string{"-u", nonRootUser}},
		{"root uid with group", "0:0", []string{"-u", nonRootUser}},
		{"named non-root user", "app", nil},
		{"numeric non-root uid", "1001", nil},
		{"numeric non-root uid with group", "999:999", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := dockerUserArgs(tc.imageUser)
			if !slices.Equal(got, tc.want) {
				t.Errorf("dockerUserArgs(%q) = %v, want %v", tc.imageUser, got, tc.want)
			}
		})
	}
}
