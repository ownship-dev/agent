package executors

import (
	"testing"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

// resize must reject anything the uint16 narrowing would wrap. 65536 is the
// case that matters: it silently becomes 0 and blanks the caller's terminal.
func TestResizeRejectsOutOfRangeDims(t *testing.T) {
	cases := []struct {
		name       string
		rows, cols uint32
		want       bool
	}{
		{"typical", 24, 80, true},
		{"at the cap", maxPTYDim, maxPTYDim, true},
		{"zero rows", 0, 80, false},
		{"zero cols", 24, 0, false},
		{"over the cap", maxPTYDim + 1, 80, false},
		{"wraps to zero in uint16", 65536, 80, false},
		{"max uint32", ^uint32(0), ^uint32(0), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := &agentv1.TerminalInput{Rows: c.rows, Cols: c.cols}
			if got := ptyDimsOK(in); got != c.want {
				t.Errorf("ptyDimsOK(rows=%d, cols=%d) = %v, want %v", c.rows, c.cols, got, c.want)
			}
		})
	}
}
