package executors

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

// relayHarness plays the gateway: it collects what the agent emits for one session.
type relayHarness struct {
	mu     sync.Mutex
	frames []*agentv1.RelayResponse
	notify chan struct{}
}

func newRelayHarness(r *Relay) *relayHarness {
	h := &relayHarness{notify: make(chan struct{}, 1024)}
	r.SetEmit(func(e *agentv1.Event) error {
		h.mu.Lock()
		h.frames = append(h.frames, e.GetRelayResponse())
		h.mu.Unlock()
		h.notify <- struct{}{}
		return nil
	})
	return h
}

func (h *relayHarness) snapshot() []*agentv1.RelayResponse {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]*agentv1.RelayResponse(nil), h.frames...)
}

// waitFor blocks until the collected frames satisfy done, or fails the test.
func (h *relayHarness) waitFor(t *testing.T, done func([]*agentv1.RelayResponse) bool) []*agentv1.RelayResponse {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		if f := h.snapshot(); done(f) {
			return f
		}
		select {
		case <-h.notify:
		case <-deadline:
			t.Fatalf("timed out; got %d frames", len(h.snapshot()))
		}
	}
}

func finished(f []*agentv1.RelayResponse) bool {
	return len(f) > 0 && (f[len(f)-1].GetEof() || f[len(f)-1].GetError() != "")
}

func head(method, path string, headers ...string) *agentv1.RelayHead {
	h := &agentv1.RelayHead{Method: method, Path: path}
	for i := 0; i+1 < len(headers); i += 2 {
		h.Headers = append(h.Headers, &agentv1.RelayHeader{Name: headers[i], Value: headers[i+1]})
	}
	return h
}

func TestRelayForwardsRequestAndAnswer(t *testing.T) {
	var gotHost, gotAuth, gotPath string
	var gotBody []byte
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost, gotAuth, gotPath = r.Host, r.Header.Get("Authorization"), r.URL.RequestURI()
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("ETag", `"abc"`)
		w.WriteHeader(201)
		_, _ = w.Write([]byte("stored"))
	}))
	defer backend.Close()

	r := NewRelay(func(id string) (string, bool) { return backend.URL, id == "buddy" })
	h := newRelayHarness(r)
	ctx := context.Background()
	r.Handle(ctx, &agentv1.RelayRequest{SessionId: "s", Seq: 1, WorkloadId: "buddy",
		Head: head("PUT", "/ownship-backups/k?partNumber=2", "host", "gateway.ownship.dev", "authorization", "AWS4 sig", "content-length", "10", "connection", "close")})
	r.Handle(ctx, &agentv1.RelayRequest{SessionId: "s", Seq: 2, Data: []byte("hello")})
	r.Handle(ctx, &agentv1.RelayRequest{SessionId: "s", Seq: 3, Data: []byte("world")})
	r.Handle(ctx, &agentv1.RelayRequest{SessionId: "s", Seq: 4, Eof: true})

	frames := h.waitFor(t, finished)
	// MinIO checks the signature against the host the caller signed, not the buddy's address.
	if gotHost != "gateway.ownship.dev" || gotAuth != "AWS4 sig" || gotPath != "/ownship-backups/k?partNumber=2" {
		t.Fatalf("forwarded host=%q auth=%q path=%q", gotHost, gotAuth, gotPath)
	}
	if string(gotBody) != "helloworld" {
		t.Fatalf("body = %q", gotBody)
	}
	var status uint32
	var etag string
	var data bytes.Buffer
	var acked uint64
	for _, f := range frames {
		if f.GetError() != "" {
			t.Fatalf("error frame: %s", f.GetError())
		}
		if f.GetHead() != nil {
			status = f.GetHead().GetStatus()
			for _, hd := range f.GetHead().GetHeaders() {
				if strings.EqualFold(hd.GetName(), "etag") {
					etag = hd.GetValue()
				}
			}
		}
		data.Write(f.GetData())
		if f.GetAcked() > acked {
			acked = f.GetAcked()
		}
	}
	if status != 201 || etag != `"abc"` || data.String() != "stored" {
		t.Fatalf("status=%d etag=%q data=%q", status, etag, data.String())
	}
	// Body frames 2 and 3 are certainly consumed; the eof may be skipped once the length is met.
	if acked < 3 {
		t.Fatalf("acked up to %d, want at least 3", acked)
	}
}

// Without credit from the gateway the agent must stop reading the object, or a slow
// restore would pile a whole backup up in Redis.
func TestRelayResponseWaitsForCredit(t *testing.T) {
	total := relayChunkBytes * (relayWindow + 10)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, total))
	}))
	defer backend.Close()

	r := NewRelay(func(string) (string, bool) { return backend.URL, true })
	h := newRelayHarness(r)
	ctx := context.Background()
	r.Handle(ctx, &agentv1.RelayRequest{SessionId: "s", Seq: 1, WorkloadId: "buddy", Head: head("GET", "/b/k")})
	r.Handle(ctx, &agentv1.RelayRequest{SessionId: "s", Seq: 2, Eof: true})

	h.waitFor(t, func(f []*agentv1.RelayResponse) bool {
		return len(f) > 0 && f[len(f)-1].GetSeq() == relayWindow
	})
	time.Sleep(150 * time.Millisecond)
	for _, f := range h.snapshot() {
		if f.GetSeq() > relayWindow {
			t.Fatalf("sent frame %d with no credit (window %d)", f.GetSeq(), relayWindow)
		}
	}

	r.Handle(ctx, &agentv1.RelayRequest{SessionId: "s", Acked: uint64(relayWindow + 20)})
	frames := h.waitFor(t, finished)
	got := 0
	for _, f := range frames {
		got += len(f.GetData())
	}
	if got != total || !frames[len(frames)-1].GetEof() {
		t.Fatalf("got %d of %d bytes, eof=%v", got, total, frames[len(frames)-1].GetEof())
	}
}

func TestRelayRefusesUnknownWorkload(t *testing.T) {
	r := NewRelay(func(string) (string, bool) { return "", false })
	h := newRelayHarness(r)
	r.Handle(context.Background(), &agentv1.RelayRequest{SessionId: "s", Seq: 1, WorkloadId: "nope", Head: head("GET", "/b")})
	frames := h.waitFor(t, finished)
	if frames[len(frames)-1].GetError() == "" {
		t.Fatal("expected an error frame")
	}
}

// The path is chosen by another homelab's agent: it must never steer the request elsewhere.
func TestRelayTargetStaysOnTheBuddy(t *testing.T) {
	for _, p := range []string{"http://169.254.169.254/latest", "b/k", ""} {
		if got, err := relayTarget("http://127.0.0.1:9000", p); err == nil {
			t.Fatalf("%q accepted as %q", p, got)
		}
	}
	for _, p := range []string{"/b/k?x=1", "//169.254.169.254/latest", "/b/a%2Fb"} {
		got, err := relayTarget("http://127.0.0.1:9000", p)
		if err != nil || !strings.HasPrefix(got, "http://127.0.0.1:9000/") {
			t.Fatalf("%q -> %q, %v", p, got, err)
		}
	}
}

func TestRelayRoundTripperAddsTokenWithoutTouchingTheSignedRequest(t *testing.T) {
	var seen http.Header
	backend := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { seen = r.Header }))
	defer backend.Close()
	req, _ := http.NewRequest("GET", backend.URL+"/b", nil)
	req.Header.Set("Authorization", "AWS4 sig")
	resp, err := relayRoundTripper{base: http.DefaultTransport, token: "tok"}.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if seen.Get("X-Ownship-Relay") != "tok" || seen.Get("Authorization") != "AWS4 sig" {
		t.Fatalf("headers = %v", seen)
	}
	if req.Header.Get("X-Ownship-Relay") != "" {
		t.Fatal("the caller's request was mutated")
	}
}
