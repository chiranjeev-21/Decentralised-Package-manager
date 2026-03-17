// Package bundleid generates deterministic bundle identifiers from lockfiles.
//
// A bundle ID is the canonical identifier for a complete dependency set.
// Because lockfiles are deterministic, two builds with the same lockfile
// need byte-for-byte identical dependencies — making the bundle ID a safe
// cache key and a safe content identity for P2P sharing.
//
// Security: when orgSecret is set, bundle IDs are HMAC-SHA256 rather than
// plain SHA256. This prevents external observers querying the DHT from
// enumerating your dependency graph and inferring your tech stack or
// identifying machines running vulnerable dependency versions.
package bundleid

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// KnownLockfiles lists the lockfile names this tool recognises, in priority order.
// The first match in a directory wins.
var KnownLockfiles = []string{
	"package-lock.json", // npm
	"yarn.lock",         // yarn
	"pnpm-lock.yaml",    // pnpm
	"requirements.txt",  // pip (plain)
	"requirements.lock", // pip (locked)
	"Pipfile.lock",      // pipenv
	"poetry.lock",       // poetry
	"Cargo.lock",        // cargo
	"go.sum",            // go modules
	"Gemfile.lock",      // bundler (ruby)
	"composer.lock",     // composer (php)
}

// FromFile computes a bundle ID from a specific lockfile path.
// orgSecret may be empty; if set, the ID is HMAC-SHA256 (opaque to outsiders).
func FromFile(path, orgSecret string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open lockfile %q: %w", path, err)
	}
	defer f.Close()
	return fromReader(f, orgSecret)
}

// FromDir finds the first known lockfile in dir and computes a bundle ID from it.
// Returns the bundle ID and the lockfile path that was used.
func FromDir(dir, orgSecret string) (id, lockfilePath string, err error) {
	for _, name := range KnownLockfiles {
		candidate := filepath.Join(dir, name)
		if _, statErr := os.Stat(candidate); statErr == nil {
			id, err = FromFile(candidate, orgSecret)
			return id, candidate, err
		}
	}
	return "", "", fmt.Errorf("no lockfile found in %q (looked for: %s)",
		dir, strings.Join(KnownLockfiles, ", "))
}

// FromReader computes a bundle ID from an arbitrary reader (e.g. piped stdin).
func fromReader(r io.Reader, orgSecret string) (string, error) {
	if orgSecret != "" {
		mac := hmac.New(sha256.New, []byte(orgSecret))
		if _, err := io.Copy(mac, r); err != nil {
			return "", fmt.Errorf("hmac lockfile: %w", err)
		}
		return "hmac:" + hex.EncodeToString(mac.Sum(nil)), nil
	}

	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", fmt.Errorf("hash lockfile: %w", err)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// IsHMAC reports whether a bundle ID was generated with an org secret.
// Plain SHA256 IDs can be reverse-looked-up; HMAC IDs cannot.
func IsHMAC(bundleID string) bool {
	return strings.HasPrefix(bundleID, "hmac:")
}

// Short returns the first 12 hex characters of the bundle ID for logging.
func Short(bundleID string) string {
	parts := strings.SplitN(bundleID, ":", 2)
	if len(parts) == 2 && len(parts[1]) >= 12 {
		return parts[0] + ":" + parts[1][:12]
	}
	if len(bundleID) >= 12 {
		return bundleID[:12]
	}
	return bundleID
}
