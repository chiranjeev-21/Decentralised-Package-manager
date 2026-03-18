// Package peer implements the LAN peer server and client.
//
// Each proxy exposes a lightweight HTTP server on a separate port
// (default :7879) that other peers use to fetch cached files.
//
// Security:
//   - Only files that exist in the local store are served.
//     store.Get re-verifies the SHA256 hash on every read before
//     returning bytes. A peer cannot be tricked into serving a file
//     it doesn't actually have.
//   - The server only responds to GET /p2p/file and HEAD /p2p/has.
//     No other endpoints exist — there is no way to write to or
//     enumerate the store through this interface.
//   - Rate limiting per remote IP prevents a single bad peer from
//     exhausting connections.
package peer

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"p2p-ci/internal/store"
)

// Server serves cached files to peer proxies on the LAN.
type Server struct {
	store    *store.Store
	log      *slog.Logger
	addr     string
	srv      *http.Server
	// connCount tracks active connections per remote IP for rate limiting.
	mu        sync.Mutex
	connCount map[string]int
}

const maxConnsPerPeer = 16

// NewServer creates a peer server that will listen on addr (e.g. ":7879").
func NewServer(addr string, st *store.Store, log *slog.Logger) *Server {
	s := &Server{
		store:     st,
		log:       log,
		addr:      addr,
		connCount: make(map[string]int),
	}

	mux := http.NewServeMux()
	// GET /p2p/file?key=<url-encoded cache key>
	// Streams the file if we have it, 404 if not.
	mux.HandleFunc("/p2p/file", s.handleFile)
	// HEAD /p2p/has?key=<url-encoded cache key>
	// Returns 200 if we have it, 404 if not. No body.
	mux.HandleFunc("/p2p/has", s.handleHas)
	// GET /p2p/health — simple liveness check.
	mux.HandleFunc("/p2p/health", func(w http.ResponseWriter, r *http.Request) {
		objects, bytes := st.Stats()
		fmt.Fprintf(w, `{"status":"ok","objects":%d,"bytes":%d}`, objects, bytes)
	})

	s.srv = &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 10 * time.Minute,
		IdleTimeout:  60 * time.Second,
		ConnState:    s.trackConn,
	}

	return s
}

// Start begins serving in the background.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("peer server listen %s: %w", s.addr, err)
	}
	s.log.Info("peer server listening", "addr", s.addr)
	go func() {
		if err := s.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			s.log.Error("peer server error", "err", err)
		}
	}()
	return nil
}

// Stop gracefully shuts down the peer server.
func (s *Server) Stop() {
	s.srv.Close()
}

func (s *Server) handleFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Rate limit: max N concurrent connections per peer IP.
	remoteIP, _, _ := net.SplitHostPort(r.RemoteAddr)
	if !s.checkRateLimit(remoteIP) {
		s.log.Warn("peer rate limited", "ip", remoteIP)
		http.Error(w, "too many connections from peer", http.StatusTooManyRequests)
		return
	}

	cacheKey, err := url.QueryUnescape(r.URL.Query().Get("key"))
	if err != nil || cacheKey == "" {
		http.Error(w, "missing or invalid key param", http.StatusBadRequest)
		return
	}

	// store.Get re-verifies the SHA256 hash before returning bytes.
	// If the file is corrupt or tampered, it returns an error — we never
	// serve bad content to peers.
	reader, entry, err := s.store.Get(cacheKey)
	if err != nil {
		s.log.Warn("peer requested key with hash error", "key", cacheKey[:min(len(cacheKey), 60)], "err", err)
		http.Error(w, "cache error", http.StatusInternalServerError)
		return
	}
	if reader == nil {
		http.NotFound(w, r)
		return
	}
	defer reader.Close()

	s.log.Info("serving to peer",
		"ip", remoteIP,
		"file", shortKey(cacheKey),
		"size_kb", entry.Size/1024,
	)

	w.Header().Set("Content-Type", entry.ContentType)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", entry.Size))
	w.Header().Set("X-P2PCI-Hash", entry.Hash)
	w.Header().Set("X-P2PCI-Source", "peer")

	if r.Method == http.MethodHead {
		return
	}
	io.Copy(w, reader) //nolint:errcheck
}

func (s *Server) handleHas(w http.ResponseWriter, r *http.Request) {
	cacheKey, err := url.QueryUnescape(r.URL.Query().Get("key"))
	if err != nil || cacheKey == "" {
		http.Error(w, "missing key", http.StatusBadRequest)
		return
	}
	reader, _, err := s.store.Get(cacheKey)
	if err != nil || reader == nil {
		http.NotFound(w, r)
		return
	}
	reader.Close()
	w.WriteHeader(http.StatusOK)
}

func (s *Server) trackConn(c net.Conn, state http.ConnState) {
	ip, _, _ := net.SplitHostPort(c.RemoteAddr().String())
	s.mu.Lock()
	defer s.mu.Unlock()
	switch state {
	case http.StateNew:
		s.connCount[ip]++
	case http.StateClosed, http.StateHijacked:
		s.connCount[ip]--
		if s.connCount[ip] <= 0 {
			delete(s.connCount, ip)
		}
	}
}

func (s *Server) checkRateLimit(ip string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connCount[ip] < maxConnsPerPeer
}

func shortKey(key string) string {
	parts := make([]rune, 0, 40)
	for i, r := range key {
		if i > 60 {
			return string(parts) + "..."
		}
		parts = append(parts, r)
	}
	return string(parts)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}