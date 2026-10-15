// Command signsums signs a file with the release key, as updater verifies it.
// Usage: RELEASE_SIGNING_KEY=<b64> go run ./tools/signsums <in> <out.sig>
package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"os"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: signsums <input> <output.sig>")
		os.Exit(2)
	}
	keyB64 := os.Getenv("RELEASE_SIGNING_KEY")
	if keyB64 == "" {
		fmt.Fprintln(os.Stderr, "RELEASE_SIGNING_KEY is not set")
		os.Exit(1)
	}
	key, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil || len(key) != ed25519.PrivateKeySize {
		fmt.Fprintln(os.Stderr, "RELEASE_SIGNING_KEY must be a base64 ed25519 private key")
		os.Exit(1)
	}
	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "read input:", err)
		os.Exit(1)
	}
	sig := ed25519.Sign(ed25519.PrivateKey(key), data)
	out := base64.StdEncoding.EncodeToString(sig) + "\n"
	if err := os.WriteFile(os.Args[2], []byte(out), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write signature:", err)
		os.Exit(1)
	}
}
