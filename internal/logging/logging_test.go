package logging

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestJSONMatchesTheSharedShape(t *testing.T) {
	var buf bytes.Buffer
	New(&buf, "json", "", "1.2.3").Error("pairing failed", "err", errors.New("bad code"), "machineId", "m1")
	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("not JSON: %v: %s", err, buf.String())
	}
	want := map[string]any{"level": "error", "msg": "pairing failed", "service": "agent", "version": "1.2.3", "err": "bad code", "machineId": "m1"}
	for k, v := range want {
		if line[k] != v {
			t.Errorf("%s = %v, want %v", k, line[k], v)
		}
	}
	if _, ok := line["time"].(string); !ok {
		t.Errorf("missing time: %s", buf.String())
	}
}

func TestTextIsTheDefault(t *testing.T) {
	var buf bytes.Buffer
	New(&buf, "", "", "1.2.3").Info("agent connected")
	if strings.HasPrefix(buf.String(), "{") || !strings.Contains(buf.String(), "msg=\"agent connected\"") {
		t.Fatalf("expected text output, got %s", buf.String())
	}
}

func TestLevelFilters(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, "json", "warn", "")
	log.Info("hidden")
	log.Warn("shown")
	if strings.Contains(buf.String(), "hidden") || !strings.Contains(buf.String(), "shown") {
		t.Fatalf("level not applied: %s", buf.String())
	}
}
