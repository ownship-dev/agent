package executors

import (
	"math/rand"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// remoteShell runs words the way sshd does: joined with spaces and parsed by sh -c.
func remoteShell(t *testing.T, words []string) string {
	t.Helper()
	out, err := exec.Command("sh", "-c", strings.Join(words, " ")).Output()
	if err != nil {
		t.Fatalf("sh -c %q: %v", strings.Join(words, " "), err)
	}
	return string(out)
}

func quotedPrintf(w string) []string {
	return GuestPrefix{Quote: true}.Wrap("printf", "%s", w)
}

func TestShellQuoteGoldenCases(t *testing.T) {
	for _, w := range []string{
		"", "plain", "it's", "'", "''", `\`, `\'`, "$(id)", "`id`", "${HOME}", "a b  c", "\n", "a\nb",
		"--", "--help", "-n", "*", "?", "~", "#comment", "a;b&&c||d", "|", ">x", "<x", "\t", "\"", `"$x"`, "!",
	} {
		if got := remoteShell(t, quotedPrintf(w)); got != w {
			t.Errorf("round trip of %q gave %q", w, got)
		}
	}
}

func TestShellQuoteRandomBytes(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 300; i++ {
		b := make([]byte, r.Intn(40))
		for j := range b {
			b[j] = byte(1 + r.Intn(255)) // NUL is refused separately
		}
		w := string(b)
		if got := remoteShell(t, quotedPrintf(w)); got != w {
			t.Fatalf("round trip of %q gave %q", w, got)
		}
	}
}

func FuzzShellQuote(f *testing.F) {
	for _, s := range []string{"", "'", "$(id)", "a\nb", "--", "\xff"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, w string) {
		if strings.ContainsRune(w, 0) {
			if argv := quotedPrintf(w); argv[0] != "sh" || !strings.Contains(argv[len(argv)-1], "NUL") {
				t.Fatalf("NUL not refused: %q", argv)
			}
			return
		}
		if got := remoteShell(t, quotedPrintf(w)); got != w {
			t.Fatalf("round trip of %q gave %q", w, got)
		}
	})
}

func TestQuotedPrefixRefusesNUL(t *testing.T) {
	argv := GuestPrefix{Argv: []string{"ssh", "h"}, Quote: true}.Wrap("echo", "a\x00b")
	if slices.Contains(argv, "ssh") {
		t.Fatalf("NUL word reached ssh: %q", argv)
	}
}

func TestSSHPrefixPinsTheHostKeyAndRunsAsRoot(t *testing.T) {
	p := sshPrefix(newSSHPaths("/var/lib/ownship/.ownship"), "201", "10.0.0.9", false)
	if !p.Quote {
		t.Fatal("an SSH prefix must quote inner words")
	}
	joined := strings.Join(p.Argv, " ")
	for _, want := range []string{
		"-i /var/lib/ownship/.ownship/vm_ssh_ed25519",
		"StrictHostKeyChecking=yes",
		"UserKnownHostsFile=/var/lib/ownship/.ownship/vm_known_hosts",
		"HostKeyAlias=ownship-vm-201",
		"BatchMode=yes",
		"PasswordAuthentication=no",
		"ControlPath=/var/lib/ownship/.ownship/vm_ssh_control/%C",
		"-T -l ownship -- 10.0.0.9 sudo -n --",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %q", want, joined)
		}
	}
	if !strings.HasSuffix(joined, "10.0.0.9 sudo -n --") {
		t.Fatalf("host and sudo must end the prefix: %q", joined)
	}
	if tty := strings.Join(sshPrefix(newSSHPaths("/s"), "1", "10.0.0.9", true).Argv, " "); !strings.Contains(tty, " -tt ") {
		t.Fatalf("a tty session needs -tt: %q", tty)
	}
	// Words after the prefix are quoted, so a volume path or env pair stays one word remotely.
	argv := p.Wrap("sh", "-c", "mkdir -p '/opt/x' && cat > /opt/x/f")
	if argv[len(argv)-1] != `'mkdir -p '\''/opt/x'\'' && cat > /opt/x/f'` {
		t.Fatalf("got %q", argv[len(argv)-1])
	}
}
