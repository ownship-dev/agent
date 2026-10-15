package sealstream

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"testing"
)

func key() []byte {
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	return k
}

func seal(t *testing.T, k, plain []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	w, err := NewWriter(&out, k)
	if err != nil {
		t.Fatal(err)
	}
	// Odd write sizes exercise the chunk boundary handling.
	for len(plain) > 0 {
		n := min(len(plain), 10_007)
		if _, err := w.Write(plain[:n]); err != nil {
			t.Fatal(err)
		}
		plain = plain[n:]
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func open(k, sealed []byte) ([]byte, error) {
	r, err := NewReader(bytes.NewReader(sealed), k)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

func TestRoundTripAtEverySizeThatMatters(t *testing.T) {
	k := key()
	for _, size := range []int{0, 1, ChunkSize - 1, ChunkSize, ChunkSize + 1, 3 * ChunkSize, 3*ChunkSize + 77} {
		plain := make([]byte, size)
		_, _ = rand.Read(plain)
		got, err := open(k, seal(t, k, plain))
		if err != nil || !bytes.Equal(got, plain) {
			t.Fatalf("size %d: err=%v equal=%v", size, err, bytes.Equal(got, plain))
		}
	}
}

func TestRefusesTamperingTruncationAndTheWrongKey(t *testing.T) {
	k := key()
	plain := make([]byte, 3*ChunkSize+10)
	_, _ = rand.Read(plain)
	sealed := seal(t, k, plain)

	flipped := append([]byte{}, sealed...)
	flipped[headerSize+100] ^= 1
	// Dropping whole trailing chunks is the attack a plain GCM-per-chunk scheme misses.
	cases := map[string][]byte{
		"flipped bit":         flipped,
		"truncated to chunks": sealed[:headerSize+2*sealedChunk],
		"cut mid-chunk":       sealed[:len(sealed)-5],
		"not a sealed stream": []byte("plain tar data, not sealed"),
	}
	for name, data := range cases {
		if _, err := open(k, data); !errors.Is(err, ErrTampered) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if _, err := open(key(), sealed); !errors.Is(err, ErrTampered) {
		t.Errorf("wrong key: err = %v", err)
	}
}

func TestSealedOutputHidesThePlaintext(t *testing.T) {
	plain := bytes.Repeat([]byte("postgres password=hunter2 "), 1000)
	if bytes.Contains(seal(t, key(), plain), []byte("hunter2")) {
		t.Fatal("plaintext visible in the sealed stream")
	}
}
