// server.go - the headless stdio MCP server (Option A, Go split). It loads a parameter catalog for the target
// plugin, registers the generic param tools + the live verbs on one MCP server, and speaks stdio JSON-RPC to
// an agent. The catalog is produced by the C++ host (cpp/Host) which loads the VST3/AU and enumerates its
// automatable parameters at load time, writing them as JSON; the Go server reads that JSON.
//
// Runtime shape (see docs/ARCHITECTURE.md):
//
//	agent  <--stdio MCP-->  this Go server  <--localhost TCP-->  cpp/Host (JUCE)  --hosts-->  target VST3/AU
//
// The catalog gives validate + clamp + choice math; connect_live opens the socket so set_param/get_param drive
// the running instance. GCF encodes the big read tool (list_params) for token efficiency.

package sidechain

import (
	"context"
	"fmt"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Config configures the headless server.
type Config struct {
	// CatalogPath is a JSON file (emitted by cpp/Host) describing the loaded plugin's parameter catalog.
	CatalogPath string
	// Name/Version identify the server to the MCP client.
	Name    string
	Version string
}

// Run builds and runs the headless stdio MCP server until the transport closes. It fails fast if the catalog
// cannot be loaded (a server with no catalog cannot validate or clamp any set).
func Run(ctx context.Context, cfg Config) error {
	if cfg.Name == "" {
		cfg.Name = "mcp-vst-sidechain"
	}
	if cfg.Version == "" {
		cfg.Version = "0.1.0"
	}

	cat, err := loadCatalogFile(cfg.CatalogPath)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "sidechain: loaded catalog (%d params) from %s\n", len(cat.All()), cfg.CatalogPath)

	srv, _ := NewServer(cfg.Name, cfg.Version, cat)
	return srv.Run(ctx, &mcp.StdioTransport{})
}

// NewServer builds the MCP server (generic param tools + live verbs) over a catalog and returns it plus the
// session, so a caller can drive it in-process (tests, or embedding in another Go host). The session's live
// endpoint starts nil (headless) and is set by connect_live.
func NewServer(name, version string, cat ParamCatalog) (*mcp.Server, *session) {
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    name,
		Title:   "Sidechain - agent control input for any plugin",
		Version: version,
	}, nil)

	s := newSession(cat)
	// The generic tools read the current endpoint through this accessor; the live verbs mutate s.live, so the
	// accessor just returns it. One session, one live field, shared by both tool sets.
	registerParamToolsOn(srv, s, func() LiveEndpoint { return s.live })
	registerLiveTools(srv, s)
	return srv, s
}

// loadCatalogFile reads and parses a catalog JSON file.
func loadCatalogFile(path string) (*Catalog, error) {
	if path == "" {
		return nil, fmt.Errorf("no catalog path (set --catalog or SIDECHAIN_CATALOG to the JSON that cpp/Host emitted for the loaded plugin)")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read catalog %s: %w", path, err)
	}
	return loadCatalogJSON(data)
}
