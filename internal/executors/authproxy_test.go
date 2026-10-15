package executors

import (
	"strings"
	"testing"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
	"golang.org/x/crypto/bcrypt"
)

func TestRenderCaddyfile(t *testing.T) {
	auth := &agentv1.BasicAuth{User: "admin", Password: "s3cret pass{word}"}
	out, err := renderCaddyfile(auth, "ownship-abc:3000")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, auth.GetPassword()) {
		t.Fatal("plaintext password leaked into the Caddyfile")
	}
	if !strings.Contains(out, "reverse_proxy ownship-abc:3000") {
		t.Fatalf("missing upstream:\n%s", out)
	}
	// The emitted hash must actually verify the original password.
	fields := strings.Fields(out)
	var hash string
	for i, f := range fields {
		if f == "admin" && i+1 < len(fields) {
			hash = fields[i+1]
		}
	}
	if hash == "" {
		t.Fatalf("no hash after user token:\n%s", out)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(auth.GetPassword())); err != nil {
		t.Fatalf("hash does not verify the password: %v", err)
	}
}

func TestRenderCaddyfileRejectsUnsafeUser(t *testing.T) {
	for _, user := range []string{"", "a b", "a}b", "a{b", `a"b`, "a\nb", "a\tb", "a'b", "a`b"} {
		if _, err := renderCaddyfile(&agentv1.BasicAuth{User: user, Password: "x"}, "up:1"); err == nil {
			t.Fatalf("user %q should be rejected", user)
		}
	}
}
