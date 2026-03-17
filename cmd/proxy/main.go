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
	"p2p-ci/internal/proxy"
	"p2p-ci/internal/store"
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
	)

	st, err := store.New(cfg.CacheDir, log)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	objects, totalBytes := st.Stats()
	log.Info("store ready", "cached_objects", objects, "total_size_mb", totalBytes/1024/1024)

	handler := proxy.New(cfg, st, log)

	rootHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_p2pci/health" {
			obj, b := st.Stats()
			fmt.Fprintf(w, `{"status":"ok","cached_objects":%d,"total_bytes":%d,"upstream":"%s"}`,
				obj, b, cfg.UpstreamRegistry)
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
		time.Sleep(200 * time.Millisecond)
		printSetupTips(cfg.ListenAddr, cfg.UpstreamRegistry)
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

func printSetupTips(listenAddr, upstream string) {
	addr := "http://" + listenAddr
	fmt.Printf(`
+----------------------------------------------------------+
  p2p-ci proxy running at %s
  Forwarding to: %s

  pip:
    export PIP_INDEX_URL=%s/simple
    export PIP_TRUSTED_HOST=localhost

  npm (change upstream to https://registry.npmjs.org first):
    npm config set registry %s

  Health: curl %s/_p2pci/health
+----------------------------------------------------------+
`, listenAddr, upstream, addr, addr, addr)
}