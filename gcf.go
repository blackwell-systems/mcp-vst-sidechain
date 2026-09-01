// gcf.go - encode the MODEL-FACING output of the big read tools as GCF (blackwell-systems/gcf-go), the
// token-compact wire format, instead of JSON. This is where GCF earns its keep: a plugin with hundreds of
// parameters is a token nightmare to serialise into an agent's context as JSON; GCF is 50-92% smaller,
// comprehended zero-shot. It is ORTHOGONAL to the plugin control socket, which stays plain JSON (no model
// reads that channel and the C++ side has no GCF decoder).
//
// Scope: only the model-facing tool RESULT payload (e.g. list_params). Small tools (get_param, set_param, note
// verbs) keep their short human text. Degrades safely: if GCF is disabled (SIDECHAIN_MCP_FORMAT=json) or an
// encode hits the numeric-domain guard (EncodeGenericChecked returns an error), it falls back to the JSON
// StructuredContent path, so a bad value never panics the long-running server.

package sidechain

import (
	"os"
	"strings"

	gcf "github.com/blackwell-systems/gcf-go"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// gcfEnabled reports whether tool output should be GCF-encoded. Default ON; SIDECHAIN_MCP_FORMAT=json (or
// =0/off) forces the legacy JSON StructuredContent path for a client that wants raw JSON.
func gcfEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SIDECHAIN_MCP_FORMAT"))) {
	case "json", "0", "off", "false":
		return false
	default:
		return true
	}
}

// structuredResult is the return for a read tool whose payload is a large structured value. When GCF is on and
// the value encodes cleanly, it returns the human `summary` plus a GCF block as the tool's TEXT content and NO
// StructuredContent (so there is no parallel JSON copy inflating the model's token bill). Otherwise it falls
// back to `summary` text + `payload` as JSON StructuredContent (the prior behaviour).
func structuredResult(summary string, payload any) (*mcp.CallToolResult, any, error) {
	if gcfEnabled() {
		if enc, err := gcf.EncodeGenericChecked(payload); err == nil {
			text := summary
			if enc != "" {
				text = summary + "\n\n" + enc
			}
			return textResult(text), nil, nil
		}
		// EncodeGenericChecked failed (out-of-int64-domain value) - fall through to JSON.
	}
	return textResult(summary), payload, nil
}
