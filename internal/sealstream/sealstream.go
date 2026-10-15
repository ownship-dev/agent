// Package sealstream encrypts a stream with AES-256-GCM in chunks (STREAM construction), so
// a reordered, altered or truncated stream fails to open.
package sealstream

import (
	"bufio"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	ChunkSize   = 64 << 10
	prefixSize  = 7
	magic       = "OSB1"
	headerSize  = len(magic) + prefixSize
	sealedChunk = ChunkSize + 16
)

var ErrTampered = errors.New("backup is corrupt or was tampered with")

func aead(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("backup key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func nonce(prefix []byte, counter uint32, last bool) []byte {
	n := make([]byte, 12)
	copy(n, prefix)
	binary.BigEndian.PutUint32(n[prefixSize:], counter)
	if last {
		n[11] = 1
	}
	return n
}

// Writer seals what is written to it; Close writes the final chunk and must be called.
type Writer struct {
	w       io.Writer
	aead    cipher.AEAD
	prefix  []byte
	buf     []byte
	counter uint32
	closed  bool
}

func NewWriter(w io.Writer, key []byte) (*Writer, error) {
	a, err := aead(key)
	if err != nil {
		return nil, err
	}
	prefix := make([]byte, prefixSize)
	if _, err := rand.Read(prefix); err != nil {
		return nil, err
	}
	if _, err := w.Write(append([]byte(magic), prefix...)); err != nil {
		return nil, err
	}
	return &Writer{w: w, aead: a, prefix: prefix, buf: make([]byte, 0, ChunkSize)}, nil
}

func (s *Writer) Write(p []byte) (int, error) {
	if s.closed {
		return 0, errors.New("sealstream: write after close")
	}
	n := len(p)
	for len(p) > 0 {
		take := min(ChunkSize-len(s.buf), len(p))
		s.buf = append(s.buf, p[:take]...)
		p = p[take:]
		// Hold a full chunk until more data arrives, so the last chunk gets the final flag.
		if len(s.buf) == ChunkSize && len(p) > 0 {
			if err := s.flush(false); err != nil {
				return 0, err
			}
		}
	}
	return n, nil
}

func (s *Writer) flush(last bool) error {
	if s.counter == ^uint32(0) {
		return errors.New("sealstream: stream too long")
	}
	sealed := s.aead.Seal(nil, nonce(s.prefix, s.counter, last), s.buf, nil)
	s.counter++
	s.buf = s.buf[:0]
	_, err := s.w.Write(sealed)
	return err
}

func (s *Writer) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	if len(s.buf) == ChunkSize {
		// Data ended on a boundary: seal it, then an empty final chunk.
		if err := s.flush(false); err != nil {
			return err
		}
	}
	return s.flush(true)
}

// Reader opens a sealed stream, failing with ErrTampered on any mismatch.
type Reader struct {
	r       *bufio.Reader
	aead    cipher.AEAD
	prefix  []byte
	counter uint32
	plain   []byte
	done    bool
}

func NewReader(r io.Reader, key []byte) (*Reader, error) {
	a, err := aead(key)
	if err != nil {
		return nil, err
	}
	br := bufio.NewReaderSize(r, sealedChunk+1)
	header := make([]byte, headerSize)
	if _, err := io.ReadFull(br, header); err != nil || string(header[:len(magic)]) != magic {
		return nil, ErrTampered
	}
	return &Reader{r: br, aead: a, prefix: header[len(magic):]}, nil
}

func (s *Reader) Read(p []byte) (int, error) {
	for len(s.plain) == 0 {
		if s.done {
			return 0, io.EOF
		}
		if err := s.next(); err != nil {
			return 0, err
		}
	}
	n := copy(p, s.plain)
	s.plain = s.plain[n:]
	return n, nil
}

func (s *Reader) next() error {
	chunk := make([]byte, sealedChunk)
	n, err := io.ReadFull(s.r, chunk)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return err
	}
	chunk = chunk[:n]
	// The last chunk is the one with nothing after it.
	_, peekErr := s.r.Peek(1)
	last := errors.Is(peekErr, io.EOF)
	if !last && n < sealedChunk {
		return ErrTampered
	}
	plain, openErr := s.aead.Open(nil, nonce(s.prefix, s.counter, last), chunk, nil)
	if openErr != nil {
		return ErrTampered
	}
	s.counter++
	s.plain = plain
	s.done = last
	return nil
}
