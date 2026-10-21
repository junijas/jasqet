package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// A piece whose size the playlist doesn't give stops at the cap, at once and
// without the usual retries, whether the server sends endless data or says
// up front that it's too big.
func TestStreamPieceWithoutSizeStopsAtTheCap(t *testing.T) {
	old := segCap
	segCap = 1 << 20
	defer func() { segCap = old }()
	var reqs atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs.Add(1)
		if r.URL.Path == "/sized" {
			w.Header().Set("Content-Length", strconv.Itoa(8<<20))
		}
		buf := make([]byte, 64<<10)
		for i := 0; i < 1<<14; i++ { // up to 1 GB, more than enough to never end
			if _, err := w.Write(buf); err != nil {
				return
			}
		}
	}))
	defer srv.Close()
	m := newTestManager(t)
	dir := t.TempDir()
	for _, path := range []string{"/endless", "/sized"} {
		reqs.Store(0)
		d := &Download{segState: make([]byte, 1)}
		task := segTask{path: filepath.Join(dir, "t0_000001"), ref: SegRef{URL: srv.URL + path}}
		start := time.Now()
		err := m.fetchSegmentTask(context.Background(), d, 0, task, &keyCache{}, reqOpts{})
		if !errors.Is(err, errSegTooBig) {
			t.Fatalf("%s: want the too big error, got %v", path, err)
		}
		if n := reqs.Load(); n != 1 || time.Since(start) > 20*time.Second {
			t.Fatalf("%s: %d requests in %s, want one try only", path, n, time.Since(start))
		}
		for _, f := range []string{task.path, task.path + ".tmp"} {
			if _, err := os.Stat(f); err == nil {
				t.Fatalf("%s: %s was left behind", path, filepath.Base(f))
			}
		}
	}
}

// A single-file stream (one piece for the whole video, size not given) is
// the whole video, so it has no cap.
func TestSingleFileStreamHasNoCap(t *testing.T) {
	old := segCap
	segCap = 1 << 20
	defer func() { segCap = old }()
	data := make([]byte, 3<<20)
	rand.New(rand.NewSource(7)).Read(data)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for b := data; len(b) > 0; b = b[min(len(b), 64<<10):] {
			w.Write(b[:min(len(b), 64<<10)]) // no length given
		}
	}))
	defer srv.Close()
	m := newTestManager(t)
	task := segTask{path: filepath.Join(t.TempDir(), "t0_000000"), ref: SegRef{URL: srv.URL + "/movie.ts"}, whole: true}
	d := &Download{segState: make([]byte, 1)}
	if err := m.fetchSegmentTask(context.Background(), d, 0, task, &keyCache{}, reqOpts{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(task.path); !bytes.Equal(got, data) {
		t.Fatal("the file differs")
	}
}

// Checking a chunk reads it whole, so one bigger than any real piece (only a
// single-file stream can be) isn't checked that way.
func TestBigChunksAreNotReadWholeToCheckThem(t *testing.T) {
	old := segCap
	segCap = 1 << 10
	defer func() { segCap = old }()
	dir := t.TempDir()
	page := []byte("<html><body>link expired</body></html>")
	small := filepath.Join(dir, "small")
	os.WriteFile(small, page, 0o644)
	if err := checkSizeless(small, true); !errors.Is(err, errBadChunk) {
		t.Fatalf("a small error page should still be caught, got %v", err)
	}
	big := filepath.Join(dir, "big")
	os.WriteFile(big, append(page, make([]byte, 2<<10)...), 0o644)
	if err := checkSizeless(big, true); err != nil || !fileExists(big) {
		t.Fatalf("a chunk over the cap should be left as it is, got %v", err)
	}
}

// Encrypted chunks are decrypted piece by piece, never read whole, and come
// out the same as before.
func TestEncryptedChunksAreDecryptedPieceByPiece(t *testing.T) {
	key, iv := []byte("0123456789abcdef"), []byte("fedcba9876543210")
	plain := make([]byte, 2<<20+1000) // over two of its 1 MB pieces
	rand.New(rand.NewSource(8)).Read(plain)
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	padded := append(append([]byte(nil), plain...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	block, _ := aes.NewCipher(key)
	enc := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(enc, padded)
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "chunk.tmp"), filepath.Join(dir, "chunk.dec")
	os.WriteFile(src, enc, 0o644)
	if err := decryptFileAES128(src, dst, key, iv); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dst); !bytes.Equal(got, plain) {
		t.Fatal("the decrypted chunk differs")
	}
	os.WriteFile(src, enc[:len(enc)-3], 0o644)
	if err := decryptFileAES128(src, dst, key, iv); !errors.Is(err, errEncSize) {
		t.Fatalf("a cut-off chunk should have the wrong size, got %v", err)
	}
}
