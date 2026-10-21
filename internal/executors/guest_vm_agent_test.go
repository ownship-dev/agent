package executors

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
)

// fakeQM records every qm argv and answers through reply.
func fakeQM(t *testing.T, reply func(args []string) ([]byte, error)) *[][]string {
	t.Helper()
	var mu sync.Mutex
	var calls [][]string
	orig := qmOutput
	qmOutput = func(_ context.Context, args ...string) ([]byte, error) {
		mu.Lock()
		calls = append(calls, append([]string{}, args...))
		mu.Unlock()
		return reply(args)
	}
	t.Cleanup(func() { qmOutput = orig })
	return &calls
}

func guestExecReply(out string, code int) []byte {
	b, _ := json.Marshal(map[string]any{"exitcode": code, "exited": 1, "out-data": out})
	return b
}

func testHostKey(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, _ := ssh.NewPublicKey(pub)
	return string(ssh.MarshalAuthorizedKey(k))
}

func TestEnsureVMKeyCreatesOnceWithTightModes(t *testing.T) {
	p := newSSHPaths(filepath.Join(t.TempDir(), "state"))
	pub, created, err := ensureVMKey(p)
	if err != nil || !created || !strings.HasPrefix(pub, "ssh-ed25519 ") {
		t.Fatalf("%q %v %v", pub, created, err)
	}
	for path, want := range map[string]os.FileMode{p.Key: 0o600, filepath.Dir(p.Key): 0o700, p.ControlDir: 0o700} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("%s: %v %v, want %v", path, info.Mode().Perm(), err, want)
		}
	}
	again, created, err := ensureVMKey(p)
	if err != nil || created || again != pub {
		t.Fatalf("second call must reuse the key: %q %v %v", again, created, err)
	}
}

func TestParseHostKeyAcceptsOnlyEd25519(t *testing.T) {
	line, fpr, err := parseHostKey(testHostKey(t) + "\n")
	if err != nil || !strings.HasPrefix(line, "ssh-ed25519 ") || !strings.HasPrefix(fpr, "SHA256:") {
		t.Fatalf("%q %q %v", line, fpr, err)
	}
	for _, bad := range []string{"", "garbage", "ssh-rsa AAAAB3NzaC1yc2E= x", "ssh-ed25519 AAAA\nssh-ed25519 BBBB"} {
		if _, _, err := parseHostKey(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestPinHostKeyReplacesOnlyThatAlias(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	a, b, c := strings.TrimSpace(testHostKey(t)), strings.TrimSpace(testHostKey(t)), strings.TrimSpace(testHostKey(t))
	_ = pinHostKey(path, "ownship-vm-1", a)
	_ = pinHostKey(path, "ownship-vm-2", b)
	_ = pinHostKey(path, "ownship-vm-1", c)
	data, _ := os.ReadFile(path)
	got := string(data)
	if strings.Contains(got, a) || !strings.Contains(got, "ownship-vm-1 "+c) || !strings.Contains(got, "ownship-vm-2 "+b) {
		t.Fatalf("known_hosts:\n%s", got)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode().Perm())
	}
	_ = unpinHostKey(path, "ownship-vm-2")
	data, _ = os.ReadFile(path)
	if strings.Contains(string(data), "ownship-vm-2") {
		t.Fatal("unpin left the entry")
	}
}

func TestVMPinHostKeyReadsThroughTheGuestAgent(t *testing.T) {
	key := testHostKey(t)
	calls := fakeQM(t, func(args []string) ([]byte, error) {
		return guestExecReply(key, 0), nil
	})
	p := newSSHPaths(t.TempDir())
	fpr, err := vmPinHostKey(context.Background(), p, "201")
	if err != nil || !strings.HasPrefix(fpr, "SHA256:") {
		t.Fatalf("%q %v", fpr, err)
	}
	want := []string{"guest", "exec", "201", "--timeout", "60", "--", "cat", "/etc/ssh/ssh_host_ed25519_key.pub"}
	if !slices.Equal((*calls)[0], want) {
		t.Fatalf("argv %q", (*calls)[0])
	}
	data, _ := os.ReadFile(p.KnownHosts)
	if !strings.HasPrefix(string(data), "ownship-vm-201 ssh-ed25519 ") {
		t.Fatalf("known_hosts %q", data)
	}
}

func TestVMPinHostKeyRefusesAFailedRead(t *testing.T) {
	fakeQM(t, func([]string) ([]byte, error) { return guestExecReply("", 1), nil })
	p := newSSHPaths(t.TempDir())
	if _, err := vmPinHostKey(context.Background(), p, "201"); err == nil {
		t.Fatal("want an error")
	}
	if _, err := os.Stat(p.KnownHosts); !os.IsNotExist(err) {
		t.Fatal("nothing may be pinned on failure")
	}
}

func TestVMInstallKeyPassesTheKeyAsAnArgument(t *testing.T) {
	calls := fakeQM(t, func([]string) ([]byte, error) { return guestExecReply("", 0), nil })
	if err := vmInstallKey(context.Background(), "201", "ssh-ed25519 AAAA agent"); err != nil {
		t.Fatal(err)
	}
	argv := (*calls)[0]
	if argv[len(argv)-1] != "ssh-ed25519 AAAA agent" || argv[len(argv)-2] != "sh" || strings.Contains(argv[len(argv)-3], "AAAA") {
		t.Fatalf("the key must be a positional argument, never part of the script: %q", argv)
	}
}

func TestVMGuestIPv4SkipsLoopbackBridgesAndLinkLocal(t *testing.T) {
	fakeQM(t, func([]string) ([]byte, error) {
		return []byte(`[
			{"name":"lo","ip-addresses":[{"ip-address":"127.0.0.1","ip-address-type":"ipv4"}]},
			{"name":"docker0","ip-addresses":[{"ip-address":"172.17.0.1","ip-address-type":"ipv4"}]},
			{"name":"eth0","ip-addresses":[
				{"ip-address":"fe80::1","ip-address-type":"ipv6"},
				{"ip-address":"169.254.3.4","ip-address-type":"ipv4"},
				{"ip-address":"192.168.1.50","ip-address-type":"ipv4"}]}]`), nil
	})
	ip, err := vmGuestIPv4(context.Background(), "201")
	if err != nil || ip != "192.168.1.50" {
		t.Fatalf("%q %v", ip, err)
	}
}

func TestVMGuestIPv4RejectsJunk(t *testing.T) {
	for _, reply := range []string{`not json`, `[{"name":"eth0","ip-addresses":[{"ip-address":"1.2.3.4 -oProxyCommand=x","ip-address-type":"ipv4"}]}]`, `[]`} {
		fakeQM(t, func([]string) ([]byte, error) { return []byte(reply), nil })
		if ip, err := vmGuestIPv4(context.Background(), "201"); err == nil {
			t.Errorf("reply %s gave %q", reply, ip)
		}
	}
	fakeQM(t, func([]string) ([]byte, error) {
		return []byte("QEMU guest agent is not running"), errors.New("exit 255")
	})
	if _, err := vmGuestIPv4(context.Background(), "201"); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("err %v", err)
	}
}
