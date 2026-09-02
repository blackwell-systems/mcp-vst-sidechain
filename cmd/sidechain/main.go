// Command sidechain is the headless stdio MCP server for mcp-vst-sidechain: it exposes a hosted plugin's
// parameter catalog + realtime control to an AI agent over MCP, GCF-encoded with a JSON fallback.
//
// It reads a parameter catalog JSON (emitted by cpp/Host when it loaded the target VST3/AU) and serves the
// generic param tools + live verbs on stdio. Point an MCP client (Claude Code/Desktop) at this binary.
//
//	Build:    go build -o sidechain ./cmd/sidechain
//	Managed:  ./sidechain --plugin /path/to/Plugin.vst3    (spawns + supervises the host; auto-connects live)
//	External: ./sidechain --catalog plugin-catalog.json    (attach to an already-running host; agent connects)
package main

import (
	"context"
	"flag"
	"log"
	"os"

	sidechain "github.com/blackwell-systems/mcp-vst-sidechain"
)

func main() {
	plugin := flag.String("plugin", "",
		"MANAGED mode: VST3/AU path (or AU identifier) to host. sidechain spawns and supervises the host, waits for its catalog, and auto-connects live. Mutually exclusive with --catalog")
	catalog := flag.String("catalog", os.Getenv("SIDECHAIN_CATALOG"),
		"EXTERNAL mode: path to a catalog JSON emitted by an already-running host (the agent calls connect_live itself). Or set SIDECHAIN_CATALOG. Mutually exclusive with --plugin")
	hostBin := flag.String("host-bin", "",
		"managed mode: explicit path to the sidechain-host binary (default: discovered next to this executable, then on PATH)")
	port := flag.Int("port", 0,
		"managed mode: control port (0 = pick a free ephemeral port)")
	selftest := flag.Bool("selftest", false,
		"managed mode: run the managed startup, verify it (enumerate/connect/read/set), print a one-line result to stderr, and exit without serving MCP")
	semanticDir := flag.String("semantic-dir", os.Getenv("SIDECHAIN_SEMANTIC_DIR"),
		"directory for the persistent semantic store (per-fingerprint files); empty uses a per-user cache dir")
	flag.Parse()

	err := sidechain.Run(context.Background(), sidechain.Config{
		Plugin:      *plugin,
		CatalogPath: *catalog,
		HostBin:     *hostBin,
		Port:        *port,
		SelfTest:    *selftest,
		SemanticDir: *semanticDir,
	})
	if err != nil {
		log.Fatalf("sidechain: %v", err)
	}
}
