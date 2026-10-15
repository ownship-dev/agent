package executors

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"testing"

	"github.com/minio/minio-go/v7"
	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

// fakeGateway does what the gateway's relay does, in process: one HTTP request becomes
// one session of frames to the Relay, and its frames become the HTTP response.
type fakeGateway struct {
	relay *Relay
	mu    sync.Mutex
	next  int
	subs  map[string]chan *agentv1.RelayResponse
}

func newFakeGateway(relay *Relay) *fakeGateway {
	g := &fakeGateway{relay: relay, subs: map[string]chan *agentv1.RelayResponse{}}
	relay.SetEmit(func(e *agentv1.Event) error {
		r := e.GetRelayResponse()
		g.mu.Lock()
		ch := g.subs[r.GetSessionId()]
		g.mu.Unlock()
		if ch != nil {
			ch <- r
		}
		return nil
	})
	return g
}

func (g *fakeGateway) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Header.Get("X-Ownship-Relay") == "" {
		http.Error(w, "no relay token", http.StatusForbidden)
		return
	}
	ctx := req.Context()
	g.mu.Lock()
	g.next++
	id := "s" + strconv.Itoa(g.next)
	ch := make(chan *agentv1.RelayResponse, 4096)
	g.subs[id] = ch
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		delete(g.subs, id)
		g.mu.Unlock()
	}()

	h := &agentv1.RelayHead{Method: req.Method, Path: req.URL.RequestURI()}
	h.Headers = append(h.Headers, &agentv1.RelayHeader{Name: "host", Value: req.Host})
	for name, values := range req.Header {
		if name == "X-Ownship-Relay" {
			continue
		}
		for _, v := range values {
			h.Headers = append(h.Headers, &agentv1.RelayHeader{Name: name, Value: v})
		}
	}
	if req.ContentLength >= 0 && (req.ContentLength > 0 || req.Method == http.MethodPut || req.Method == http.MethodPost) {
		h.Headers = append(h.Headers, &agentv1.RelayHeader{Name: "content-length", Value: strconv.FormatInt(req.ContentLength, 10)})
	}
	seq := uint64(1)
	g.relay.Handle(ctx, &agentv1.RelayRequest{SessionId: id, Seq: seq, WorkloadId: "buddy", Head: h})

	var acked uint64
	credit := make(chan struct{}, 1)
	var amu sync.Mutex
	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, err := req.Body.Read(buf)
			if n > 0 {
				for {
					amu.Lock()
					ok := seq-acked < relayWindow
					amu.Unlock()
					if ok {
						break
					}
					select {
					case <-credit:
					case <-ctx.Done():
						return
					}
				}
				seq++
				g.relay.Handle(ctx, &agentv1.RelayRequest{SessionId: id, Seq: seq, Data: append([]byte(nil), buf[:n]...)})
			}
			if err != nil {
				seq++
				g.relay.Handle(ctx, &agentv1.RelayRequest{SessionId: id, Seq: seq, Eof: true})
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			g.relay.Handle(context.Background(), &agentv1.RelayRequest{SessionId: id, Abort: true})
			return
		case f := <-ch:
			if a := f.GetAcked(); a > 0 {
				amu.Lock()
				if a > acked {
					acked = a
				}
				amu.Unlock()
				select {
				case credit <- struct{}{}:
				default:
				}
			}
			if f.GetError() != "" {
				http.Error(w, f.GetError(), http.StatusBadGateway)
				return
			}
			if hd := f.GetHead(); hd != nil {
				for _, x := range hd.GetHeaders() {
					w.Header().Add(x.GetName(), x.GetValue())
				}
				w.WriteHeader(int(hd.GetStatus()))
			}
			if len(f.GetData()) > 0 {
				_, _ = w.Write(f.GetData())
			}
			if f.GetSeq() > 0 {
				g.relay.Handle(ctx, &agentv1.RelayRequest{SessionId: id, Acked: f.GetSeq()})
			}
			if f.GetEof() {
				return
			}
		}
	}
}

// Runs minio-go through the relay against a real MinIO: this is what proves the SigV4
// signature survives the hop. Opt-in: set RELAY_E2E_MINIO (and _USER/_PASSWORD).
func TestRelayEndToEndAgainstMinIO(t *testing.T) {
	minioURL := os.Getenv("RELAY_E2E_MINIO")
	if minioURL == "" {
		t.Skip("RELAY_E2E_MINIO not set")
	}
	relay := NewRelay(func(string) (string, bool) { return minioURL, true })
	gw := httptest.NewServer(newFakeGateway(relay))
	defer gw.Close()

	d := &Deployer{controlPlaneURL: gw.URL}
	target := &agentv1.S3Target{
		Region: "us-east-1", Bucket: "ownship-backups", ForcePathStyle: true, EnsureBucket: true,
		AccessKeyId: os.Getenv("RELAY_E2E_USER"), SecretAccessKey: os.Getenv("RELAY_E2E_PASSWORD"),
		RelayToken: "test-token",
	}
	client, err := d.s3Client(target)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := ensureBucket(ctx, client, target); err != nil {
		t.Fatalf("ensure bucket: %v", err)
	}

	// Three 16 MiB parts and a tail, streamed with no known length like a real backup.
	want := make([]byte, 50<<20+123)
	_, _ = rand.Read(want)
	if _, err := client.PutObject(ctx, target.GetBucket(), "e2e/object", io.NopCloser(bytes.NewReader(want)), -1,
		minio.PutObjectOptions{ContentType: "application/octet-stream", PartSize: 16 * 1024 * 1024}); err != nil {
		t.Fatalf("put: %v", err)
	}
	obj, err := client.GetObject(ctx, target.GetBucket(), "e2e/object", minio.GetObjectOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	got, err := io.ReadAll(obj)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("round trip differs: got %d bytes, want %d", len(got), len(want))
	}
	if err := client.RemoveObject(ctx, target.GetBucket(), "e2e/object", minio.RemoveObjectOptions{}); err != nil {
		t.Fatalf("remove: %v", err)
	}

	// A wrong secret must be refused by MinIO itself: the relay adds no authority.
	bad, _ := d.s3Client(&agentv1.S3Target{Region: "us-east-1", Bucket: "ownship-backups", ForcePathStyle: true,
		AccessKeyId: target.GetAccessKeyId(), SecretAccessKey: "wrong", RelayToken: "test-token"})
	if _, err := bad.StatObject(ctx, target.GetBucket(), "e2e/object", minio.StatObjectOptions{}); err == nil {
		t.Fatal("a wrong secret was accepted")
	}
}
