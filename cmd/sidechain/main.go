// Command sidechain is the headless stdio MCP server for mcp-vst-sidechain: it exposes a hosted plugin's
// parameter catalog + realtime control to an AI agent over MCP, GCF-encoded with a JSON fallback.
//
// It reads a parameter catalog JSON (emitted by cpp/Host when it loaded the target VST3/AU) and serves the
// generic param tools + live verbs on stdio. Point an MCP client (Claude Code/Desktop) at this binary.
//
//	Build: go build -o sidechain ./cmd/sidechain
//	Run:   ./sidechain --catalog plugin-catalog.json      (or SIDECHAIN_CATALOG=plugin-catalog.json ./sidechain)
package main

import (
	"context"
	"flag"
	"log"
	"os"

	sidechain "github.com/blackwell-systems/mcp-vst-sidechain"
)

func main() {
	catalog := flag.String("catalog", os.Getenv("SIDECHAIN_CATALOG"),
		"path to the plugin parameter catalog JSON (emitted by cpp/Host at plugin load; or set SIDECHAIN_CATALOG)")
	flag.Parse()

	if err := sidechain.Run(context.Background(), sidechain.Config{CatalogPath: *catalog}); err != nil {
		log.Fatalf("sidechain: %v", err)
	}
}
