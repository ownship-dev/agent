package vmfetch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/tls"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
)

func TestFindExpectedReadsGNUAndBSDLines(t *testing.T) {
	s256 := strings.Repeat("a", 64)
	s512 := strings.Repeat("b", 128)
	file := []byte(strings.Join([]string{
		"# comment",
		s256 + "  other.img",
		s512 + " *debian-12-genericcloud-amd64.qcow2",
		"SHA256 (Fedora-Cloud-Base-Generic-44-1.7.x86_64.qcow2) = " + strings.ToUpper(s256),
		s256 + "  ./nested/rocky.qcow2",
	}, "\n"))
	cases := map[string]Expected{
		"debian-12-genericcloud-amd64.qcow2":            {"sha512", s512},
		"Fedora-Cloud-Base-Generic-44-1.7.x86_64.qcow2": {"sha256", s256},
		"rocky.qcow2": {"sha256", s256},
	}
	for name, want := range cases {
		got, err := FindExpected(file, name)
		if err != nil || got != want {
			t.Fatalf("%s: got %+v %v, want %+v", name, got, err, want)
		}
	}
	if _, err := FindExpected(file, "missing.img"); err == nil || !strings.Contains(err.Error(), "no line for missing.img") {
		t.Fatalf("missing line: %v", err)
	}
}

func TestFindExpectedRefusesConflictingLines(t *testing.T) {
	file := []byte(strings.Repeat("a", 64) + "  x.img\n" + strings.Repeat("c", 64) + "  x.img\n")
	if _, err := FindExpected(file, "x.img"); err == nil {
		t.Fatal("two digests for one file must be refused")
	}
	same := []byte(strings.Repeat("a", 64) + "  x.img\n" + strings.Repeat("a", 64) + "  x.img\n")
	if _, err := FindExpected(same, "x.img"); err != nil {
		t.Fatalf("a repeated identical line is fine: %v", err)
	}
}

func TestBSDLineWithWrongLengthIsIgnored(t *testing.T) {
	file := []byte("SHA256 (x.img) = " + strings.Repeat("a", 128) + "\n")
	if _, err := FindExpected(file, "x.img"); err == nil {
		t.Fatal("a sha256 tag with a 128-digit value is not a digest")
	}
}

func newKey(t *testing.T) (*openpgp.Entity, string) {
	t.Helper()
	e, err := openpgp.NewEntity("vendor", "", "vendor@example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, openpgp.PublicKeyType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Serialize(w); err != nil {
		t.Fatal(err)
	}
	w.Close()
	return e, buf.String()
}

func clearsigned(t *testing.T, e *openpgp.Entity, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := clearsign.Encode(&buf, e.PrivateKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(w, body)
	w.Close()
	return buf.Bytes()
}

func TestVerifiedClearsigned(t *testing.T) {
	signer, key := newKey(t)
	_, otherKey := newKey(t)
	body := "SHA256 (x.img) = " + strings.Repeat("a", 64) + "\n"
	file := clearsigned(t, signer, body)

	text, err := Verified(file, nil, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := FindExpected(text, "x.img"); err != nil {
		t.Fatalf("plaintext lost the line: %v", err)
	}
	if _, err := Verified(file, nil, otherKey); err == nil {
		t.Fatal("a signature by another key must fail")
	}
	tampered := bytes.Replace(file, []byte("aaaa"), []byte("bbbb"), 1)
	if _, err := Verified(tampered, nil, key); err == nil {
		t.Fatal("a changed body must fail")
	}
	// Without a key the body is still read, unverified.
	if text, err := Verified(file, nil, ""); err != nil || !strings.Contains(string(text), "x.img") {
		t.Fatalf("no key: %q %v", text, err)
	}
}

func TestVerifiedDetached(t *testing.T) {
	signer, key := newKey(t)
	file := []byte(strings.Repeat("a", 64) + "  x.img\n")
	var armored, binary bytes.Buffer
	if err := openpgp.ArmoredDetachSign(&armored, signer, bytes.NewReader(file), nil); err != nil {
		t.Fatal(err)
	}
	if err := openpgp.DetachSign(&binary, signer, bytes.NewReader(file), nil); err != nil {
		t.Fatal(err)
	}
	for name, sig := range map[string][]byte{"armored": armored.Bytes(), "binary": binary.Bytes()} {
		if _, err := Verified(file, sig, key); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := Verified(append([]byte("x"), file...), sig, key); err == nil {
			t.Fatalf("%s: a changed file must fail", name)
		}
	}
	if _, err := Verified(file, nil, key); err == nil || !strings.Contains(err.Error(), "not signed") {
		t.Fatalf("a key with nothing signed must fail: %v", err)
	}
	if _, err := Verified(file, nil, "not a key"); err == nil {
		t.Fatal("garbage key must fail")
	}
}

func TestPublicAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"1.1.1.1":         true,
		"2606:4700::1111": true,
		"127.0.0.1":       false,
		"10.0.0.5":        false,
		"192.168.1.10":    false,
		"172.16.0.1":      false,
		"169.254.169.254": false,
		"100.64.1.1":      false,
		"0.0.0.1":         false,
		"::1":             false,
		"fd00::1":         false,
		"fe80::1":         false,
		"::ffff:10.0.0.1": false,
		"224.0.0.1":       false,
	} {
		if got := publicAddr(netip.MustParseAddr(addr)); got != want {
			t.Errorf("%s: got %v want %v", addr, got, want)
		}
	}
}

func TestCheckURL(t *testing.T) {
	ok := "https://cloud.debian.org/images/x.qcow2"
	if _, err := CheckURL(ok, Strict); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		"http://cloud.debian.org/x.qcow2",
		"https://user:pw@cloud.debian.org/x.qcow2",
		"https://cloud.debian.org:8443/x.qcow2",
		"https://evil.example/x.qcow2",
		"file:///etc/passwd",
	} {
		if _, err := CheckURL(bad, Mirror); err == nil {
			t.Errorf("%s must be refused for a catalogue image", bad)
		}
	}
	if _, err := CheckURL("https://access.cdn.redhat.com/rhel.qcow2?token=x", Open); err != nil {
		t.Fatalf("a link may name any host: %v", err)
	}
}

// testServer answers every host on its own listener, so tests can name real-looking hosts on
// the default port while the dial guard still sees the real address.
func testServer(t *testing.T, h http.Handler, allow ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	oldAddr, oldHost, oldHook := publicAddr, allowedHost, transportHook
	t.Cleanup(func() { publicAddr, allowedHost, transportHook = oldAddr, oldHost, oldHook })
	publicAddr = func(netip.Addr) bool { return true }
	allowed := map[string]bool{}
	for _, a := range allow {
		allowed[a] = true
	}
	allowedHost = func(h string) bool { return allowed[h] }
	addr := srv.Listener.Addr().String()
	transportHook = func(tr *http.Transport) {
		dial := tr.DialContext
		tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dial(ctx, network, addr)
		}
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return srv
}

func TestDownloadHashesAndCaps(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 3<<20)
	testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}), "cloud.debian.org")
	raw := "https://cloud.debian.org/img.qcow2"

	var out bytes.Buffer
	var calls int
	sums, err := Download(context.Background(), raw, Mirror, &out, 10<<20, func(done, total int64) { calls++ })
	if err != nil {
		t.Fatal(err)
	}
	s256 := sha256.Sum256(payload)
	s512 := sha512.Sum512(payload)
	if sums.SHA256 != hex.EncodeToString(s256[:]) || sums.SHA512 != hex.EncodeToString(s512[:]) || sums.Bytes != int64(len(payload)) {
		t.Fatalf("sums: %+v", sums)
	}
	if !bytes.Equal(out.Bytes(), payload) || calls == 0 {
		t.Fatal("payload not written or no progress")
	}
	if _, err := Download(context.Background(), raw, Mirror, io.Discard, 1<<20, nil); err == nil {
		t.Fatal("over the cap must fail")
	}
}

func TestRedirectRules(t *testing.T) {
	testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "mirror.example" {
			w.Write([]byte("image"))
			return
		}
		http.Redirect(w, r, "https://mirror.example"+r.URL.Path, http.StatusFound)
	}), "dl.fedoraproject.org")
	raw := "https://dl.fedoraproject.org/img"

	if _, err := Download(context.Background(), raw, Mirror, io.Discard, 1<<20, nil); err != nil {
		t.Fatalf("an image may follow a redirect to a mirror: %v", err)
	}
	if _, err := Small(context.Background(), raw, 1<<20); err == nil || !strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("a checksum must not leave the allowlist: %v", err)
	}
}

func TestErrorsNeverCarryTheLink(t *testing.T) {
	testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	raw := "https://access.cdn.redhat.example/rhel.qcow2?_auth_=SECRET"
	_, err := Download(context.Background(), raw, Open, io.Discard, 1<<20, nil)
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("error leaked the token or did not fail: %v", err)
	}
	publicAddr = func(netip.Addr) bool { return false }
	_, err = Download(context.Background(), raw, Open, io.Discard, 1<<20, nil)
	if err == nil || strings.Contains(err.Error(), "SECRET") || !strings.Contains(err.Error(), "non-public") {
		t.Fatalf("guard error: %v", err)
	}
}
