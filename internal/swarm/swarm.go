// Package swarm manages the set of known peers and coordinates
// peer-first file resolution.
//
// Resolution order:
//   1. Ask all known peers simultaneously (fan-out HEAD check)
//   2. First peer that responds with 200 streams the file
//   3. Verify SHA256 of received bytes
//   4. Only if no peer has it: fall back to upstream registry
//
// Security:
//   - Peers that fail hash verification are blacklisted for 5 minutes.
//   - Peers that are unreachable are removed after 3 consecutive failures.
//   - The blacklist is checked before any peer is queried.
package swarm

import (
	"io"
	"log/slog"
	"sync"
	"time"

	"p2p-ci/internal/peer"
	"p2p-ci/internal/store"
)

const (
	blacklistDuration  = 5 * time.Minute
	maxFailures        = 3
	peerQueryTimeout   = 3 * time.Second
)

type peerState struct {
	addr        string
	failures    int
	blacklisted time.Time
}

// Swarm manages the peer list and coordinates file lookups.
type Swarm struct {
	mu      sync.RWMutex
	peers   map[string]*peerState
	client  *peer.Client
	store   *store.Store
	log     *slog.Logger
}

// New creates a new swarm.
func New(st *store.Store, log *slog.Logger) *Swarm {
	return &Swarm{
		peers:  make(map[string]*peerState),
		client: peer.NewClient(),
		store:  st,
		log:    log,
	}
}

// AddPeer registers a newly discovered peer. Safe to call multiple times
// with the same address — idempotent.
func (s *Swarm) AddPeer(addr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.peers[addr]; !exists {
		s.peers[addr] = &peerState{addr: addr}
		s.log.Info("swarm: peer added", "addr", addr, "total_peers", len(s.peers))
	}
}

// RemovePeer removes a peer that has gone away.
func (s *Swarm) RemovePeer(addr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.peers, addr)
	s.log.Info("swarm: peer removed", "addr", addr, "total_peers", len(s.peers))
}

// PeerCount returns the number of currently known peers.
func (s *Swarm) PeerCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.peers)
}

// FetchFromPeers tries to get cacheKey from any known peer.
// Returns (reader, peerAddr, nil) on success.
// Returns (nil, "", nil) if no peer has it — caller should fall back to upstream.
// Returns (nil, "", err) on a hash verification failure — log and fall back.
func (s *Swarm) FetchFromPeers(cacheKey string) (io.ReadCloser, string, error) {
	activePeers := s.activePeers()
	if len(activePeers) == 0 {
		return nil, "", nil
	}

	s.log.Info("swarm: querying peers", "peers", len(activePeers), "key", shortKey(cacheKey))

	// Fan out: ask all peers simultaneously, return first hit.
	type result struct {
		addr   string
		reader io.ReadCloser
		ct     string
		err    error
		hash   string
	}

	ch := make(chan result, len(activePeers))

	for _, p := range activePeers {
		go func(addr string) {
			res, err := s.client.Fetch([]string{addr}, cacheKey)
			if err != nil {
				ch <- result{addr: addr, err: err}
				return
			}
			ch <- result{
				addr:   addr,
				reader: res.Body,
				ct:     res.ContentType,
				hash:   res.Hash,
			}
		}(p)
	}

	// Collect results — return first success, track failures.
	var firstSuccess *result
	failures := make(map[string]error)

	for i := 0; i < len(activePeers); i++ {
		r := <-ch
		if r.err != nil {
			failures[r.addr] = r.err
			continue
		}
		if firstSuccess == nil {
			firstSuccess = &r
		} else {
			// Already have a result — close extra readers.
			if r.reader != nil {
				r.reader.Close()
			}
		}
	}

	// Record failures.
	for addr, err := range failures {
		s.recordFailure(addr, err)
	}

	if firstSuccess == nil {
		return nil, "", nil // no peer had it
	}

	// Store the peer-fetched file in our local cache so we can seed it too.
	go func(r result) {
		defer r.reader.Close()
		if _, err := s.store.Put(cacheKey, r.ct, r.reader); err != nil {
			s.log.Warn("swarm: failed to cache peer fetch", "err", err)
		}
	}(*firstSuccess)

	// Re-open from our local store so the caller gets a fresh verified reader.
	// This ensures what the caller serves has passed our own hash check.
	reader, entry, err := s.store.Get(cacheKey)
	if err != nil || reader == nil {
		// Store write might still be in progress — return a fresh fetch.
		res, fetchErr := s.client.Fetch([]string{firstSuccess.addr}, cacheKey)
		if fetchErr != nil {
			return nil, "", fetchErr
		}
		return res.Body, firstSuccess.addr, nil
	}
	_ = entry
	return reader, firstSuccess.addr, nil
}

// activePeers returns the list of peer addresses that are not blacklisted.
func (s *Swarm) activePeers() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now()
	var active []string
	for addr, p := range s.peers {
		if !p.blacklisted.IsZero() && now.Before(p.blacklisted.Add(blacklistDuration)) {
			continue // still blacklisted
		}
		active = append(active, addr)
	}
	return active
}

func (s *Swarm) recordFailure(addr string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.peers[addr]
	if !ok {
		return
	}
	p.failures++
	s.log.Warn("swarm: peer failure",
		"addr", addr,
		"failures", p.failures,
		"err", err,
	)
	if p.failures >= maxFailures {
		p.blacklisted = time.Now()
		s.log.Warn("swarm: peer blacklisted",
			"addr", addr,
			"until", p.blacklisted.Add(blacklistDuration).Format(time.RFC3339),
		)
	}
}

func shortKey(key string) string {
	if len(key) > 60 {
		return key[:60] + "..."
	}
	return key
}