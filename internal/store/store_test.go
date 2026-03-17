package store_test

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"p2p-ci/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := store.New(dir, log)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return s
}

func TestPutAndGet_RoundTrip(t *testing.T) {
	s := newTestStore(t)

	content := "hello, p2p-ci cache world"
	key := "https://registry.npmjs.org/lodash/-/lodash-1.0.0.tgz"

	hash, err := s.Put(key, "application/octet-stream", strings.NewReader(content))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if hash == "" {
		t.Fatal("Put returned empty hash")
	}

	r, entry, err := s.Get(key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if r == nil {
		t.Fatal("Get returned nil reader (cache miss)")
	}
	defer r.Close()

	got, _ := io.ReadAll(r)
	if string(got) != content {
		t.Fatalf("content mismatch: got %q want %q", got, content)
	}
	if entry.Hash != hash {
		t.Fatalf("entry hash mismatch: %q vs %q", entry.Hash, hash)
	}
}

func TestGet_Miss(t *testing.T) {
	s := newTestStore(t)

	r, entry, err := s.Get("https://does.not.exist/pkg.tgz")
	if err != nil {
		t.Fatalf("unexpected error on miss: %v", err)
	}
	if r != nil || entry != nil {
		t.Fatal("expected nil reader and entry on miss")
	}
}

// TestTamperDetection verifies that a store object whose bytes have been modified
// on disk is detected, deleted, and treated as a cache miss rather than
// serving poisoned content.
func TestTamperDetection(t *testing.T) {
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, _ := store.New(dir, log)

	key := "https://registry.npmjs.org/safe-pkg/-/safe-pkg-1.0.0.tgz"
	original := bytes.Repeat([]byte("safe content "), 100)

	hash, err := s.Put(key, "application/octet-stream", bytes.NewReader(original))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Tamper with the object on disk.
	objPath := dir + "/objects/" + hash[:2] + "/" + hash
	f, err := os.OpenFile(objPath, os.O_WRONLY, 0600)
	if err != nil {
		t.Fatalf("open object for tampering: %v", err)
	}
	f.WriteAt([]byte("MALICIOUS"), 0)
	f.Close()

	// Get must detect the tamper and return an error.
	r, _, err := s.Get(key)
	if err == nil {
		t.Fatal("expected hash mismatch error, got nil")
	}
	if r != nil {
		r.Close()
		t.Fatal("expected nil reader after tamper detection")
	}
	if !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("expected 'hash mismatch' in error, got: %v", err)
	}

	// The object should be gone from disk and index.
	if _, statErr := os.Stat(objPath); !os.IsNotExist(statErr) {
		t.Fatal("tampered object should have been deleted from disk")
	}

	// Subsequent Get should be a clean miss (not an error).
	r2, _, err2 := s.Get(key)
	if err2 != nil {
		t.Fatalf("second Get after tamper should be clean miss, got error: %v", err2)
	}
	if r2 != nil {
		r2.Close()
		t.Fatal("second Get after tamper should return nil (miss)")
	}
}

func TestAtomicWrite_NoCorrruptionOnPartialWrite(t *testing.T) {
	s := newTestStore(t)
	key := "https://registry.npmjs.org/atomic-test/-/1.0.0.tgz"

	// Simulate a reader that errors partway through.
	errReader := io.MultiReader(
		strings.NewReader("partial content here"),
		errorReader{},
	)

	_, err := s.Put(key, "application/octet-stream", errReader)
	if err == nil {
		t.Fatal("expected error from partial write, got nil")
	}

	// Should be a clean miss — no corrupt entry.
	r, entry, err := s.Get(key)
	if err != nil {
		t.Fatalf("Get after failed Put: %v", err)
	}
	if r != nil || entry != nil {
		r.Close()
		t.Fatal("partial write should not leave a cached entry")
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) {
	return 0, io.ErrUnexpectedEOF
}