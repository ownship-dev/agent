// Package updater applies an AgentUpdate: download, verify the ed25519 signature against
// a compiled-in key, replace this executable and re-exec.
package updater

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ownship/agent/internal/config"
	"github.com/ownship/agent/internal/version"
)

// Caps the download so a bad URL can't exhaust memory before the signature check.
const maxUpdateBytes = 256 << 20

// updateMu serializes Apply (pushed) and SelfUpdate (hourly) so they can't corrupt the
// binary between them.
var updateMu sync.Mutex

// stagingPath is per-PID so the service and a manual `update` never share a staging file.
func stagingPath(exe string) string {
	return fmt.Sprintf("%s.new.%d", exe, os.Getpid())
}

// ReleasePublicKey verifies every update. It is public and compiled in, so a source build
// verifies like an official one and a compromised control plane can't swap it.
var ReleasePublicKey = "5uvfn0cZ7/bKfnJ/nRBNox1/fQ7LlPr9B7nS0oq3QZQ="

// ReleaseBaseURL is overridable via -ldflags for self-hosted release buckets.
var ReleaseBaseURL = "https://assets.ownship.dev/agent/releases"

func Apply(ctx context.Context, downloadURL, signatureB64, newVersion, currentVersion string) error {
	pub, err := releaseKey()
	if err != nil {
		return err
	}
	// The version is part of the signed message, so an old release can't be replayed as newer.
	if !version.Newer(newVersion, currentVersion) {
		return fmt.Errorf("refusing downgrade: %s <= current %s", newVersion, currentVersion)
	}
	// The signature gives integrity; https also keeps the download private.
	if u, err := url.Parse(downloadURL); err != nil || u.Scheme != "https" {
		return fmt.Errorf("update URL must be https")
	}
	sig, err := base64.StdEncoding.DecodeString(signatureB64)
	if err != nil {
		return fmt.Errorf("bad signature encoding: %w", err)
	}

	data, err := download(ctx, downloadURL)
	if err != nil {
		return err
	}
	if !verifyRelease(pub, newVersion, data, sig) {
		return fmt.Errorf("release signature verification failed")
	}

	updateMu.Lock()
	exe, err := replaceExecutable(data)
	updateMu.Unlock()
	if err != nil {
		return err
	}
	if err := syscall.Exec(exe, os.Args, os.Environ()); err != nil {
		// Exec only returns on failure; restore the previous binary.
		_ = os.Rename(exe+".bak", exe)
		return fmt.Errorf("exec new binary: %w", err)
	}
	return nil
}

// SelfUpdate installs the latest verified release. It does not re-exec; the caller restarts.
func SelfUpdate(ctx context.Context, goos, goarch string) (path, sum string, err error) {
	base, err := latestBase()
	if err != nil {
		return "", "", err
	}
	// Polling `latest` isn't version-bound like Apply, so refuse a channel that went backwards.
	latest, err := latestVersion(ctx, base)
	if err != nil {
		return "", "", err
	}
	if !version.Newer(latest, config.Version) {
		return "", "", fmt.Errorf("refusing downgrade: %s <= current %s", latest, config.Version)
	}
	bin := fmt.Sprintf("agent-%s-%s", goos, goarch)

	exe, err := os.Executable()
	if err != nil {
		return "", "", err
	}
	// Held across the download: two SelfUpdates here share one staging file.
	updateMu.Lock()
	defer updateMu.Unlock()

	// Stream to a file on the same filesystem (atomic rename), hashing as it lands.
	staged := stagingPath(exe)
	got, err := downloadToFile(ctx, base+"/"+bin, staged)
	if err != nil {
		return "", "", fmt.Errorf("download %s: %w", bin, err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(staged)
		}
	}()

	sums, err := verifiedChecksums(ctx, base)
	if err != nil {
		return "", "", err
	}

	want, err := checksumFor(string(sums), bin)
	if err != nil {
		return "", "", err
	}
	if got != want {
		return "", "", fmt.Errorf("checksum mismatch for %s", bin)
	}

	if err := swapIn(exe, staged); err != nil {
		return "", "", err
	}
	ok = true
	return exe, want, nil
}

// AutoUpdateEnabled requires a compiled-in key; manual `update` works without one.
func AutoUpdateEnabled() bool { return ReleasePublicKey != "" }

// NeedsUpdate compares checksums only, so nothing is downloaded unless something changed.
func NeedsUpdate(ctx context.Context, goos, goarch string) (bool, error) {
	base, err := latestBase()
	if err != nil {
		return false, err
	}
	sums, err := verifiedChecksums(ctx, base)
	if err != nil {
		return false, err
	}
	want, err := checksumFor(string(sums), fmt.Sprintf("agent-%s-%s", goos, goarch))
	if err != nil {
		return false, err
	}
	cur, err := currentChecksum()
	if err != nil {
		return false, err
	}
	return want != cur, nil
}

// latestVersion reads version.txt, published with checksums.txt.
func latestVersion(ctx context.Context, base string) (string, error) {
	data, err := download(ctx, base+"/version.txt")
	if err != nil {
		return "", fmt.Errorf("fetch version: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

func latestBase() (string, error) {
	base := strings.TrimRight(ReleaseBaseURL, "/") + "/latest"
	if u, err := url.Parse(base); err != nil || u.Scheme != "https" {
		return "", fmt.Errorf("release URL must be https")
	}
	return base, nil
}

// verifiedChecksums fails closed: no key, no signature or a bad one aborts the update rather
// than trusting TLS alone.
func verifiedChecksums(ctx context.Context, base string) ([]byte, error) {
	pub, err := releaseKey()
	if err != nil {
		return nil, err
	}
	sums, err := download(ctx, base+"/checksums.txt")
	if err != nil {
		return nil, fmt.Errorf("fetch checksums: %w", err)
	}
	sigB64, err := download(ctx, base+"/checksums.txt.sig")
	if err != nil {
		return nil, fmt.Errorf("fetch checksums signature: %w", err)
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sigB64)))
	if err != nil {
		return nil, fmt.Errorf("bad checksums signature encoding: %w", err)
	}
	if !ed25519.Verify(pub, sums, sig) {
		return nil, fmt.Errorf("checksums signature verification failed")
	}
	return sums, nil
}

func currentChecksum() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	f, err := os.Open(exe)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// checksumFor reads a `shasum -a 256` listing.
func checksumFor(sums, bin string) (string, error) {
	for _, line := range strings.Split(sums, "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[1] == bin {
			return f[0], nil
		}
	}
	return "", fmt.Errorf("no checksum published for %s", bin)
}

// replaceExecutable swaps the binary atomically, keeping <exe>.bak for rollback.
func replaceExecutable(data []byte) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	tmp := stagingPath(exe)
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return "", err
	}
	if err := swapIn(exe, tmp); err != nil {
		return "", err
	}
	return exe, nil
}

// swapIn replaces exe with newPath, keeping <exe>.bak for rollback.
func swapIn(exe, newPath string) error {
	bak := exe + ".bak"
	if err := copyFile(exe, bak); err != nil {
		_ = os.Remove(newPath)
		return fmt.Errorf("backup current binary: %w", err)
	}
	// Atomic on one filesystem; the running process keeps its old inode until it restarts.
	if err := os.Rename(newPath, exe); err != nil {
		_ = os.Remove(bak)
		_ = os.Remove(newPath)
		return fmt.Errorf("replace binary: %w", err)
	}
	return nil
}

// downloadToFile streams to dest with the same size cap, returning the hex sha256.
func downloadToFile(ctx context.Context, downloadURL, dest string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download failed: %s", resp.Status)
	}
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxUpdateBytes+1))
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(dest)
		return "", err
	}
	if n > maxUpdateBytes {
		_ = os.Remove(dest)
		return "", fmt.Errorf("update exceeds %d byte limit", maxUpdateBytes)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// signedMessage is version + "\n" + binary, so a signature can't be reused across versions.
func signedMessage(version string, data []byte) []byte {
	msg := make([]byte, 0, len(version)+1+len(data))
	msg = append(msg, version...)
	msg = append(msg, '\n')
	return append(msg, data...)
}

func verifyRelease(pub ed25519.PublicKey, version string, data, sig []byte) bool {
	return ed25519.Verify(pub, signedMessage(version, data), sig)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func releaseKey() (ed25519.PublicKey, error) {
	if ReleasePublicKey == "" {
		return nil, fmt.Errorf("auto-update disabled: no release signing key compiled in")
	}
	raw, err := base64.StdEncoding.DecodeString(ReleasePublicKey)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("invalid release public key")
	}
	return ed25519.PublicKey(raw), nil
}

func download(ctx context.Context, downloadURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, err
	}
	// AgentUpdate has no command timeout, so bound the download against a hung CDN.
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download failed: %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxUpdateBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxUpdateBytes {
		return nil, fmt.Errorf("update exceeds %d byte limit", maxUpdateBytes)
	}
	return data, nil
}
