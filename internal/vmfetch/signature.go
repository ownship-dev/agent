package vmfetch

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
)

const maxKeyBytes = 64 << 10

// Verified returns the text to read digests from. With a key the file must be clearsigned by it
// or match the detached sig; without one a clearsigned body is read as plain text.
func Verified(file, sig []byte, armoredKey string) ([]byte, error) {
	block, _ := clearsign.Decode(file)
	if strings.TrimSpace(armoredKey) == "" {
		if block != nil {
			return block.Plaintext, nil
		}
		return file, nil
	}
	if len(armoredKey) > maxKeyBytes {
		return nil, fmt.Errorf("the signing key is larger than %d bytes", maxKeyBytes)
	}
	keyring, err := openpgp.ReadArmoredKeyRing(strings.NewReader(armoredKey))
	if err != nil {
		return nil, fmt.Errorf("the signing key is not an armored OpenPGP public key: %v", err)
	}
	if block != nil {
		if _, err := block.VerifySignature(keyring, nil); err != nil {
			return nil, fmt.Errorf("the checksum file's signature does not verify: %v", err)
		}
		return block.Plaintext, nil
	}
	if len(sig) == 0 {
		return nil, fmt.Errorf("a signing key is set but the checksum file is not signed and no signature URL is given")
	}
	if bytes.HasPrefix(bytes.TrimSpace(sig), []byte("-----BEGIN PGP")) {
		_, err = openpgp.CheckArmoredDetachedSignature(keyring, bytes.NewReader(file), bytes.NewReader(sig), nil)
	} else {
		_, err = openpgp.CheckDetachedSignature(keyring, bytes.NewReader(file), bytes.NewReader(sig), nil)
	}
	if err != nil {
		return nil, fmt.Errorf("the checksum file's detached signature does not verify: %v", err)
	}
	return file, nil
}
