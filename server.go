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
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Config configures the headless server.
type Config struct {
	// CatalogPath is a JSON file (emitted by cpp/Host) describing the loaded plugin's parameter catalog. Used in
	// external-host mode (the agent calls connect_live itself). Mutually exclusive with Plugin.
	CatalogPath string
	// Plugin is a VST3/AU path (or AU identifier) that selects MANAGED-host mode: the server spawns and supervises
	// the cpp host itself, waits for its catalog, auto-connects the live endpoint, then serves MCP. Mutually
	// exclusive with CatalogPath.
	Plugin string
	// HostBin is an explicit path to the sidechain-host binary (managed mode). Empty triggers discovery: next to
	// the running executable, then on PATH.
	HostBin string
	// Port is the control port. In managed mode 0 means "pick a free ephemeral port". In external mode it is
	// unused (connect_live carries its own default).
	Port int
	// SelfTest, in managed mode, runs the managed startup, verifies it works (enumerate/connect/read/set), prints a
	// one-line result to stderr, and exits without serving MCP over stdio.
	SelfTest bool
	// Name/Version identify the server to the MCP client.
	Name    string
	Version string
	// SemanticDir is the Phase-3 persistent semantic store directory (per-fingerprint files). Empty uses
	// SIDECHAIN_SEMANTIC_DIR or a per-user cache dir.
	SemanticDir string
}

// Run builds and runs the headless stdio MCP server until the transport closes. It fails fast if the catalog
// cannot be loaded (a server with no catalog cannot validate or clamp any set). Setting Plugin selects managed
// mode (spawn + supervise the host); setting CatalogPath selects external mode. Exactly one must be set.
func Run(ctx context.Context, cfg Config) error {
	cfg.applyDefaults()

	hasPlugin := strings.TrimSpace(cfg.Plugin) != ""
	hasCatalog := strings.TrimSpace(cfg.CatalogPath) != ""
	switch {
	case hasPlugin && hasCatalog:
		return fmt.Errorf("--plugin and --catalog are mutually exclusive: --plugin spawns and supervises the host (managed mode); --catalog attaches to an already-running external host")
	case !hasPlugin && !hasCatalog:
		return fmt.Errorf("set either --plugin <path|AU-id> (managed mode: sidechain spawns the host) or --catalog <path> (external mode: attach to a running host)")
	case hasPlugin:
		return runManaged(ctx, cfg)
	}

	cat, err := loadCatalogFile(cfg.CatalogPath)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "sidechain: loaded catalog (%d params) from %s\n", len(cat.All()), cfg.CatalogPath)

	srv, s := NewServer(cfg.Name, cfg.Version, cat)
	attachStore(s, cfg.SemanticDir)
	return srv.Run(ctx, &mcp.StdioTransport{})
}

// applyDefaults fills the server identity defaults shared by every mode.
func (cfg *Config) applyDefaults() {
	if cfg.Name == "" {
		cfg.Name = "mcp-vst-sidechain"
	}
	if cfg.Version == "" {
		cfg.Version = "0.1.0"
	}
}

// attachStore binds the persistent semantic store to a session so a param probed in a past session is recalled
// instead of re-swept and agent annotations accumulate across runs. A load error (a corrupt file) is non-fatal:
// the server runs in-memory. Shared by external and managed modes.
func attachStore(s *session, semanticDir string) {
	store := NewSemanticStore(orDefault(semanticDir, defaultSemanticDir()))
	if err := s.attachStore(store); err != nil {
		fmt.Fprintf(os.Stderr, "sidechain: semantic store disabled (%v)\n", err)
	} else {
		fmt.Fprintf(os.Stderr, "sidechain: semantic store %s (fingerprint %s)\n", store.dir, s.entry.Fingerprint[:19])
	}
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
	registerGovernedTools(srv, s)
	registerSemanticTools(srv, s)
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
