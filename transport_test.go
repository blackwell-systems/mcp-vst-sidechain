// transport_test.go - end-to-end through the real MCP transport. Unlike the handler-level tests, this stands up
// the actual mcp.Server over an in-memory transport and calls tools as a client would, so it exercises the
// layers the direct-handler tests skip: tool registration, JSON argument unmarshaling into the input structs,
// dispatch, and result encoding (including the GCF path for list_params). Headless - no plugin, no socket.

package sidechain

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPTransportRoundTrip(t *testing.T) {
	cat := NewCatalog([]ParamDef{
		{ID: "cutoff", Label: "Cutoff", Type: "float", Min: 20, Max: 20000, Default: 1000, Group: "filter"},
		{ID: "filterType", Label: "Filter Type", Type: "choice", Min: 0, Max: 2, Step: 1, Default: 0, Group: "filter",
			Choices: []string{"Lowpass", "Bandpass", "Ladder"}},
	})
	srv, _ := NewServer("test", "0.0.0", cat)

	ctx := context.Background()
	clientT, serverT := mcp.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, serverT) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer cs.Close()

	// The tools are registered and discoverable over the wire.
	lt, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	names := map[string]bool{}
	for _, tool := range lt.Tools {
		names[tool.Name] = true
	}
	for _, want := range []string{"list_params", "get_param", "set_param", "set_params", "describe_param"} {
		if !names[want] {
			t.Errorf("tool %q not registered over the transport", want)
		}
	}

	// list_params: exercises the structured/GCF result-encoding path through the transport.
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_params", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("list_params call: %v", err)
	}
	if res.IsError {
		t.Fatalf("list_params returned error content: %s", textOf(res))
	}
	if txt := textOf(res); !strings.Contains(txt, "2/2 params") {
		t.Fatalf("list_params text = %q, want it to mention 2/2 params", txt)
	}

	// set_param by choice, then read it back - both marshaled from JSON into the input structs by the SDK.
	sres, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "set_param",
		Arguments: map[string]any{"id": "filterType", "choice": "Ladder"}})
	if err != nil {
		t.Fatalf("set_param call: %v", err)
	}
	if sres.IsError {
		t.Fatalf("set_param error: %s", textOf(sres))
	}
	gres, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "get_param",
		Arguments: map[string]any{"id": "filterType"}})
	if err != nil {
		t.Fatalf("get_param call: %v", err)
	}
	if txt := textOf(gres); !strings.Contains(txt, "Ladder") {
		t.Fatalf("get_param after set choice = %q, want it to show Ladder", txt)
	}

	// Unknown id is reported as tool-error content, not a transport error.
	ures, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "get_param",
		Arguments: map[string]any{"id": "nope"}})
	if err != nil {
		t.Fatalf("get_param unknown call: %v", err)
	}
	if txt := textOf(ures); !strings.Contains(txt, "unknown id") {
		t.Fatalf("get_param unknown = %q, want an unknown-id message", txt)
	}
}
