package linkbridge

import (
	"context"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// echoServer answers every connection with msg, then closes.
func echoServer(t *testing.T, msg string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte(msg))
			c.Close()
		}
	}()
	return ln.Addr().String()
}

func readFrom(t *testing.T, port uint16) string {
	t.Helper()
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	b, _ := io.ReadAll(c)
	return string(b)
}

func allowAll(Key, net.Addr) bool { return true }

func TestBridgeForwardsAndReResolves(t *testing.T) {
	up1, up2 := echoServer(t, "one"), echoServer(t, "two")
	var target atomic.Value
	target.Store(up1)
	b := New(filepath.Join(t.TempDir(), "lb.json"),
		func(context.Context, string, uint32) (string, error) { return target.Load().(string), nil }, allowAll)
	defer b.Close()
	port, err := b.Ensure(Key{Source: "127.0.0.1", Target: "db", Port: 5432})
	if err != nil {
		t.Fatal(err)
	}
	if got := readFrom(t, port); got != "one" {
		t.Fatalf("got %q", got)
	}
	target.Store(up2) // the target was redeployed on a new address
	if got := readFrom(t, port); got != "two" {
		t.Fatalf("got %q", got)
	}
}

func TestBridgeEnsureIsIdempotent(t *testing.T) {
	b := New(filepath.Join(t.TempDir(), "lb.json"), nil, allowAll)
	defer b.Close()
	k := Key{Source: "127.0.0.1", Target: "db", Port: 5432}
	p1, _ := b.Ensure(k)
	p2, _ := b.Ensure(k)
	if p1 == 0 || p1 != p2 {
		t.Fatalf("ports %d %d", p1, p2)
	}
}

func TestBridgePortStableAcrossRestart(t *testing.T) {
	p := filepath.Join(t.TempDir(), "lb.json")
	dial := func(context.Context, string, uint32) (string, error) { return "127.0.0.1:1", nil }
	b := New(p, dial, allowAll)
	port, err := b.Ensure(Key{Source: "127.0.0.1", Target: "db", Port: 5432})
	if err != nil {
		t.Fatal(err)
	}
	b.Close()
	b2 := New(p, dial, allowAll)
	defer b2.Close()
	if err := b2.Resume(); err != nil {
		t.Fatal(err)
	}
	again, _ := b2.Ensure(Key{Source: "127.0.0.1", Target: "db", Port: 5432})
	if again != port {
		t.Fatalf("port changed %d -> %d", port, again)
	}
}

func TestBridgeRefusesDisallowedPeers(t *testing.T) {
	up := echoServer(t, "secret")
	b := New(filepath.Join(t.TempDir(), "lb.json"),
		func(context.Context, string, uint32) (string, error) { return up, nil },
		func(Key, net.Addr) bool { return false })
	defer b.Close()
	port, _ := b.Ensure(Key{Source: "127.0.0.1", Target: "db", Port: 5432})
	if got := readFrom(t, port); got != "" {
		t.Fatalf("a disallowed peer got %q", got)
	}
}

func TestBridgeNeverBindsWildcardOrNonsense(t *testing.T) {
	b := New(filepath.Join(t.TempDir(), "lb.json"), nil, allowAll)
	defer b.Close()
	for _, src := range []string{"0.0.0.0", "::", "", "not-an-ip"} {
		if _, err := b.Ensure(Key{Source: src, Target: "db", Port: 1}); err == nil {
			t.Errorf("source %q must be refused", src)
		}
	}
}

func TestDropTargetClosesItsListeners(t *testing.T) {
	b := New(filepath.Join(t.TempDir(), "lb.json"), nil, allowAll)
	defer b.Close()
	port, _ := b.Ensure(Key{Source: "127.0.0.1", Target: "db", Port: 5432})
	keep, _ := b.Ensure(Key{Source: "127.0.0.1", Target: "other", Port: 80})
	b.DropWorkload("db")
	if _, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second); err == nil {
		t.Fatal("dropped target still listening")
	}
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", keep), time.Second)
	if err != nil {
		t.Fatal("other targets must stay up")
	}
	c.Close()
}

func TestDropWorkloadClosesLinksFromAndToIt(t *testing.T) {
	b := New(filepath.Join(t.TempDir(), "lb.json"), nil, allowAll)
	defer b.Close()
	from, _ := b.Ensure(Key{Source: "127.0.0.1", From: "app", Target: "db", Port: 5432})
	to, _ := b.Ensure(Key{Source: "127.0.0.1", From: "other", Target: "app", Port: 80})
	keep, _ := b.Ensure(Key{Source: "127.0.0.1", From: "other", Target: "db", Port: 5432})
	b.DropWorkload("app")
	for _, p := range []uint16{from, to} {
		if _, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", p), time.Second); err == nil {
			t.Fatalf("port %d still listening", p)
		}
	}
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", keep), time.Second)
	if err != nil {
		t.Fatal("unrelated links stay up")
	}
	c.Close()
}
