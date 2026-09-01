// server_test.go - coverage for server.go: loadCatalogFile error paths (empty path, missing file) and success,
// plus NewServer returning a non-nil server + session for a valid catalog. No stdio transport is run.

package sidechain

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadCatalogFileErrors(t *testing.T) {
	// empty path.
	if _, err := loadCatalogFile(""); err == nil {
		t.Fatal("empty catalog path should error")
	}
	// missing file.
	missing := filepath.Join(t.TempDir(), "nope.json")
	if _, err := loadCatalogFile(missing); err == nil {
		t.Fatal("missing catalog file should error")
	}
}

func TestLoadCatalogFileSuccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cat.json")
	blob := `{"stateRootTag":"PARAMS","stateVersion":2,"params":[
		{"id":"cutoff","label":"Cutoff","group":"filter","type":"float","min":20,"max":20000,"default":1000}]}`
	if err := os.WriteFile(path, []byte(blob), 0o600); err != nil {
		t.Fatalf("write catalog: %v", err)
	}
	c, err := loadCatalogFile(path)
	if err != nil {
		t.Fatalf("loadCatalogFile: %v", err)
	}
	if len(c.All()) != 1 || c.Get("cutoff") == nil {
		t.Fatalf("loaded catalog missing cutoff: %+v", c.All())
	}
}

func TestNewServer(t *testing.T) {
	srv, s := NewServer("test", "9.9.9", testCatalog())
	if srv == nil {
		t.Fatal("NewServer returned a nil server")
	}
	if s == nil {
		t.Fatal("NewServer returned a nil session")
	}
	if s.catalog == nil || len(s.catalog.All()) != 3 {
		t.Fatalf("session catalog = %+v, want the 3-param test catalog", s.catalog)
	}
	if s.live != nil {
		t.Fatal("new server session should start headless (live nil)")
	}
}
