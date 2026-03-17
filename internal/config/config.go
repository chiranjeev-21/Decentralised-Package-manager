package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	ListenAddr     string `yaml:"listen_addr"`
	CacheDir       string `yaml:"cache_dir"`
	MaxCacheSizeGB int    `yaml:"max_cache_size_gb"`

	// UpstreamRegistry is the real registry this proxy forwards to.
	// e.g. "https://pypi.org" or "https://registry.npmjs.org"
	// Package managers point at http://localhost:7878 and the proxy
	// forwards everything upstream to this URL transparently.
	UpstreamRegistry string `yaml:"upstream_registry"`

	AuditLog  string `yaml:"audit_log"`
	OrgSecret string `yaml:"org_secret"`
}

func Default() *Config {
	home, _ := os.UserHomeDir()
	return &Config{
		ListenAddr:       "127.0.0.1:7878",
		CacheDir:         filepath.Join(home, ".p2p-cache"),
		MaxCacheSizeGB:   20,
		UpstreamRegistry: "https://pypi.org",
	}
}

func Load(path string) (*Config, error) {
	cfg := Default()

	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config %q: %w", path, err)
		}
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}

	if v := os.Getenv("P2PCI_LISTEN"); v != "" {
		cfg.ListenAddr = v
	}
	if v := os.Getenv("P2PCI_CACHE_DIR"); v != "" {
		cfg.CacheDir = v
	}
	if v := os.Getenv("P2PCI_ORG_SECRET"); v != "" {
		cfg.OrgSecret = v
	}
	if v := os.Getenv("P2PCI_UPSTREAM"); v != "" {
		cfg.UpstreamRegistry = v
	}

	return cfg, cfg.Validate()
}

func (c *Config) Validate() error {
	if c.ListenAddr == "" {
		return fmt.Errorf("listen_addr is required")
	}
	if c.UpstreamRegistry == "" {
		return fmt.Errorf("upstream_registry is required")
	}
	u, err := url.Parse(c.UpstreamRegistry)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("upstream_registry must be a valid http/https URL, got %q", c.UpstreamRegistry)
	}
	if c.MaxCacheSizeGB <= 0 {
		return fmt.Errorf("max_cache_size_gb must be positive")
	}
	return nil
}

// UpstreamHost returns just the hostname for logging.
func (c *Config) UpstreamHost() string {
	u, err := url.Parse(c.UpstreamRegistry)
	if err != nil {
		return c.UpstreamRegistry
	}
	return strings.TrimPrefix(u.Host, "www.")
}