// Package linkbridge forwards private links between engines on one host, where containers
// can't resolve each other by name.
package linkbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// Key is one forwarded link: where the source reaches the bridge, the only workload admitted,
// and the target.
type Key struct {
	Source string `json:"source"`
	From   string `json:"from"`
	Target string `json:"target"`
	Port   uint32 `json:"port"`
}

func (k Key) id() string {
	return k.Source + "|" + k.From + "|" + k.Target + "|" + strconv.FormatUint(uint64(k.Port), 10)
}

// Dialer resolves the target per connection, so a redeploy never breaks a link.
type Dialer func(ctx context.Context, targetWorkloadID string, port uint32) (string, error)

type Bridge struct {
	path  string
	dial  Dialer
	allow func(Key, net.Addr) bool
	mu    sync.Mutex
	ports map[string]uint16
	keys  map[string]Key
	lns   map[string]net.Listener
}

type persisted struct {
	Keys  map[string]Key    `json:"keys"`
	Ports map[string]uint16 `json:"ports"`
}

func New(path string, dial Dialer, allow func(Key, net.Addr) bool) *Bridge {
	b := &Bridge{path: path, dial: dial, allow: allow,
		ports: map[string]uint16{}, keys: map[string]Key{}, lns: map[string]net.Listener{}}
	if data, err := os.ReadFile(path); err == nil {
		var p persisted
		if json.Unmarshal(data, &p) == nil {
			for k, v := range p.Ports {
				b.ports[k] = v
			}
			for k, v := range p.Keys {
				b.keys[k] = v
			}
		}
	}
	return b
}

func (b *Bridge) Resume() error {
	b.mu.Lock()
	keys := make([]Key, 0, len(b.keys))
	for _, k := range b.keys {
		keys = append(keys, k)
	}
	b.mu.Unlock()
	var errs []error
	for _, k := range keys {
		if _, err := b.Ensure(k); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Ensure returns the link's port, stable across agent restarts.
func (b *Bridge) Ensure(k Key) (uint16, error) {
	ip := net.ParseIP(k.Source)
	if ip == nil || ip.IsUnspecified() {
		return 0, fmt.Errorf("link bridge needs a concrete listen address, got %q", k.Source)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	id := k.id()
	if ln, ok := b.lns[id]; ok {
		return uint16(ln.Addr().(*net.TCPAddr).Port), nil
	}
	ln, err := listen(net.JoinHostPort(k.Source, strconv.Itoa(int(b.ports[id]))))
	if err != nil && b.ports[id] != 0 {
		// The old port was taken while the agent was down; the next deploy injects the new one.
		ln, err = listen(net.JoinHostPort(k.Source, "0"))
	}
	if err != nil {
		return 0, err
	}
	port := uint16(ln.Addr().(*net.TCPAddr).Port)
	b.lns[id], b.ports[id], b.keys[id] = ln, port, k
	if err := b.flush(); err != nil {
		ln.Close()
		delete(b.lns, id)
		return 0, err
	}
	go b.serve(k, ln)
	return port, nil
}

// listen binds before the address exists: Podman creates a network's gateway IP only when a
// container attaches.
func listen(addr string) (net.Listener, error) {
	lc := net.ListenConfig{Control: freebind}
	return lc.Listen(context.Background(), "tcp", addr)
}

func (b *Bridge) serve(k Key, ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go b.handle(k, c)
	}
}

func (b *Bridge) handle(k Key, c net.Conn) {
	defer c.Close()
	if b.dial == nil || b.allow == nil || !b.allow(k, c.RemoteAddr()) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	addr, err := b.dial(ctx, k.Target, k.Port)
	cancel()
	if err != nil {
		return
	}
	up, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return
	}
	defer up.Close()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, c); closeWrite(up); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, up); closeWrite(c); done <- struct{}{} }()
	<-done
	<-done
}

func closeWrite(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}
}

func (b *Bridge) DropWorkload(workloadID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, k := range b.keys {
		if k.Target != workloadID && k.From != workloadID {
			continue
		}
		if ln, ok := b.lns[id]; ok {
			ln.Close()
		}
		delete(b.lns, id)
		delete(b.keys, id)
		delete(b.ports, id)
	}
	_ = b.flush()
}

func (b *Bridge) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, ln := range b.lns {
		ln.Close()
		delete(b.lns, id)
	}
}

func (b *Bridge) flush() error {
	if err := os.MkdirAll(filepath.Dir(b.path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(persisted{Keys: b.keys, Ports: b.ports})
	if err != nil {
		return err
	}
	tmp := b.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, b.path)
}
