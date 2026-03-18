// Package proxy implements the caching HTTP reverse proxy for PyPI.
//
// Resolution order for package files:
//   1. Local disk cache (instant, 0ms)
//   2. LAN peer swarm (fast, ~5ms over LAN)
//   3. Upstream registry (slow, 50-500ms over internet)
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
	"p2p-ci/internal/swarm"
)

const filesHost = "https://files.pythonhosted.org"

var linkRewriter = regexp.MustCompile(`href="https://files\.pythonhosted\.org(/packages/[^"]+)"`)

type Handler struct {
	cfg       *config.Config
	store     *store.Store
	swarm     *swarm.Swarm
	upstream  *http.Client
	log       *slog.Logger
	indexBase *url.URL
}

func New(cfg *config.Config, st *store.Store, sw *swarm.Swarm, log *slog.Logger) *Handler {
	base, _ := url.Parse(cfg.UpstreamRegistry)
	return &Handler{
		cfg:       cfg,
		store:     st,
		swarm:     sw,
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

func (h *Handler) serveIndex(w http.ResponseWriter, r *http.Request) {
	upstreamURL := h.indexBase.String() + r.URL.Path
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}

	h.log.Info("index (not cached)", "path", r.URL.Path)

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstreamURL, nil)
	if err != nil {
		http.Error(w, "build request failed", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Accept", "text/html")
	req.Header.Set("User-Agent", r.Header.Get("User-Agent"))
	// No Accept-Encoding — forces plain text so regex rewriting works.

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

	rewritten := linkRewriter.ReplaceAll(body, []byte(`href="$1"`))

	h.log.Info("index rewritten",
		"path", r.URL.Path,
		"original_bytes", len(body),
		"rewritten_bytes", len(rewritten),
	)

	copyHeaders(w.Header(), resp.Header)
	w.Header().Del("Content-Encoding")
	w.Header().Set("X-P2PCI-Cache", "BYPASS")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(rewritten)))
	w.WriteHeader(resp.StatusCode)
	w.Write(rewritten) //nolint:errcheck
}

func (h *Handler) servePackageFile(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	path := r.URL.Path
	upstreamURL := filesHost + path
	cacheKey := upstreamURL

	// ── 1. Local cache ───────────────────────────────────────────────────
	reader, entry, err := h.store.Get(cacheKey)
	if err != nil {
		h.log.Warn("cache read error, continuing", "err", err)
	}
	if reader != nil {
		defer reader.Close()
		h.log.Info("cache HIT (local)",
			"file", fileBaseName(path),
			"size_kb", entry.Size/1024,
			"elapsed_ms", time.Since(start).Milliseconds(),
		)
		w.Header().Set("Content-Type", entry.ContentType)
		w.Header().Set("X-P2PCI-Cache", "HIT")
		w.Header().Set("X-P2PCI-Source", "local")
		w.Header().Set("X-P2PCI-Hash", entry.Hash[:12])
		w.Header().Set("Content-Length", fmt.Sprintf("%d", entry.Size))
		if r.Method == http.MethodHead {
			return
		}
		io.Copy(w, reader) //nolint:errcheck
		return
	}

	// ── 2. Peer swarm ────────────────────────────────────────────────────
	if h.swarm != nil && h.swarm.PeerCount() > 0 {
		peerReader, peerAddr, err := h.swarm.FetchFromPeers(cacheKey)
		if err != nil {
			h.log.Warn("swarm fetch error, falling back to upstream",
				"file", fileBaseName(path), "err", err)
		} else if peerReader != nil {
			defer peerReader.Close()
			h.log.Info("cache HIT (peer)",
				"file", fileBaseName(path),
				"peer", peerAddr,
				"elapsed_ms", time.Since(start).Milliseconds(),
			)

			// Re-read from local store so we serve the hash-verified version.
			if localReader, localEntry, e := h.store.Get(cacheKey); e == nil && localReader != nil {
				defer localReader.Close()
				w.Header().Set("Content-Type", localEntry.ContentType)
				w.Header().Set("X-P2PCI-Cache", "HIT")
				w.Header().Set("X-P2PCI-Source", fmt.Sprintf("peer:%s", peerAddr))
				w.Header().Set("X-P2PCI-Hash", localEntry.Hash[:12])
				w.Header().Set("Content-Length", fmt.Sprintf("%d", localEntry.Size))
				if r.Method != http.MethodHead {
					io.Copy(w, localReader) //nolint:errcheck
				}
				return
			}

			// Store write still in progress — stream from peer reader directly.
			w.Header().Set("X-P2PCI-Cache", "HIT")
			w.Header().Set("X-P2PCI-Source", fmt.Sprintf("peer:%s", peerAddr))
			if r.Method != http.MethodHead {
				io.Copy(w, peerReader) //nolint:errcheck
			}
			return
		}
	}

	// ── 3. Upstream registry ─────────────────────────────────────────────
	h.log.Info("cache MISS — fetching upstream",
		"file", fileBaseName(path),
		"peers_checked", func() int {
			if h.swarm != nil {
				return h.swarm.PeerCount()
			}
			return 0
		}(),
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

	// Tee to client + cache simultaneously.
	pr, pw := io.Pipe()
	storeDone := make(chan error, 1)
	go func() {
		hash, err := h.store.Put(cacheKey, resp.Header.Get("Content-Type"), pr)
		if err != nil {
			h.log.Error("store.Put failed", "err", err)
		} else {
			h.log.Info("cached from upstream",
				"file", fileBaseName(path),
				"hash", hash[:12],
				"elapsed_ms", time.Since(start).Milliseconds(),
			)
		}
		storeDone <- err
	}()

	copyHeaders(w.Header(), resp.Header)
	w.Header().Set("X-P2PCI-Cache", "MISS")
	w.Header().Set("X-P2PCI-Source", "upstream")
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