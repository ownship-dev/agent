package executors

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Cross-checks the Go and TS catalogues; skipped when the TS files aren't present.

const coreSrc = "../../../../packages/core/src/"

func tsQuotedIDs(t *testing.T, file, marker, terminator string) []string {
	t.Helper()
	raw, err := os.ReadFile(coreSrc + file)
	if err != nil {
		t.Skipf("TS source %s not readable (detached checkout?): %v", file, err)
	}
	body := string(raw)
	start := strings.Index(body, marker)
	if start < 0 {
		t.Fatalf("marker %q not found in %s", marker, file)
	}
	rest := body[start+len(marker):]
	if end := strings.Index(rest, terminator); end >= 0 {
		rest = rest[:end]
	}
	ids := regexp.MustCompile(`'([a-z0-9.\-]+)'`).FindAllStringSubmatch(rest, -1)
	out := make([]string, 0, len(ids))
	for _, m := range ids {
		out = append(out, m[1])
	}
	return out
}

func assertSameSet(t *testing.T, name string, goKeys, tsKeys []string) {
	t.Helper()
	sort.Strings(goKeys)
	sort.Strings(tsKeys)
	if strings.Join(goKeys, ",") != strings.Join(tsKeys, ",") {
		t.Errorf("%s drift: Go=%v TS=%v — keep the Go and TS catalogues in sync", name, goKeys, tsKeys)
	}
}

func TestBaseOSParityWithTS(t *testing.T) {
	goKeys := make([]string, 0, len(baseOSCatalogue))
	for id := range baseOSCatalogue {
		goKeys = append(goKeys, id)
	}
	assertSameSet(t, "base-os", goKeys, tsQuotedIDs(t, "base-os.ts", "BASE_OS_IDS = [", "]"))
}

func TestEngineParityWithTS(t *testing.T) {
	goKeys := make([]string, 0, len(engineBackups))
	for id := range engineBackups {
		goKeys = append(goKeys, id)
	}
	assertSameSet(t, "engine", goKeys, tsQuotedIDs(t, "services.ts", "type ServiceEngine =", ";"))
}
