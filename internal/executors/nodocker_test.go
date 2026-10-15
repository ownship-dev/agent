package executors

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every container CLI call goes through Engine, or Podman workloads silently hit Docker.
var literalDocker = regexp.MustCompile(`exec\.CommandContext\(ctx, "docker"|runStreaming(Env)?\([^)]*"docker"|"--", "docker"`)

func TestNoLiteralDockerExec(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "engine.go" {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			if literalDocker.MatchString(line) {
				t.Errorf("%s:%d calls docker directly; use Engine", f, i+1)
			}
		}
	}
}
