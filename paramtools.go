// paramtools.go - the generic MCP param surface: list_params, get_param, set_param, set_params. These operate
// on a ParamCatalog (validate + clamp against the plugin's real ranges/choices) and, when connected, forward
// to a LiveEndpoint (the running instance). This is the reusable core of Sidechain: any host that can supply a
// catalog + a live endpoint gets an agent-drivable parameter surface for free.
//
// The batch set_params path accepts JSON rows OR a token-compact GCF table (the LLM -> plugin direction):
// authoring a whole patch is ONE call, not one-per-parameter.

package sidechain

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"

	gcf "github.com/blackwell-systems/gcf-go"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// session holds the state shared across tool calls: the catalog (the loaded plugin's param set), the headless
// session param values (real units, keyed by ID; used when NOT connected live), and the current live endpoint
// (nil => headless). One session per server process.
type session struct {
	mu      sync.Mutex
	catalog ParamCatalog
	params  map[string]float64
	live    LiveEndpoint
}

func newSession(cat ParamCatalog) *session {
	return &session{catalog: cat, params: map[string]float64{}}
}

// textResult wraps a plain-text tool reply.
func textResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

type emptyIn struct{}

// ---- input types ----

type listParamsIn struct {
	Group  string `json:"group,omitempty" jsonschema:"filter by group/category (best-effort plugin metadata; may be 'other')"`
	Filter string `json:"filter,omitempty" jsonschema:"case-insensitive substring on the param id OR label (a large plugin's catalog is best paged)"`
}

type getParamIn struct {
	ID string `json:"id" jsonschema:"the plugin parameter id"`
}

type setParamIn struct {
	ID         string   `json:"id" jsonschema:"the plugin parameter id to set"`
	Value      *float64 `json:"value,omitempty" jsonschema:"value in REAL units (Hz, dB, index for choices, 0/1 for bool). Mutually exclusive with normalized/choice."`
	Normalized *float64 `json:"normalized,omitempty" jsonschema:"value as normalized 0..1 (mapped to the real range). Mutually exclusive with value/choice."`
	Choice     string   `json:"choice,omitempty" jsonschema:"for choice params: the choice NAME (case-insensitive). Mutually exclusive with value/normalized."`
}

// ---- handlers ----

func (s *session) handleListParams(ctx context.Context, _ *mcp.CallToolRequest, in listParamsIn) (*mcp.CallToolResult, any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	matches := s.catalog.Filter(in.Group, in.Filter)
	groups := s.catalog.Groups()
	total := len(s.catalog.All())
	out := struct {
		Count  int        `json:"count"`
		Total  int        `json:"total"`
		Groups []string   `json:"groups"`
		Params []ParamDef `json:"params"`
	}{Count: len(matches), Total: total, Groups: groups, Params: matches}

	txt := fmt.Sprintf("%d/%d params (groups: %s).", len(matches), total, strings.Join(groups, ", "))
	if in.Group == "" && in.Filter == "" {
		txt += " Pass group= or filter= to page this list."
	}
	return structuredResult(txt, out)
}

func (s *session) handleGetParam(ctx context.Context, _ *mcp.CallToolRequest, in getParamIn) (*mcp.CallToolResult, any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.catalog.Get(in.ID)
	if p == nil {
		return textResult(fmt.Sprintf("get_param: unknown id %q. Use list_params to discover ids.", in.ID)), nil, nil
	}
	// LIVE: read the value straight off the running instance (authoritative when connected).
	if s.live != nil {
		val, norm, txt, err := s.live.GetParam(p.ID)
		if err != nil {
			return textResult("get_param (live) failed: " + err.Error()), nil, nil
		}
		out := struct {
			Param      ParamDef `json:"param"`
			Value      float64  `json:"value"`
			Normalized float64  `json:"normalized"`
			IsSet      bool     `json:"isSet"`
			Live       bool     `json:"live"`
		}{Param: *p, Value: val, Normalized: norm, IsSet: true, Live: true}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("LIVE %s = %s (%s)", p.ID, formatFloat(val), txt)}}}, out, nil
	}

	cur, isSet := s.params[in.ID]
	if !isSet {
		cur = p.Default
	}
	out := struct {
		Param      ParamDef `json:"param"`
		Value      float64  `json:"value"`
		Normalized float64  `json:"normalized"`
		IsSet      bool     `json:"isSet"`
	}{Param: *p, Value: cur, Normalized: p.realToNorm(cur), IsSet: isSet}

	extra := ""
	if p.Type == "choice" && int(cur+0.5) >= 0 && int(cur+0.5) < len(p.Choices) {
		extra = " (" + p.Choices[int(cur+0.5)] + ")"
	}
	txt := fmt.Sprintf("%s = %s%s [%s %v..%v]%s", p.ID, formatFloat(cur), extra, p.Type, p.Min, p.Max,
		map[bool]string{true: "", false: " (default, unset)"}[isSet])
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: txt}}}, out, nil
}

func (s *session) handleSetParam(ctx context.Context, _ *mcp.CallToolRequest, in setParamIn) (*mcp.CallToolResult, any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.catalog.Get(in.ID)
	if p == nil {
		return textResult(fmt.Sprintf("set_param: unknown id %q. Use list_params to discover ids.", in.ID)), nil, nil
	}

	real, errMsg := resolveReal(p, in.Value, in.Normalized, in.Choice)
	if errMsg != "" {
		return textResult("set_param: " + errMsg), nil, nil
	}

	extra := ""
	if p.Type == "choice" && int(real+0.5) >= 0 && int(real+0.5) < len(p.Choices) {
		extra = " (" + p.Choices[int(real+0.5)] + ")"
	}

	// LIVE: forward as a NORMALIZED set (the plugin denormalises against its own real range, so no unit drift).
	// Do NOT mutate the headless session params - the plugin is authoritative while live.
	if s.live != nil {
		val, _, _, err := s.live.SetParam(p.ID, p.realToNorm(real))
		if err != nil {
			return textResult("set_param (live) failed: " + err.Error()), nil, nil
		}
		return textResult(fmt.Sprintf("Set LIVE %s = %s%s (applied %s).", p.ID, formatFloat(real), extra, formatFloat(val))), nil, nil
	}

	s.params[p.ID] = real
	return textResult(fmt.Sprintf("Set %s = %s%s.", p.ID, formatFloat(real), extra)), nil, nil
}

// ---- set_params: BATCH write (the LLM -> plugin direction; JSON or GCF input) ----

type setParamsRow struct {
	ID         string   `json:"id"`
	Value      *float64 `json:"value,omitempty"`
	Normalized *float64 `json:"normalized,omitempty"`
	Choice     string   `json:"choice,omitempty"`
}

type setParamsIn struct {
	Params []setParamsRow `json:"params,omitempty" jsonschema:"array of {id, value|normalized|choice} - set many params in ONE call. Provide this OR gcf."`
	GCF    string         `json:"gcf,omitempty" jsonschema:"a GCF-encoded array of the same rows (token-compact; emit it from a template). Provide this OR params. Template:\n## [N]{id,value,choice}\ncutoff|800|~\nfilterType|~|Ladder\n(one row per param, | separated, ~ = omit that column; value in real units, choice = the menu NAME. The 'GCF profile=generic' header is optional - it is prepended if missing.)"`
}

// handleSetParams applies MANY params in one call, from a JSON array (params) or a GCF string (gcf). When live,
// each row forwards to the running instance; otherwise it mutates the headless session. Unknown/invalid rows
// are skipped and reported, never fatal (a partial patch still applies).
func (s *session) handleSetParams(ctx context.Context, _ *mcp.CallToolRequest, in setParamsIn) (*mcp.CallToolResult, any, error) {
	rows := in.Params
	if strings.TrimSpace(in.GCF) != "" {
		payload := in.GCF
		if !strings.HasPrefix(strings.TrimSpace(payload), "GCF") {
			payload = "GCF profile=generic\n" + payload // lenient: accept the bare table the model emits
		}
		dec, err := gcf.DecodeGeneric(payload)
		if err != nil {
			return textResult("set_params: GCF did not decode: " + err.Error()), nil, nil
		}
		b, _ := json.Marshal(dec)
		if err := json.Unmarshal(b, &rows); err != nil {
			return textResult("set_params: GCF payload is not an array of {id,...}: " + err.Error()), nil, nil
		}
	}
	if len(rows) == 0 {
		return textResult("set_params: provide params[] (JSON) or gcf (a GCF table)."), nil, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	var applied, skipped int
	var skips []string
	for _, r := range rows {
		p := s.catalog.Get(r.ID)
		if p == nil {
			skipped++
			skips = append(skips, r.ID+"(unknown)")
			continue
		}
		real, errMsg := resolveReal(p, r.Value, r.Normalized, r.Choice)
		if errMsg != "" {
			skipped++
			skips = append(skips, r.ID+"("+shortReason(errMsg)+")")
			continue
		}
		if s.live != nil {
			if _, _, _, err := s.live.SetParam(p.ID, p.realToNorm(real)); err != nil {
				skipped++
				skips = append(skips, r.ID+"(live-fail)")
				continue
			}
		} else {
			s.params[p.ID] = real
		}
		applied++
	}

	dest := "session"
	if s.live != nil {
		dest = "LIVE instance"
	}
	msg := fmt.Sprintf("set_params: applied %d to the %s, skipped %d.", applied, dest, skipped)
	if len(skips) > 0 {
		msg += " Skipped: " + strings.Join(skips, ", ")
	}
	return textResult(msg), nil, nil
}

// resolveReal validates exactly one of {value, normalized, choice} against a ParamDef and returns the clamped
// real-unit value, or a non-empty error message.
func resolveReal(p *ParamDef, value, normalized *float64, choice string) (float64, string) {
	n := 0
	if value != nil {
		n++
	}
	if normalized != nil {
		n++
	}
	if strings.TrimSpace(choice) != "" {
		n++
	}
	if n != 1 {
		return 0, "provide exactly one of value / normalized / choice."
	}
	switch {
	case strings.TrimSpace(choice) != "":
		if p.Type != "choice" {
			return 0, fmt.Sprintf("%q is not a choice param (type %s); use value or normalized.", p.ID, p.Type)
		}
		idx, ok := p.choiceIndex(choice)
		if !ok {
			return 0, fmt.Sprintf("%q is not a valid choice for %s. Choices: %s", choice, p.ID, strings.Join(p.Choices, ", "))
		}
		return float64(idx), ""
	case normalized != nil:
		return p.normToReal(*normalized), ""
	default:
		return p.clampReal(*value), ""
	}
}

// shortReason maps a verbose resolveReal message to a compact skip tag for the batch report.
func shortReason(msg string) string {
	switch {
	case strings.Contains(msg, "exactly one"):
		return "no-value"
	case strings.Contains(msg, "not a choice param"):
		return "not-choice"
	case strings.Contains(msg, "not a valid choice"):
		return "bad-choice"
	default:
		return "invalid"
	}
}

// formatFloat prints a float without a trailing ".000000", integers as integers.
func formatFloat(v float64) string {
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', 6, 64)
}

// RegisterParamTools wires the generic parameter tools (list/get/set_param, set_params) onto an MCP server,
// bound to a catalog and an optional live endpoint. A host calls this alongside any of its own tools - the two
// tool sets compose on one server. `live` returns the current LiveEndpoint (or nil for headless); it is a
// function so a host can swap the endpoint at runtime (connect_live / disconnect_live).
//
// This is the extension seam: Sidechain's headless server calls it with its own session; a different host
// (e.g. an in-DAW wrapper plugin) can supply its own catalog + endpoint and reuse the exact same tool surface.
func RegisterParamTools(srv *mcp.Server, cat ParamCatalog, live func() LiveEndpoint) {
	s := newSession(cat)
	registerParamToolsOn(srv, s, live)
}

// registerParamToolsOn wires the four generic tools onto an existing session. Before each dispatch it mirrors
// the host-supplied live accessor into s.live (under the session mutex), so the tools transparently forward to
// whatever endpoint the host currently has connected - or run headless when it returns nil. The headless
// server uses this directly with its own session (so its connect_live/disconnect_live drive the same field).
func registerParamToolsOn(srv *mcp.Server, s *session, live func() LiveEndpoint) {
	sync := func() {
		if live == nil {
			return
		}
		s.mu.Lock()
		s.live = live()
		s.mu.Unlock()
	}
	mcp.AddTool(srv, &mcp.Tool{Name: "list_params", Description: "List the loaded plugin's automatable parameters (id/label/type/range/choices/default/group). Optional group= and filter= to page a large catalog."},
		func(ctx context.Context, r *mcp.CallToolRequest, in listParamsIn) (*mcp.CallToolResult, any, error) {
			sync()
			return s.handleListParams(ctx, r, in)
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "get_param", Description: "Get one parameter's definition + current value (real units + normalized 0..1) by id. Reads the live instance when connected."},
		func(ctx context.Context, r *mcp.CallToolRequest, in getParamIn) (*mcp.CallToolResult, any, error) {
			sync()
			return s.handleGetParam(ctx, r, in)
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "set_param", Description: "Set one parameter by id, in REAL units (value), normalized 0..1 (normalized), or by choice NAME (choice). Validated against the plugin's range/choices."},
		func(ctx context.Context, r *mcp.CallToolRequest, in setParamIn) (*mcp.CallToolResult, any, error) {
			sync()
			return s.handleSetParam(ctx, r, in)
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "set_params", Description: "Set MANY params in ONE call (author a whole patch at once). Provide params[] (JSON array of {id, value|normalized|choice}) OR gcf (a token-compact GCF table with the same rows). Applies to the live instance when connected, else the session. Unknown/invalid rows are skipped and reported, never fatal."},
		func(ctx context.Context, r *mcp.CallToolRequest, in setParamsIn) (*mcp.CallToolResult, any, error) {
			sync()
			return s.handleSetParams(ctx, r, in)
		})
}
