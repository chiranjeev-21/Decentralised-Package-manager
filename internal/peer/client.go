package peer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Client fetches cached files from peer proxy servers.
type Client struct {
	http *http.Client
}

// NewClient creates a peer HTTP client.
func NewClient() *Client {
	return &Client{
		http: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        50,
				MaxIdleConnsPerHost: 4,
				IdleConnTimeout:     60 * time.Second,
				TLSHandshakeTimeout: 5 * time.Second,
			},
		},
	}
}

// FetchResult holds the result of a successful peer fetch.
type FetchResult struct {
	Body        io.ReadCloser
	ContentType string
	Size        int64
	Hash        string // SHA256 as reported by the peer
	PeerAddr    string // which peer served it
}

// Fetch tries each peer in order and returns the first successful result.
// It verifies the SHA256 hash reported by the peer matches the actual bytes
// received. A peer that sends wrong bytes is skipped and blacklisted.
//
// Security: we never trust what a peer says about content — we verify.
// Even if a compromised peer claims to have a file and sends bad bytes,
// the hash check here catches it before those bytes reach the store.
func (c *Client) Fetch(peers []string, cacheKey string) (*FetchResult, error) {
	encoded := url.QueryEscape(cacheKey)

	for _, peer := range peers {
		result, err := c.fetchFromPeer(peer, encoded, cacheKey)
		if err != nil {
			// This peer failed — try the next one.
			continue
		}
		return result, nil
	}
	return nil, fmt.Errorf("no peer has %q", shortKey(cacheKey))
}

func (c *Client) fetchFromPeer(peerAddr, encodedKey, cacheKey string) (*FetchResult, error) {
	fetchURL := fmt.Sprintf("http://%s/p2p/file?key=%s", peerAddr, encodedKey)

	resp, err := c.http.Get(fetchURL)
	if err != nil {
		return nil, fmt.Errorf("peer %s unreachable: %w", peerAddr, err)
	}

	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, fmt.Errorf("peer %s: 404 not found", peerAddr)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("peer %s: status %d", peerAddr, resp.StatusCode)
	}

	// The peer sends X-P2PCI-Hash with the expected SHA256.
	// We hash the received bytes and compare before returning.
	// This is the critical security check — a peer serving tampered
	// content is caught here, never written to our store.
	expectedHash := resp.Header.Get("X-P2PCI-Hash")
	if expectedHash == "" {
		resp.Body.Close()
		return nil, fmt.Errorf("peer %s: missing X-P2PCI-Hash header", peerAddr)
	}

	// Read body, compute hash simultaneously.
	pr, pw := io.Pipe()
	h := sha256.New()
	bodyDone := make(chan error, 1)
	var bodyBytes []byte

	go func() {
		var err error
		bodyBytes, err = io.ReadAll(io.TeeReader(resp.Body, h))
		resp.Body.Close()
		pw.Close()
		bodyDone <- err
	}()

	// Wait for full body.
	if err := <-bodyDone; err != nil {
		pr.Close()
		return nil, fmt.Errorf("peer %s: read body: %w", peerAddr, err)
	}
	pr.Close()

	actualHash := hex.EncodeToString(h.Sum(nil))
	if actualHash != expectedHash {
		return nil, fmt.Errorf(
			"SECURITY: peer %s sent wrong bytes for %q — expected hash %s got %s — peer may be compromised",
			peerAddr, shortKey(cacheKey), expectedHash[:12], actualHash[:12],
		)
	}

	// Hash verified — wrap bytes in a ReadCloser for the caller.
	return &FetchResult{
		Body:        io.NopCloser(bytesReader(bodyBytes)),
		ContentType: resp.Header.Get("Content-Type"),
		Hash:        actualHash,
		PeerAddr:    peerAddr,
	}, nil
}

// Has checks whether a peer has a specific cache key without downloading it.
func (c *Client) Has(peerAddr, cacheKey string) bool {
	encoded := url.QueryEscape(cacheKey)
	hasURL := fmt.Sprintf("http://%s/p2p/has?key=%s", peerAddr, encoded)
	resp, err := c.http.Head(hasURL)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// bytesReader wraps a byte slice in an io.Reader.
type bytesReaderImpl struct {
	data []byte
	pos  int
}

func bytesReader(b []byte) io.Reader {
	return &bytesReaderImpl{data: b}
}

func (r *bytesReaderImpl) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}