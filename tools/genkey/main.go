// Command genkey creates an ed25519 release-signing keypair. Never commit the private key.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
)

func main() {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fmt.Fprintln(os.Stderr, "keygen failed:", err)
		os.Exit(1)
	}
	fmt.Println("RELEASE_PUBKEY (repo variable):")
	fmt.Println(base64.StdEncoding.EncodeToString(pub))
	fmt.Println()
	fmt.Println("RELEASE_SIGNING_KEY (repo secret — keep private):")
	fmt.Println(base64.StdEncoding.EncodeToString(priv))
}
