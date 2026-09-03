package hostbin

import (
	"os"
	"testing"
)

// TestExtractNotEmbedded covers the normal (stub) build: nothing is embedded, so Extract reports not-embedded and
// the caller falls back to other host discovery.
func TestExtractNotEmbedded(t *testing.T) {
	if len(Bytes()) != 0 {
		t.Skip("built with -tags embedhost; the not-embedded path is not exercised here")
	}
	path, embedded, err := Extract()
	if embedded || err != nil || path != "" {
		t.Fatalf("Extract() with no payload = (%q, %v, %v), want (\"\", false, nil)", path, embedded, err)
	}
}

// TestExtractBytesWritesExecutable covers the content-addressed write half with synthetic bytes (independent of the
// embed tag): it writes an executable to the cache dir, is idempotent (warm cache reuses the same path), and a
// different payload lands at a different content-hashed path.
func TestExtractBytesWritesExecutable(t *testing.T) {
	// Redirect the cache dir to a temp location so the test does not touch the real user cache. os.UserCacheDir
	// reads XDG_CACHE_HOME on Linux and HOME/Library/Caches on macOS, so set both.
	tmp := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", tmp)
	t.Setenv("HOME", tmp)

	payload := []byte("#!/bin/sh\necho fake-host\n")
	p1, err := extractBytes(payload)
	if err != nil {
		t.Fatalf("extractBytes: %v", err)
	}
	fi, err := os.Stat(p1)
	if err != nil {
		t.Fatalf("stat extracted: %v", err)
	}
	if fi.Size() != int64(len(payload)) {
		t.Fatalf("extracted size %d, want %d", fi.Size(), len(payload))
	}
	if fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("extracted file is not executable: mode %v", fi.Mode())
	}

	// Idempotent: same bytes -> same path (warm cache), no error.
	p2, err := extractBytes(payload)
	if err != nil || p2 != p1 {
		t.Fatalf("second extractBytes = (%q, %v), want (%q, nil)", p2, err, p1)
	}

	// Different bytes -> different content-hashed path.
	p3, err := extractBytes([]byte("#!/bin/sh\necho other\n"))
	if err != nil {
		t.Fatalf("extractBytes (other): %v", err)
	}
	if p3 == p1 {
		t.Fatalf("different payload extracted to the same path %q", p3)
	}
}
