package hostbin

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Extract writes the embedded host binary (if any) to a per-user cache path named by its content hash, makes it
// executable, and returns the path. When nothing is embedded (a normal build) it returns ("", false, nil) so the
// caller falls back to other discovery. The content-hash filename lets a new build extract fresh and coexist with
// older ones, and lets a warm cache skip the write. The write is atomic (temp in the same dir, then rename), so a
// second concurrent process either sees the finished file or writes its own temp and renames over it.
func Extract() (path string, embedded bool, err error) {
	b := Bytes()
	if len(b) == 0 {
		return "", false, nil
	}
	p, err := extractBytes(b)
	return p, true, err
}

// extractBytes is the content-addressed write half of Extract, split out so it can be unit-tested with synthetic
// bytes (the embedded payload only exists in an -tags embedhost build).
func extractBytes(b []byte) (string, error) {
	sum := sha256.Sum256(b)
	name := "sidechain-host-" + hex.EncodeToString(sum[:])[:12]
	if runtime.GOOS == "windows" {
		name += ".exe"
	}

	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("locate cache dir: %w", err)
	}
	dir := filepath.Join(cache, "mcp-vst-sidechain")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create cache dir %s: %w", dir, err)
	}
	dst := filepath.Join(dir, name)

	// Warm cache: reuse an already-extracted host with the right size (the name already pins the content hash).
	if fi, err := os.Stat(dst); err == nil && !fi.IsDir() && fi.Size() == int64(len(b)) {
		return dst, nil
	}

	tmp, err := os.CreateTemp(dir, "extract-*")
	if err != nil {
		return "", fmt.Errorf("create temp in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", fmt.Errorf("write host: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return "", fmt.Errorf("close host: %w", err)
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		os.Remove(tmpName)
		return "", fmt.Errorf("chmod host: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		os.Remove(tmpName)
		return "", fmt.Errorf("install host at %s: %w", dst, err)
	}
	return dst, nil
}
