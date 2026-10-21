package updater

import (
	"context"
	"crypto/ed25519"
	"regexp"
	"strings"
	"testing"
)

func TestVerifyReleaseBindsVersion(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	binary := []byte("fake-binary-bytes")

	// A signature the release tooling would produce for v1.2.0.
	sig := ed25519.Sign(priv, signedMessage("1.2.0", binary))

	if !verifyRelease(pub, "1.2.0", binary, sig) {
		t.Fatal("valid (version, binary, sig) should verify")
	}

	// Replaying under a higher version must fail: the version is part of the signed message.
	if verifyRelease(pub, "9.9.9", binary, sig) {
		t.Fatal("replay under a different version must fail verification")
	}

	if verifyRelease(pub, "1.2.0", []byte("evil-binary"), sig) {
		t.Fatal("tampered binary must fail verification")
	}
}

func TestAutoUpdateEnabledTracksReleaseKey(t *testing.T) {
	orig := ReleasePublicKey
	t.Cleanup(func() { ReleasePublicKey = orig })

	ReleasePublicKey = ""
	if AutoUpdateEnabled() {
		t.Fatal("auto-update must be disabled without a release key")
	}
	ReleasePublicKey = "any-key"
	if !AutoUpdateEnabled() {
		t.Fatal("auto-update must be enabled once a release key is set")
	}
}

// The key is public and ships in source, so source builds verify like official ones.
// Blanking it is what a self-hosted-bucket build does.
func TestReleaseKeyShipsInSource(t *testing.T) {
	if ReleasePublicKey == "" {
		t.Fatal("release public key must be compiled in by default")
	}
	if _, err := releaseKey(); err != nil {
		t.Fatalf("compiled-in release key is unusable: %v", err)
	}
	if !AutoUpdateEnabled() {
		t.Fatal("auto-update must be on by default now the key ships in source")
	}
}

// Fail closed: with no key the update aborts before any download.
func TestVerifiedChecksumsRefusesWithoutAKey(t *testing.T) {
	orig := ReleasePublicKey
	t.Cleanup(func() { ReleasePublicKey = orig })
	ReleasePublicKey = ""

	// An unreachable base proves the key check runs first.
	_, err := verifiedChecksums(t.Context(), "https://127.0.0.1:1/agent/releases/latest")
	if err == nil {
		t.Fatal("verifiedChecksums must fail without a release key")
	}
	if !strings.Contains(err.Error(), "no release signing key") {
		t.Fatalf("expected the missing-key error, got: %v", err)
	}
}

func TestCurrentChecksum(t *testing.T) {
	// Hashes the test binary itself — should be a stable 64-char hex digest.
	sum, err := currentChecksum()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(sum) {
		t.Fatalf("currentChecksum = %q; want 64 hex chars", sum)
	}
	again, err := currentChecksum()
	if err != nil || again != sum {
		t.Fatalf("currentChecksum not stable: %q vs %q (%v)", sum, again, err)
	}
}

func TestChecksumFor(t *testing.T) {
	sums := "aaa111  agent-linux-amd64\nbbb222  agent-linux-arm64\n"
	got, err := checksumFor(sums, "agent-linux-arm64")
	if err != nil || got != "bbb222" {
		t.Fatalf("checksumFor arm64 = %q, %v; want bbb222", got, err)
	}
	if _, err := checksumFor(sums, "agent-darwin-arm64"); err == nil {
		t.Fatal("expected error for a binary not in the listing")
	}
}

// VM workloads rely on this: an agent older than VM support would rebuild a VM as an LXC.
func TestApplyRefusesEveryDowngrade(t *testing.T) {
	for _, v := range []string{"1.4.0", "1.5.0"} {
		err := Apply(context.Background(), "https://example.invalid/agent", "", v, "1.5.0")
		if err == nil || !strings.Contains(err.Error(), "refusing downgrade") {
			t.Fatalf("Apply(%s) = %v", v, err)
		}
	}
}
