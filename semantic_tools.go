// semantic_tools.go - the Phase 3 MCP tools over the persistent semantic store (semantic.go): annotate_params
// (the agent teaches the bridge what params MEAN), get_semantic_map (the whole current-plugin semantic view, the
// primary read for Phase 4), and forget_semantics. describe_param (paramtools.go) is the fourth touch point: it
// recalls a cached inference from the store instead of re-probing. The bridge never infers roles itself.

package sidechain

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type annotateRow struct {
	ID         string   `json:"id" jsonschema:"the plugin parameter id (from list_params)"`
	Role       string   `json:"role,omitempty" jsonschema:"a stable, free-form role reused across plugins, e.g. filter.cutoff, amp.attack, osc.pitch. Equivalence is string equality, so reusing a role makes it cross-plugin."`
	Aliases    []string `json:"aliases,omitempty" jsonschema:"alternate names an agent/user might use, e.g. [brightness, vcf freq]"`
	Polarity   string   `json:"polarity,omitempty" jsonschema:"what higher values do, e.g. 'higher = brighter'"`
	Section    string   `json:"section,omitempty" jsonschema:"a logical section/group name for the param"`
	Confidence float64  `json:"confidence,omitempty" jsonschema:"0..1 confidence in this annotation"`
	Notes      string   `json:"notes,omitempty"`
}

type annotateParamsIn struct {
	Params []annotateRow `json:"params" jsonschema:"the params to annotate; only provided fields are updated, omitted fields are preserved"`
}

// handleAnnotateParams merge-updates agent-authored semantics and persists them. Only provided fields change;
// omitted fields on an existing param are preserved. Works headless (annotations need no live plugin).
func (s *session) handleAnnotateParams(ctx context.Context, _ *mcp.CallToolRequest, in annotateParamsIn) (*mcp.CallToolResult, any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store == nil || s.entry == nil {
		return textResult("annotate_params: the semantic store is not enabled for this server."), nil, nil
	}
	applied, skipped := 0, 0
	for _, r := range in.Params {
		p := s.catalog.Get(r.ID)
		if p == nil {
			skipped++
			continue
		}
		sem := s.entry.param(r.ID)
		sem.Label = p.Label
		if r.Role != "" {
			sem.Role = r.Role
		}
		if r.Aliases != nil {
			sem.Aliases = r.Aliases
		}
		if r.Polarity != "" {
			sem.Polarity = r.Polarity
		}
		if r.Section != "" {
			sem.Section = r.Section
		}
		if r.Confidence != 0 {
			sem.Confidence = r.Confidence
		}
		if r.Notes != "" {
			sem.Notes = r.Notes
		}
		applied++
	}
	if merged, err := s.store.Save(s.entry); err == nil {
		s.entry = merged
	} else {
		return textResult("annotate_params: applied in memory but persist failed: " + err.Error()), nil, nil
	}
	return textResult(fmt.Sprintf("Annotated %d param(s) (skipped %d unknown); persisted to the semantic store.", applied, skipped)), nil, nil
}

// semanticRow is the agent-facing projection of one param's semantics (the raw inversion samples are omitted).
type semanticRow struct {
	ID            string   `json:"id"`
	Label         string   `json:"label,omitempty"`
	BehaviorClass string   `json:"behaviorClass,omitempty"`
	Unit          string   `json:"unit,omitempty"`
	RealMin       float64  `json:"realMin,omitempty"`
	RealMax       float64  `json:"realMax,omitempty"`
	Curve         string   `json:"curve,omitempty"`
	Role          string   `json:"role,omitempty"`
	Aliases       []string `json:"aliases,omitempty"`
	Polarity      string   `json:"polarity,omitempty"`
	Section       string   `json:"section,omitempty"`
	Notes         string   `json:"notes,omitempty"`
}

type semanticMapOut struct {
	Fingerprint string         `json:"fingerprint"`
	Plugin      PluginIdentity `json:"plugin"`
	Params      []semanticRow  `json:"params"`
}

// handleGetSemanticMap returns the whole current-plugin semantic entry (per-param inference summary + behavior
// class + annotations) in one call, sorted by id. This is the primary read for Phase 4 (intent -> params).
func (s *session) handleGetSemanticMap(ctx context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store == nil || s.entry == nil {
		return textResult("get_semantic_map: the semantic store is not enabled for this server."), nil, nil
	}
	ids := make([]string, 0, len(s.entry.Params))
	for id := range s.entry.Params {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	rows := make([]semanticRow, 0, len(ids))
	roles := 0
	probed := 0
	for _, id := range ids {
		sem := s.entry.Params[id]
		row := semanticRow{ID: id, Label: sem.Label, BehaviorClass: sem.BehaviorClass,
			Role: sem.Role, Aliases: sem.Aliases, Polarity: sem.Polarity, Section: sem.Section, Notes: sem.Notes}
		if sem.Inference != nil {
			row.Unit, row.RealMin, row.RealMax, row.Curve = sem.Inference.Unit, sem.Inference.RealMin, sem.Inference.RealMax, sem.Inference.Curve
			probed++
		}
		if sem.Role != "" {
			roles++
		}
		rows = append(rows, row)
	}
	out := semanticMapOut{Fingerprint: s.entry.Fingerprint, Plugin: s.entry.Plugin, Params: rows}
	name := s.entry.Plugin.Name
	if name == "" {
		name = "this plugin"
	}
	return textResult(fmt.Sprintf("Semantic map for %s: %d param(s) known (%d probed, %d with roles). Fingerprint %s.",
		name, len(rows), probed, roles, strings.TrimPrefix(s.entry.Fingerprint, "sha256:")[:12])), out, nil
}

// handleForgetSemantics drops the current plugin's stored entry (fingerprint file) and clears the in-memory view.
func (s *session) handleForgetSemantics(ctx context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store == nil || s.entry == nil {
		return textResult("forget_semantics: the semantic store is not enabled for this server."), nil, nil
	}
	fp := s.entry.Fingerprint
	if err := s.store.Forget(fp); err != nil {
		return textResult("forget_semantics failed: " + err.Error()), nil, nil
	}
	s.entry = &SemanticEntry{Fingerprint: fp, Plugin: identityOf(s.catalog), Params: map[string]*ParamSemantics{}}
	s.infer = map[string]ParamInference{}
	return textResult("Forgot the stored semantics for this plugin (inference cache cleared)."), nil, nil
}

// registerSemanticTools wires the Phase-3 store tools. Registered on every server; they report "not enabled" when
// no store is attached (e.g. a library embedding via RegisterParamTools).
func registerSemanticTools(srv *mcp.Server, s *session) {
	mcp.AddTool(srv, &mcp.Tool{Name: "annotate_params", Description: "Teach the bridge what parameters MEAN: attach a stable role (e.g. filter.cutoff), aliases, polarity, section, notes to params. Merge-updates (omitted fields preserved) and persists across sessions. Works headless."}, s.handleAnnotateParams)
	mcp.AddTool(srv, &mcp.Tool{Name: "get_semantic_map", Description: "The whole current-plugin semantic map: per-param behavior class (derived), unit/range/curve (when probed), and your annotations (role/aliases/polarity). The primary read for reasoning about a plugin."}, s.handleGetSemanticMap)
	mcp.AddTool(srv, &mcp.Tool{Name: "forget_semantics", Description: "Drop the stored semantics for the current plugin (its fingerprint entry) and clear the inference cache."}, s.handleForgetSemantics)
}
