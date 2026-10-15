package executors

import (
	"strings"
	"testing"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

func TestEnvFileContent(t *testing.T) {
	env := []*agentv1.EnvEntry{
		{Key: "DATABASE_URL", Value: "postgres://x"},
		{Key: "DEBUG", Value: "1"},
	}
	out := envFileContent(env, 8080)

	lines := strings.Split(strings.TrimSpace(out), "\n")
	if lines[0] != "PORT=8080" {
		t.Fatalf("expected PORT first, got %q", lines[0])
	}
	if !strings.Contains(out, "DATABASE_URL=postgres://x") {
		t.Fatalf("missing env var in output:\n%s", out)
	}
	if !strings.Contains(out, "DEBUG=1") {
		t.Fatalf("missing env var in output:\n%s", out)
	}
}
