package executors

import (
	"strconv"
	"strings"
)

type StatsSample struct {
	CPUPct        float64
	MemBytes      uint64
	MemLimitBytes uint64
	NetRxBytes    uint64
	NetTxBytes    uint64
	// Cumulative; the control plane diffs it over a window.
	RestartCount uint64
}

// parseDockerStats reads e.g. "12.34%;50MiB / 2GiB;1.2kB / 3.4kB".
func parseDockerStats(line string) StatsSample {
	parts := strings.Split(line, ";")
	var s StatsSample
	if len(parts) >= 1 {
		s.CPUPct = parsePercent(parts[0])
	}
	if len(parts) >= 2 {
		s.MemBytes, s.MemLimitBytes = parsePair(parts[1])
	}
	if len(parts) >= 3 {
		s.NetRxBytes, s.NetTxBytes = parsePair(parts[2])
	}
	return s
}

func parsePercent(s string) float64 {
	v, _ := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(s), "%"), 64)
	return v
}

func parsePair(s string) (uint64, uint64) {
	l, r, ok := strings.Cut(s, "/")
	if !ok {
		return parseBytes(s), 0
	}
	return parseBytes(l), parseBytes(r)
}

var byteUnits = map[string]float64{
	"B": 1, "kB": 1e3, "KB": 1e3, "KiB": 1 << 10,
	"MB": 1e6, "MiB": 1 << 20, "GB": 1e9, "GiB": 1 << 30,
	"TB": 1e12, "TiB": 1 << 40,
}

func parseUint(s string) uint64 {
	v, _ := strconv.ParseUint(s, 10, 64)
	return v
}

// parseBytes handles docker's mix of binary and SI units.
func parseBytes(s string) uint64 {
	s = strings.TrimSpace(s)
	i := 0
	for i < len(s) && (s[i] == '.' || (s[i] >= '0' && s[i] <= '9')) {
		i++
	}
	num, _ := strconv.ParseFloat(s[:i], 64)
	mult, ok := byteUnits[strings.TrimSpace(s[i:])]
	if !ok {
		mult = 1
	}
	return uint64(num * mult)
}
