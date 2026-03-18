package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"p2p-ci/internal/config"
	"p2p-ci/internal/peer"
	"p2p-ci/internal/proxy"
	"p2p-ci/internal/store"
	"p2p-ci/internal/swarm"
)

func main() {
	configPath := flag.String("config", "", "path to config.yaml")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := run(*configPath, log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(configPath string, log *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	log.Info("p2p-ci proxy starting",
		"listen", cfg.ListenAddr,
		"upstream", cfg.UpstreamRegistry,
		"cache_dir", cfg.CacheDir,
		"peer_addr", cfg.PeerAddr,
	)

	// ── Store ────────────────────────────────────────────────────────────
	st, err := store.New(cfg.CacheDir, log)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	objects, totalBytes := st.Stats()
	log.Info("store ready", "cached_objects", objects, "total_size_mb", totalBytes/1024/1024)

	// ── Swarm ────────────────────────────────────────────────────────────
	sw := swarm.New(st, log)

	// ── Peer server (serves our cache to other machines) ─────────────────
	peerSrv := peer.NewServer(cfg.PeerAddr, st, log)
	if err := peerSrv.Start(); err != nil {
		return fmt.Errorf("start peer server: %w", err)
	}
	defer peerSrv.Stop()

	// ── mDNS discovery ───────────────────────────────────────────────────
	_, peerPort, err := splitHostPort(cfg.PeerAddr)
	if err != nil {
		return fmt.Errorf("parse peer addr: %w", err)
	}

	discovery, err := peer.NewDiscovery(
		peerPort,
		func(addr string) { sw.AddPeer(addr) },
		func(addr string) { sw.RemovePeer(addr) },
		log,
	)
	if err != nil {
		return fmt.Errorf("init mDNS: %w", err)
	}
	if err := discovery.Start(); err != nil {
		return fmt.Errorf("start mDNS: %w", err)
	}
	defer discovery.Stop()

	// ── HTTP proxy ───────────────────────────────────────────────────────
	handler := proxy.New(cfg, st, sw, log)

	rootHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_p2pci/health" {
			obj, b := st.Stats()
			fmt.Fprintf(w,
				`{"status":"ok","cached_objects":%d,"total_bytes":%d,"peers":%d,"upstream":"%s"}`,
				obj, b, sw.PeerCount(), cfg.UpstreamRegistry,
			)
			return
		}
		handler.ServeHTTP(w, r)
	})

	srv := &http.Server{
		Addr:         cfg.ListenAddr,
		Handler:      rootHandler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 10 * time.Minute,
		IdleTimeout:  120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	go func() {
		time.Sleep(300 * time.Millisecond)
		printSetupTips(cfg.ListenAddr, cfg.PeerAddr, cfg.UpstreamRegistry)
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("server error: %w", err)
	case <-ctx.Done():
		log.Info("shutting down...")
		shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return srv.Shutdown(shutCtx)
	}
}

func splitHostPort(addr string) (string, int, error) {
	var host string
	var port int
	_, err := fmt.Sscanf(addr, "%s", &addr)
	if err != nil {
		return "", 0, err
	}
	// Parse "host:port" or ":port"
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			host = addr[:i]
			fmt.Sscanf(addr[i+1:], "%d", &port)
			return host, port, nil
		}
	}
	return "", 0, fmt.Errorf("no port in %q", addr)
}

func printSetupTips(listenAddr, peerAddr, upstream string) {
	addr := "http://" + listenAddr
	fmt.Printf(`
+----------------------------------------------------------+
  p2p-ci proxy running
  Proxy:    %s  (pip points here)
  Peer:     %s  (other machines share files here)
  Upstream: %s

  pip:
    export PIP_INDEX_URL=%s/simple
    export PIP_TRUSTED_HOST=localhost

  Health: curl %s/_p2pci/health
+----------------------------------------------------------+
`, listenAddr, peerAddr, upstream, addr, addr)
}