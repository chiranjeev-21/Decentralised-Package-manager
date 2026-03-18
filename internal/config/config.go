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
	ListenAddr       string `yaml:"listen_addr"`
	CacheDir         string `yaml:"cache_dir"`
	MaxCacheSizeGB   int    `yaml:"max_cache_size_gb"`
	UpstreamRegistry string `yaml:"upstream_registry"`

	// PeerAddr is the address this machine's peer server listens on.
	// Other machines on the LAN connect here to fetch cached files.
	// Should use 0.0.0.0 (not 127.0.0.1) so LAN peers can reach it.
	PeerAddr string `yaml:"peer_addr"`

	AuditLog  string `yaml:"audit_log"`
	OrgSecret string `yaml:"org_secret"`
}

func Default() *Config {
	home, _ := os.UserHomeDir()
	return &Config{
		ListenAddr:       "127.0.0.1:7878",
		PeerAddr:         "0.0.0.0:7879",
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
	if v := os.Getenv("P2PCI_PEER_ADDR"); v != "" {
		cfg.PeerAddr = v
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
	if c.PeerAddr == "" {
		return fmt.Errorf("peer_addr is required")
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

func (c *Config) UpstreamHost() string {
	u, err := url.Parse(c.UpstreamRegistry)
	if err != nil {
		return c.UpstreamRegistry
	}
	return strings.TrimPrefix(u.Host, "www.")
}