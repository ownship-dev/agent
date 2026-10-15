package tsingress

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// fakeNode stands in for a tailnet node: Listen hands back a loopback listener.
type fakeNode struct {
	authKey   string
	domains   []string
	upErr     error
	listeners map[string]net.Listener
	loggedOut bool
	closed    bool
}

func (f *fakeNode) Up(context.Context) error { return f.upErr }
func (f *fakeNode) CertDomains() []string    { return f.domains }
func (f *fakeNode) Listen(_, addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	f.listeners[addr] = ln
	return ln, err
}
func (f *fakeNode) ListenTLS(network, addr string) (net.Listener, error) {
	return f.Listen(network, addr)
}
func (f *fakeNode) Logout(context.Context) error { f.loggedOut = true; return nil }
func (f *fakeNode) Close() error                 { f.closed = true; return nil }

func withFakes(i *Ingress, domains []string) *[]*fakeNode {
	made := &[]*fakeNode{}
	i.newNode = func(_, _, authKey string) node {
		f := &fakeNode{authKey: authKey, domains: domains, listeners: map[string]net.Listener{}}
		*made = append(*made, f)
		return f
	}
	return made
}

func get(t *testing.T, ln net.Listener) string {
	t.Helper()
	res, err := http.Get("http://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	return string(b)
}

func app(body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }))
}

func TestEnsureProxiesAndReportsTheTailnetURL(t *testing.T) {
	v1, v2 := app("v1"), app("v2")
	defer v1.Close()
	defer v2.Close()
	i := New(t.TempDir())
	made := withFakes(i, []string{"myapp.tail1234.ts.net"})

	url, err := i.Ensure(context.Background(), "wl1", "myapp", "tskey-auth-x", v1.URL)
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://myapp.tail1234.ts.net" {
		t.Fatalf("url = %s", url)
	}
	n := (*made)[0]
	if n.authKey != "tskey-auth-x" || get(t, n.listeners[":80"]) != "v1" {
		t.Fatal("node not started with the key or not proxying")
	}

	// A redeploy retargets the same node instead of creating a second device.
	if _, err := i.Ensure(context.Background(), "wl1", "myapp", "", v2.URL); err != nil {
		t.Fatal(err)
	}
	if len(*made) != 1 || get(t, n.listeners[":443"]) != "v2" {
		t.Fatal("redeploy did not retarget the existing node")
	}
}

func TestPlainHTTPWithoutTailnetCertificates(t *testing.T) {
	i := New(t.TempDir())
	withFakes(i, nil)
	url, err := i.Ensure(context.Background(), "wl1", "myapp", "k", "http://127.0.0.1:1")
	if err != nil || url != "http://myapp" {
		t.Fatalf("url=%s err=%v", url, err)
	}
}

func TestDropLogsOutAndForgets(t *testing.T) {
	dir := t.TempDir()
	i := New(dir)
	made := withFakes(i, nil)
	if _, err := i.Ensure(context.Background(), "wl1", "myapp", "k", "http://127.0.0.1:1"); err != nil {
		t.Fatal(err)
	}
	if err := i.Drop(context.Background(), "wl1"); err != nil {
		t.Fatal(err)
	}
	n := (*made)[0]
	if !n.loggedOut || !n.closed || i.Has("wl1") {
		t.Fatal("node not removed from the tailnet")
	}
	if _, err := os.Stat(filepath.Join(dir, "wl1")); !os.IsNotExist(err) {
		t.Fatal("node identity left on disk")
	}
	if New(dir).Has("wl1") {
		t.Fatal("dropped node would come back after a restart")
	}
}

func TestResumeRestoresWithoutAnAuthKey(t *testing.T) {
	dir := t.TempDir()
	first := New(dir)
	withFakes(first, nil)
	if _, err := first.Ensure(context.Background(), "wl1", "myapp", "k", "http://127.0.0.1:1"); err != nil {
		t.Fatal(err)
	}

	again := New(dir)
	made := withFakes(again, nil)
	again.Resume(context.Background(), func(string) {})
	if len(*made) != 1 || (*made)[0].authKey != "" {
		t.Fatal("resume should reuse the stored identity, not an auth key")
	}
}

func TestRejectsBadInputAndFailedLogin(t *testing.T) {
	i := New(t.TempDir())
	made := withFakes(i, nil)
	if _, err := i.Ensure(context.Background(), "wl1", "Bad_Name", "k", "http://127.0.0.1:1"); err == nil {
		t.Fatal("invalid hostname accepted")
	}
	i.newNode = func(_, _, _ string) node {
		f := &fakeNode{upErr: errors.New("invalid key"), listeners: map[string]net.Listener{}}
		*made = append(*made, f)
		return f
	}
	if _, err := i.Ensure(context.Background(), "wl1", "myapp", "k", "http://127.0.0.1:1"); err == nil || !(*made)[0].closed {
		t.Fatal("failed login must error and close the node")
	}
}
