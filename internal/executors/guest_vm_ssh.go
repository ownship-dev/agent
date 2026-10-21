package executors

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

// shellQuote makes w one word for a POSIX shell: single-quoted, each ' closed, escaped and reopened.
func shellQuote(w string) string {
	return "'" + strings.ReplaceAll(w, "'", `'\''`) + "'"
}

// The guest account cloud-init creates; it has passwordless sudo, root SSH stays off.
const vmUser = "ownship"

// sshPaths are the agent's VM credentials, all under its state dir.
type sshPaths struct {
	Key        string // ed25519 private key, 0600
	KnownHosts string // pinned guest host keys, 0600
	ControlDir string // multiplexing sockets, 0700
}

func newSSHPaths(stateDir string) sshPaths {
	return sshPaths{
		Key:        filepath.Join(stateDir, "vm_ssh_ed25519"),
		KnownHosts: filepath.Join(stateDir, "vm_known_hosts"),
		ControlDir: filepath.Join(stateDir, "vm_ssh_control"),
	}
}

// hostKeyAlias keys the pinned host key by vmid, so a DHCP address change keeps the pin and
// another guest taking the old address can't use it.
func hostKeyAlias(vmid string) string { return "ownship-vm-" + vmid }

// sshPrefix runs a command in the VM as root through sudo. The caller has validated ip and
// vmid; inner words are quoted by GuestPrefix because ssh hands them to a remote shell.
func sshPrefix(p sshPaths, vmid, ip string, tty bool) GuestPrefix {
	argv := []string{"ssh",
		"-i", p.Key,
		"-o", "BatchMode=yes",
		"-o", "IdentitiesOnly=yes",
		"-o", "PasswordAuthentication=no",
		"-o", "KbdInteractiveAuthentication=no",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "UserKnownHostsFile=" + p.KnownHosts,
		"-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "HostKeyAlias=" + hostKeyAlias(vmid),
		"-o", "ConnectTimeout=10",
		"-o", "ServerAliveInterval=15",
		"-o", "ControlMaster=auto",
		"-o", "ControlPersist=60",
		"-o", "ControlPath=" + filepath.Join(p.ControlDir, "%C"),
		"-o", "LogLevel=ERROR",
	}
	if tty {
		argv = append(argv, "-tt")
	} else {
		argv = append(argv, "-T")
	}
	argv = append(argv, "-l", vmUser, "--", ip, "sudo", "-n", "--")
	return GuestPrefix{Argv: argv, Quote: true}
}

// ensureVMKey returns the agent's VM public key, creating the key pair on first use. The
// private key never leaves this file: not sent upstream, not logged.
func ensureVMKey(p sshPaths) (authorized string, created bool, err error) {
	if err := os.MkdirAll(filepath.Dir(p.Key), 0o700); err != nil {
		return "", false, err
	}
	if err := os.MkdirAll(p.ControlDir, 0o700); err != nil {
		return "", false, err
	}
	if err := os.Chmod(p.ControlDir, 0o700); err != nil {
		return "", false, err
	}
	if data, err := os.ReadFile(p.Key); err == nil {
		signer, err := ssh.ParsePrivateKey(data)
		if err != nil {
			return "", false, fmt.Errorf("read VM SSH key: %w", err)
		}
		return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))), false, nil
	} else if !os.IsNotExist(err) {
		return "", false, err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", false, err
	}
	block, err := ssh.MarshalPrivateKey(priv, "ownship-agent")
	if err != nil {
		return "", false, err
	}
	if err := writeFileAtomic(p.Key, pem.EncodeToMemory(block), 0o600); err != nil {
		return "", false, err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", false, err
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))), true, nil
}

// parseHostKey accepts only an ed25519 host key line and returns it normalised with its
// SHA256 fingerprint.
func parseHostKey(raw string) (keyLine, fpr string, err error) {
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(strings.TrimSpace(raw)))
	if err != nil {
		return "", "", fmt.Errorf("the guest's SSH host key is unreadable: %w", err)
	}
	if key.Type() != ssh.KeyAlgoED25519 {
		return "", "", fmt.Errorf("the guest's SSH host key is %s; an ed25519 key is required", key.Type())
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))), ssh.FingerprintSHA256(key), nil
}

// pinHostKey replaces the alias's entry in known_hosts, leaving every other guest's pin.
func pinHostKey(path, alias, keyLine string) error {
	var keep []string
	if data, err := os.ReadFile(path); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if f := strings.Fields(line); len(f) > 0 && f[0] != alias {
				keep = append(keep, line)
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	keep = append(keep, alias+" "+keyLine)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(path, []byte(strings.Join(keep, "\n")+"\n"), 0o600)
}

// unpinHostKey drops a destroyed guest's entry.
func unpinHostKey(path, alias string) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var keep []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if f := strings.Fields(line); len(f) > 0 && f[0] != alias {
			keep = append(keep, line)
		}
	}
	out := ""
	if len(keep) > 0 {
		out = strings.Join(keep, "\n") + "\n"
	}
	return writeFileAtomic(path, []byte(out), 0o600)
}
