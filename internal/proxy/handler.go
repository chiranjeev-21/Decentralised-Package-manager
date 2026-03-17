// Package proxy implements the caching HTTP reverse proxy for PyPI.
//
// How the two-stage PyPI download works:
//
//  1. pip requests the index:  GET /simple/requests/
//     → proxy fetches from pypi.org (plain text, no gzip), rewrites all
//       download links to point back through itself, returns modified HTML.
//       NOT cached — always fresh.
//
//  2. pip follows a rewritten link: GET /packages/.../requests-2.32.3.tar.gz
//     → proxy fetches from files.pythonhosted.org, caches the file, streams
//       to pip. On subsequent requests: served from cache instantly.
//
// This means ONLY immutable package files are cached. Index pages are always
// fresh from PyPI (they change when new versions are published).
package proxy

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"p2p-ci/internal/config"
	"p2p-ci/internal/store"
)

// filesHost is where PyPI actually serves package files from.
const filesHost = "https://files.pythonhosted.org"

// linkRewriter matches href="https://files.pythonhosted.org/packages/..."
// and rewrites to href="/packages/..." so pip fetches through our proxy.
var linkRewriter = regexp.MustCompile(`href="https://files\.pythonhosted\.org(/packages/[^"]+)"`)

type Handler struct {
	cfg       *config.Config
	store     *store.Store
	upstream  *http.Client
	log       *slog.Logger
	indexBase *url.URL
}

func New(cfg *config.Config, st *store.Store, log *slog.Logger) *Handler {
	base, _ := url.Parse(cfg.UpstreamRegistry)
	return &Handler{
		cfg:       cfg,
		store:     st,
		log:       log,
		indexBase: base,
		upstream: &http.Client{
			Timeout: 5 * time.Minute,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
				TLSHandshakeTimeout: 10 * time.Second,
			},
		},
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "only GET and HEAD are proxied", http.StatusMethodNotAllowed)
		return
	}

	path := r.URL.Path

	switch {
	case strings.HasPrefix(path, "/simple/") || path == "/simple":
		h.serveIndex(w, r)
	case strings.HasPrefix(path, "/packages/"):
		h.servePackageFile(w, r)
	default:
		h.passThrough(w, r, h.indexBase.String()+path)
	}
}

// serveIndex fetches a fresh index page from PyPI and rewrites all download
// links so they point through this proxy instead of files.pythonhosted.org.
//
// KEY: We do NOT forward Accept-Encoding here. This forces PyPI to return
// plain uncompressed HTML so our regex can match and rewrite the links.
// If we forwarded gzip, we'd be regex-matching compressed bytes — no match,
// no rewriting, pip goes straight to files.pythonhosted.org, cache never fills.
func (h *Handler) serveIndex(w http.ResponseWriter, r *http.Request) {
	upstreamURL := h.indexBase.String() + r.URL.Path
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}

	h.log.Info("index (not cached)", "path", r.URL.Path)

	// Build request — explicitly NO Accept-Encoding so we get plain text back.
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstreamURL, nil)
	if err != nil {
		http.Error(w, "build request failed", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Accept", "text/html")
	req.Header.Set("User-Agent", r.Header.Get("User-Agent"))
	// Intentionally omitting Accept-Encoding → PyPI sends uncompressed HTML

	resp, err := h.upstream.Do(req)
	if err != nil {
		h.log.Error("index fetch failed", "url", upstreamURL, "err", err)
		http.Error(w, "upstream fetch failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, "read upstream response failed", http.StatusBadGateway)
		return
	}

	// Rewrite:
	//   href="https://files.pythonhosted.org/packages/...whl#sha256=abc"
	// →  href="/packages/...whl#sha256=abc"
	//
	// pip follows the rewritten /packages/... URL back to our proxy,
	// which serves from cache or fetches + caches from files.pythonhosted.org.
	rewritten := linkRewriter.ReplaceAll(body, []byte(`href="$1"`))

	h.log.Info("index rewritten",
		"path", r.URL.Path,
		"original_bytes", len(body),
		"rewritten_bytes", len(rewritten),
	)

	copyHeaders(w.Header(), resp.Header)
	// Remove Content-Encoding since we served uncompressed content.
	w.Header().Del("Content-Encoding")
	w.Header().Set("X-P2PCI-Cache", "BYPASS")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(rewritten)))
	w.WriteHeader(resp.StatusCode)
	w.Write(rewritten) //nolint:errcheck
}

// servePackageFile serves a .whl / .tar.gz from cache, or fetches from
// files.pythonhosted.org, caches it, and streams to the client.
func (h *Handler) servePackageFile(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	path := r.URL.Path
	upstreamURL := filesHost + path
	cacheKey := upstreamURL

	// --- Cache hit ---
	reader, entry, err := h.store.Get(cacheKey)
	if err != nil {
		h.log.Warn("cache read error, falling back to upstream", "err", err)
	}
	if reader != nil {
		defer reader.Close()
		h.log.Info("cache HIT",
			"file", fileBaseName(path),
			"size_kb", entry.Size/1024,
			"elapsed_ms", time.Since(start).Milliseconds(),
		)
		if entry.ContentType != "" {
			w.Header().Set("Content-Type", entry.ContentType)
		}
		w.Header().Set("X-P2PCI-Cache", "HIT")
		w.Header().Set("X-P2PCI-Hash", entry.Hash[:12])
		w.Header().Set("Content-Length", fmt.Sprintf("%d", entry.Size))
		if r.Method == http.MethodHead {
			return
		}
		io.Copy(w, reader) //nolint:errcheck
		return
	}

	// --- Cache miss: fetch from files.pythonhosted.org ---
	h.log.Info("cache MISS — fetching",
		"file", fileBaseName(path),
		"from", filesHost,
	)

	resp, err := h.fetchUpstream(r, upstreamURL)
	if err != nil {
		http.Error(w, "upstream fetch failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		copyHeaders(w.Header(), resp.Header)
		w.Header().Set("X-P2PCI-Cache", "MISS")
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body) //nolint:errcheck
		return
	}

	if r.Method == http.MethodHead {
		copyHeaders(w.Header(), resp.Header)
		w.Header().Set("X-P2PCI-Cache", "MISS")
		return
	}

	// Tee: stream to client AND write to cache simultaneously.
	// Client gets bytes immediately — no added latency.
	pr, pw := io.Pipe()
	storeDone := make(chan error, 1)
	go func() {
		hash, err := h.store.Put(cacheKey, resp.Header.Get("Content-Type"), pr)
		if err != nil {
			h.log.Error("store.Put failed", "err", err)
		} else {
			h.log.Info("cached",
				"file", fileBaseName(path),
				"hash", hash[:12],
				"elapsed_ms", time.Since(start).Milliseconds(),
			)
		}
		storeDone <- err
	}()

	copyHeaders(w.Header(), resp.Header)
	w.Header().Set("X-P2PCI-Cache", "MISS")
	w.WriteHeader(http.StatusOK)

	tee := io.TeeReader(resp.Body, pw)
	if _, err := io.Copy(w, tee); err != nil {
		h.log.Warn("stream to client failed", "err", err)
		pw.CloseWithError(err)
	} else {
		pw.Close()
	}
	<-storeDone
}

// passThrough proxies without caching.
func (h *Handler) passThrough(w http.ResponseWriter, r *http.Request, upstreamURL string) {
	resp, err := h.fetchUpstream(r, upstreamURL)
	if err != nil {
		http.Error(w, "upstream fetch failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body) //nolint:errcheck
}

func (h *Handler) fetchUpstream(r *http.Request, upstreamURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, nil)
	if err != nil {
		return nil, err
	}
	for _, hdr := range []string{"Accept", "Accept-Encoding", "User-Agent"} {
		if v := r.Header.Get(hdr); v != "" {
			req.Header.Set(hdr, v)
		}
	}
	resp, err := h.upstream.Do(req)
	if err != nil {
		h.log.Error("upstream fetch failed", "url", upstreamURL, "err", err)
		return nil, err
	}
	return resp, nil
}

func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		switch strings.ToLower(k) {
		case "connection", "keep-alive", "proxy-authenticate",
			"proxy-authorization", "te", "trailers", "transfer-encoding", "upgrade":
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func fileBaseName(path string) string {
	parts := strings.Split(path, "/")
	if len(parts) > 0 {
		name := parts[len(parts)-1]
		if idx := strings.Index(name, "#"); idx != -1 {
			name = name[:idx]
		}
		return name
	}
	return path
}