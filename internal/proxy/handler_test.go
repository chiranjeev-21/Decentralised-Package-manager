package proxy_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"p2p-ci/internal/config"
	"p2p-ci/internal/proxy"
	"p2p-ci/internal/store"
)

func newTestHandler(t *testing.T) (*proxy.Handler, *store.Store) {
	t.Helper()
	cfg := config.Default()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.New(t.TempDir(), log)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return proxy.New(cfg, st, log), st
}

// TestMethodRejection ensures only GET and HEAD are allowed.
func TestMethodRejection(t *testing.T) {
	h, _ := newTestHandler(t)

	for _, method := range []string{
		http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch,
	} {
		req := httptest.NewRequest(method, "/simple/requests/", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("method %s: expected 405, got %d", method, w.Code)
		}
	}
}

// TestPackageFileCacheHit verifies a cached .whl is served from disk.
func TestPackageFileCacheHit(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Default()
	st, _ := store.New(t.TempDir(), log)

	// Pre-populate cache with a fake wheel file.
	// Cache key = the files.pythonhosted.org URL the proxy would use.
	cacheKey := "https://files.pythonhosted.org/packages/ab/cd/requests-2.32.3-py3-none-any.whl"
	content := strings.Repeat("fake wheel content", 200)
	if _, err := st.Put(cacheKey, "application/zip", strings.NewReader(content)); err != nil {
		t.Fatalf("pre-populate cache: %v", err)
	}

	h := proxy.New(cfg, st, log)

	// Request comes in as /packages/ab/cd/requests-2.32.3-py3-none-any.whl
	req := httptest.NewRequest(http.MethodGet,
		"/packages/ab/cd/requests-2.32.3-py3-none-any.whl", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — body: %s", w.Code, w.Body.String())
	}
	if w.Header().Get("X-P2PCI-Cache") != "HIT" {
		t.Fatalf("expected HIT, got %q", w.Header().Get("X-P2PCI-Cache"))
	}
	if w.Body.String() != content {
		t.Fatal("cached content mismatch")
	}
}

// TestIndexNotCached verifies index pages get BYPASS header (never cached).
func TestIndexBypass(t *testing.T) {
	// Spin up a fake PyPI index server
	fakePyPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(200)
		w.Write([]byte(`<html><body>
<a href="https://files.pythonhosted.org/packages/ab/cd/requests-2.32.3-py3-none-any.whl#sha256=abc">requests-2.32.3</a>
</body></html>`))
	}))
	defer fakePyPI.Close()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Default()
	cfg.UpstreamRegistry = fakePyPI.URL
	st, _ := store.New(t.TempDir(), log)
	h := proxy.New(cfg, st, log)

	req := httptest.NewRequest(http.MethodGet, "/simple/requests/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if w.Header().Get("X-P2PCI-Cache") != "BYPASS" {
		t.Fatalf("index page should be BYPASS, got %q", w.Header().Get("X-P2PCI-Cache"))
	}

	// The download link should be rewritten to /packages/... (no files.pythonhosted.org)
	body := w.Body.String()
	if strings.Contains(body, "files.pythonhosted.org") {
		t.Fatal("index response still contains files.pythonhosted.org — link rewriting failed")
	}
	if !strings.Contains(body, `href="/packages/`) {
		t.Fatalf("expected rewritten href=/packages/..., body was:\n%s", body)
	}
}