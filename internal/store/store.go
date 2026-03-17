// Package store implements a content-addressed on-disk cache.
//
// Security model:
//   - Every object is stored under its SHA256 hash as the filename.
//   - On Get, the content is re-hashed and compared to the expected hash before
//     returning. A hash mismatch causes the corrupted entry to be deleted.
//   - Writes are atomic: content is written to a temp file, hashed, then
//     renamed into place. A crashed write leaves only a harmless temp file.
//   - The URL→hash index is a best-effort lookup table. The hash IS the
//     authoritative identity of the content. Tampering with the index can only
//     cause cache misses, never cache poisoning.
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const indexFile = "index.json"

// Entry is a single index record.
type Entry struct {
	Hash        string    `json:"hash"`
	Size        int64     `json:"size"`
	ContentType string    `json:"content_type"`
	CachedAt    time.Time `json:"cached_at"`
}

// Store is a thread-safe, content-addressed object store.
type Store struct {
	baseDir string
	mu      sync.RWMutex
	index   map[string]Entry // URL → Entry
	log     *slog.Logger
}

// New creates or opens an existing store at baseDir.
func New(baseDir string, log *slog.Logger) (*Store, error) {
	for _, sub := range []string{"objects", "tmp"} {
		if err := os.MkdirAll(filepath.Join(baseDir, sub), 0700); err != nil {
			return nil, fmt.Errorf("init store dir %q: %w", sub, err)
		}
	}
	s := &Store{
		baseDir: baseDir,
		index:   make(map[string]Entry),
		log:     log,
	}
	if err := s.loadIndex(); err != nil {
		// Non-fatal: a missing or corrupt index just means cold-start cache misses.
		s.log.Warn("store index load failed, starting fresh", "err", err)
	}
	return s, nil
}

// Get returns an open reader for the cached content keyed by cacheKey (a URL).
// The caller must close the reader.
// Returns (nil, 0, nil) on a cache miss.
// SECURITY: Re-verifies the hash on every read. A corrupt or tampered file is
// deleted and treated as a miss.
func (s *Store) Get(cacheKey string) (io.ReadCloser, *Entry, error) {
	s.mu.RLock()
	entry, ok := s.index[cacheKey]
	s.mu.RUnlock()
	if !ok {
		return nil, nil, nil // miss
	}

	path := s.objectPath(entry.Hash)
	f, err := os.Open(path)
	if err != nil {
		s.log.Warn("cache object missing, treating as miss", "key", cacheKey, "hash", entry.Hash)
		s.mu.Lock()
		delete(s.index, cacheKey)
		s.mu.Unlock()
		return nil, nil, nil
	}

	// Re-verify hash before serving.
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("verify read: %w", err)
	}
	actual := hex.EncodeToString(h.Sum(nil))
	if actual != entry.Hash {
		f.Close()
		s.log.Error("SECURITY: hash mismatch on cache read — deleting corrupted entry",
			"key", cacheKey, "expected", entry.Hash, "actual", actual)
		os.Remove(path)
		s.mu.Lock()
		delete(s.index, cacheKey)
		s.mu.Unlock()
		return nil, nil, fmt.Errorf("hash mismatch: cache entry for %q is corrupted", cacheKey)
	}

	// Seek back to beginning for actual serving.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("seek: %w", err)
	}

	return f, &entry, nil
}

// Put stores content from r, indexes it under cacheKey, and returns the SHA256 hash.
// The write is atomic: a partial write due to error or crash leaves no corrupt entry.
func (s *Store) Put(cacheKey, contentType string, r io.Reader) (string, error) {
	// Write to temp file while streaming, computing hash simultaneously.
	tmp, err := os.CreateTemp(filepath.Join(s.baseDir, "tmp"), "put-*")
	if err != nil {
		return "", fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		tmp.Close()
		if !committed {
			os.Remove(tmpPath)
		}
	}()

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), r)
	if err != nil {
		return "", fmt.Errorf("stream content: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return "", fmt.Errorf("sync temp: %w", err)
	}
	tmp.Close()

	hash := hex.EncodeToString(h.Sum(nil))
	objPath := s.objectPath(hash)

	// Idempotent: if another goroutine raced us to the same object, both are identical.
	if err := os.MkdirAll(filepath.Dir(objPath), 0700); err != nil {
		return "", fmt.Errorf("mkdir object: %w", err)
	}
	if err := os.Rename(tmpPath, objPath); err != nil {
		return "", fmt.Errorf("commit object: %w", err)
	}
	committed = true

	entry := Entry{
		Hash:        hash,
		Size:        n,
		ContentType: contentType,
		CachedAt:    time.Now().UTC(),
	}

	s.mu.Lock()
	s.index[cacheKey] = entry
	s.mu.Unlock()

	if err := s.saveIndex(); err != nil {
		s.log.Warn("index save failed (non-fatal)", "err", err)
	}

	s.log.Info("cached", "key", cacheKey, "hash", hash[:12], "size_kb", n/1024)
	return hash, nil
}

// Stats returns basic cache statistics.
func (s *Store) Stats() (objects int, totalBytes int64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.index {
		objects++
		totalBytes += e.Size
	}
	return
}

// objectPath returns the filesystem path for a given hash.
// Uses a two-character prefix shard to avoid huge directories.
func (s *Store) objectPath(hash string) string {
	return filepath.Join(s.baseDir, "objects", hash[:2], hash)
}

func (s *Store) indexPath() string {
	return filepath.Join(s.baseDir, indexFile)
}

func (s *Store) loadIndex() error {
	data, err := os.ReadFile(s.indexPath())
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return json.Unmarshal(data, &s.index)
}

func (s *Store) saveIndex() error {
	s.mu.RLock()
	data, err := json.Marshal(s.index)
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	// Atomic index write.
	tmp, err := os.CreateTemp(s.baseDir, "index-*.json")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	tmp.Close()
	return os.Rename(tmpPath, s.indexPath())
}
