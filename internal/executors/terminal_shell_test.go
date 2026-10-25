package executors

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The bootstrap runs in images we don't control: it must parse under a plain POSIX sh and
// leave PS1 showing the working directory rather than a bare "#".
func TestShellBootstrapParsesAndSetsAPathPrompt(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	if out, err := exec.Command("sh", "-n", "-c", shellBootstrap).CombinedOutput(); err != nil {
		t.Fatalf("sh -n: %v %s", err, out)
	}
	// Stop before the exec so the prompt it would hand over can be read back.
	probe := strings.Replace(shellBootstrap, "exec sh -i", `printf %s "$PS1"`, 1)
	probe = strings.Replace(probe, "exec bash -i", `printf %s "$PS1"; exit`, 1)
	run := func(path string) string {
		cmd := exec.Command("/bin/sh", "-c", probe)
		cmd.Env = []string{"PATH=" + path}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("bootstrap: %v %s", err, out)
		}
		return string(out)
	}
	if _, err := exec.LookPath("bash"); err == nil {
		if got := run(os.Getenv("PATH")); !strings.Contains(got, `\w`) {
			t.Fatalf("with bash, PS1 = %q", got)
		}
	}
	if got := run(noBashPath(t)); !strings.Contains(got, "@") || !strings.Contains(got, "${PWD}") {
		t.Fatalf("without bash, PS1 = %q", got)
	}
}

// A PATH with the tools the bootstrap uses but no bash.
func noBashPath(t *testing.T) string {
	dir := t.TempDir()
	for _, c := range []string{"readlink", "id", "printf", "hostname", "cat"} {
		if p, err := exec.LookPath(c); err == nil {
			_ = os.Symlink(p, filepath.Join(dir, c))
		}
	}
	return dir
}
