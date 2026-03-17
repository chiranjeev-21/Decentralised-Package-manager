package bundleid_test

import (
	"os"
	"path/filepath"
	"testing"

	"p2p-ci/internal/bundleid"
)

func TestFromFile_Deterministic(t *testing.T) {
	dir := t.TempDir()
	lockfile := filepath.Join(dir, "package-lock.json")
	content := `{"name":"test","lockfileVersion":3,"requires":true,"packages":{}}`
	os.WriteFile(lockfile, []byte(content), 0644)

	id1, err := bundleid.FromFile(lockfile, "")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	id2, err := bundleid.FromFile(lockfile, "")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("not deterministic: %q vs %q", id1, id2)
	}
}

func TestFromDir_FindsLockfile(t *testing.T) {
	for _, name := range []string{
		"package-lock.json",
		"yarn.lock",
		"requirements.txt",
		"Cargo.lock",
		"go.sum",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, name), []byte("# lockfile"), 0644)

			id, found, err := bundleid.FromDir(dir, "")
			if err != nil {
				t.Fatalf("FromDir: %v", err)
			}
			if id == "" {
				t.Fatal("empty bundle ID")
			}
			if filepath.Base(found) != name {
				t.Fatalf("expected %q, got %q", name, filepath.Base(found))
			}
		})
	}
}

func TestFromDir_NoLockfile(t *testing.T) {
	dir := t.TempDir()
	_, _, err := bundleid.FromDir(dir, "")
	if err == nil {
		t.Fatal("expected error when no lockfile present")
	}
}

func TestHMACvsPlain_Different(t *testing.T) {
	dir := t.TempDir()
	lockfile := filepath.Join(dir, "Cargo.lock")
	os.WriteFile(lockfile, []byte("# Cargo lockfile"), 0644)

	plain, _ := bundleid.FromFile(lockfile, "")
	hmac, _ := bundleid.FromFile(lockfile, "my-org-secret")

	if plain == hmac {
		t.Fatal("plain and HMAC ids should differ")
	}
	if bundleid.IsHMAC(plain) {
		t.Fatal("plain ID should not be flagged as HMAC")
	}
	if !bundleid.IsHMAC(hmac) {
		t.Fatal("HMAC ID should be flagged as HMAC")
	}
}

func TestContentChange_ProducesDifferentID(t *testing.T) {
	dir := t.TempDir()
	lockfile := filepath.Join(dir, "poetry.lock")

	os.WriteFile(lockfile, []byte("version = 1"), 0644)
	id1, _ := bundleid.FromFile(lockfile, "")

	os.WriteFile(lockfile, []byte("version = 2"), 0644)
	id2, _ := bundleid.FromFile(lockfile, "")

	if id1 == id2 {
		t.Fatal("different lockfile contents should produce different bundle IDs")
	}
}