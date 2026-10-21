package executors

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

// fakeSSHScript stands in for ssh: it joins the remote words like ssh does, parses them with a
// real shell, and records the argv that shell would run plus stdin. Docker starts uninstalled.
const fakeSSHScript = `#!/bin/sh
while [ "$1" != "--" ]; do shift; done
shift 2
[ "$1 $2 $3" = "sudo -n --" ] || { echo "missing sudo -n --" >&2; exit 97; }
shift 3
printf '%s\0' "$@" >> "$FAKE_SSH_LOG.raw"
printf '\036' >> "$FAKE_SSH_LOG.raw"
eval "set -- $*"
for w in "$@"; do printf '%s\0' "$w"; done >> "$FAKE_SSH_LOG"
printf '\035' >> "$FAKE_SSH_LOG"
cat >> "$FAKE_SSH_LOG"
printf '\036' >> "$FAKE_SSH_LOG"
case "$*" in
  "sh -c command -v docker >/dev/null 2>&1")
    [ -e "$FAKE_SSH_LOG.docker" ] && exit 0
    exit 1 ;;
  "sh -c "*"get.docker.com"*) : > "$FAKE_SSH_LOG.docker" ;;
esac
exit 0
`

type sshCall struct {
	argv  []string
	stdin string
}

func fakeSSH(t *testing.T) (log string, read func() []sshCall, raw func() [][]string) {
	t.Helper()
	bin, err := os.MkdirTemp("", "fake-ssh-*")
	if err != nil {
		t.Fatal(err)
	}
	// Background prunes may still write here after the test; never fail on cleanup.
	t.Cleanup(func() { _ = os.RemoveAll(bin) })
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(fakeSSHScript), 0o755); err != nil {
		t.Fatal(err)
	}
	log = filepath.Join(bin, "calls")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_SSH_LOG", log)
	read = func() []sshCall {
		data, _ := os.ReadFile(log)
		var calls []sshCall
		for _, rec := range strings.Split(string(data), "\036") {
			if rec == "" {
				continue
			}
			args, stdin, _ := strings.Cut(rec, "\035")
			calls = append(calls, sshCall{argv: strings.Split(strings.TrimSuffix(args, "\x00"), "\x00"), stdin: stdin})
		}
		return calls
	}
	raw = func() [][]string {
		data, _ := os.ReadFile(log + ".raw")
		var out [][]string
		for _, rec := range strings.Split(string(data), "\036") {
			if rec != "" {
				out = append(out, strings.Split(strings.TrimSuffix(rec, "\x00"), "\x00"))
			}
		}
		return out
	}
	return log, read, raw
}

func findCall(calls []sshCall, prefix ...string) (sshCall, bool) {
	for _, c := range calls {
		if len(c.argv) >= len(prefix) && slices.Equal(c.argv[:len(prefix)], prefix) {
			return c, true
		}
	}
	return sshCall{}, false
}

// One VM workload through its whole life, with every Proxmox and SSH call faked. The env value
// is hostile on purpose: it must reach the guest byte for byte and never run.
func TestVMWorkloadEndToEnd(t *testing.T) {
	fastVMWaits(t)
	o, _ := testVMGuest(t)
	o.engines = loadEngineState(filepath.Join(t.TempDir(), "engines.json"), proxmoxEngines(), nil)
	vmCapableHost(t, o)
	vmImageInDir(t, o)
	fakeNextVmid(t, "9000", "201")
	fakeClusterGuests(t, `[{"vmid":201,"type":"qemu","tags":"ownship"}]`, nil)
	qm := vmQM(t, testHostKey(t), func(args []string) ([]byte, error, bool) {
		if args[0] == "importdisk" {
			out, err := importReply(args)
			return out, err, true
		}
		return nil, nil, false
	})
	fakeSSHProbe(t, func([]string) error { return nil })
	_, calls, raw := fakeSSH(t)
	ctx := context.Background()

	hostile := `x'; touch /tmp/pwned; echo "$(id)" \` + "`id`"
	spec := vmSpec(func(s *RunSpec) {
		s.Image = "nginx:1.27"
		s.Port = 8080
		s.Engine = KindDocker
		s.Env = []*agentv1.EnvEntry{{Key: "SECRET", Value: hostile}}
		s.Volumes = []*agentv1.VolumeMount{{Name: "data", MountPath: "/data"}}
	})
	target, err := o.Run(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if target.GuestKind != GuestVM || target.LxcID != "201" || target.URL != "http://192.168.1.50:8080" {
		t.Fatalf("target = %+v", target)
	}

	got := calls()
	if _, ok := findCall(got, "sh", "-c", "command -v docker >/dev/null 2>&1"); !ok {
		t.Fatal("never checked for docker in the VM")
	}
	if c, ok := findCall(got, "sh", "-c"); !ok || !slices.ContainsFunc(got, func(c sshCall) bool {
		return len(c.argv) == 3 && strings.Contains(c.argv[2], "get.docker.com")
	}) {
		t.Fatalf("docker was not installed: %v", c)
	}
	env, ok := findCall(got, "sh", "-c", "umask 077; cat > /run/ownship-app.env")
	if !ok || !strings.Contains(env.stdin, "SECRET="+hostile) {
		t.Fatalf("env file = %q", env.stdin)
	}
	run, ok := findCall(got, "docker", "run", "-d", "--name", lxcAppContainer)
	if !ok || !slices.Contains(run.argv, "/opt/ownship-volumes/data:/data") || run.argv[len(run.argv)-1] != "nginx:1.27" {
		t.Fatalf("docker run = %v", run.argv)
	}
	for _, c := range got {
		if strings.Contains(strings.Join(c.argv, " "), "/tmp/pwned") && c.argv[0] != "sh" {
			t.Fatalf("the hostile value escaped into argv: %v", c.argv)
		}
	}
	if _, err := os.Stat("/tmp/pwned"); err == nil {
		t.Fatal("a value ran as a command")
	}
	// Every remote word arrives as one single-quoted shell word.
	for _, r := range raw() {
		for _, w := range r {
			if !strings.HasPrefix(w, "'") || !strings.HasSuffix(w, "'") {
				t.Fatalf("unquoted remote word %q in %v", w, r)
			}
		}
	}

	if _, err := o.StatsAll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := findCall(calls(), "sh", "-c"); !ok {
		t.Fatal("stats never ran in the VM")
	}

	shell, err := o.ExecArgs(ctx, "proj-vm", []string{"sh", "-c", "echo hi; id"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(shell, "-tt") || shell[len(shell)-1] != shellQuote("echo hi; id") {
		t.Fatalf("exec argv = %v", shell)
	}

	*qm = nil
	if err := o.Stop(ctx, "proj-vm"); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(*qm, func(a []string) bool { return a[0] == "shutdown" && a[1] == "201" }) {
		t.Fatalf("sleep did not shut the VM down: %v", verbs(*qm))
	}
	if err := o.Start(ctx, "proj-vm"); err != nil {
		t.Fatal(err)
	}
	if _, ok := findCall(calls(), "docker", "start", lxcAppContainer); !ok {
		t.Fatal("wake did not start the app container")
	}

	*qm = nil
	if err := o.Destroy(ctx, "proj-vm", false); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(*qm, func(a []string) bool { return a[0] == "destroy" && a[1] == "201" }) {
		t.Fatalf("destroy did not remove the VM: %v", verbs(*qm))
	}
	if _, ok := o.vms.get("proj-vm"); ok {
		t.Fatal("the destroyed VM is still tracked")
	}
}
