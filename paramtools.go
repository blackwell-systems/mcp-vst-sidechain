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
	"math"
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
	infer   map[string]ParamInference // Phase-1 real-unit inferences, probed on demand, keyed by param id
}

func newSession(cat ParamCatalog) *session {
	return &session{catalog: cat, params: map[string]float64{}, infer: map[string]ParamInference{}}
}

// probe sweeps a param's value text on the live plugin, infers its real-unit semantics, caches, and returns
// both the raw samples (for display) and the inference. Requires a live connection. Caller holds s.mu.
func (s *session) probe(id string) ([]ValueSample, ParamInference, error) {
	if s.live == nil {
		return nil, ParamInference{}, fmt.Errorf("not live")
	}
	samples, err := s.live.SampleText(id, nil)
	if err != nil {
		return nil, ParamInference{}, err
	}
	pi := inferParam(samples)
	if s.infer == nil {
		s.infer = map[string]ParamInference{}
	}
	s.infer[id] = pi
	return samples, pi, nil
}

// inference returns a cached inference or probes once. Caller holds s.mu.
func (s *session) inference(id string) (ParamInference, error) {
	if pi, ok := s.infer[id]; ok {
		return pi, nil
	}
	_, pi, err := s.probe(id)
	return pi, err
}

// textResult wraps a plain-text tool reply.
func textResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

type emptyIn struct{}

// ---- input types ----

type listParamsIn struct {
	Group  string `json:"group,omitempty" jsonschema:"filter by group/category. Normally best-effort plugin metadata; when a plugin exposes no groups these are DERIVED from shared label prefixes (e.g. 'Filter', 'Amp', 'Osc') so a flat catalog is still pageable by section. Call list_params with no args to see the available group names."`
	Filter string `json:"filter,omitempty" jsonschema:"case-insensitive substring on the param id OR label (a large plugin's catalog is best paged)"`
}

type getParamIn struct {
	ID string `json:"id" jsonschema:"the plugin parameter id"`
}

type setParamIn struct {
	ID         string   `json:"id" jsonschema:"the plugin parameter id to set"`
	Value      *float64 `json:"value,omitempty" jsonschema:"value: for a hasRealRange param, real units (Hz, dB); for a hosted param, the normalized 0..1 or discrete index. Mutually exclusive with normalized/choice."`
	Normalized *float64 `json:"normalized,omitempty" jsonschema:"value as normalized 0..1 (mapped to the real range). Mutually exclusive with value/choice."`
	Choice     string   `json:"choice,omitempty" jsonschema:"for choice params: the choice NAME (case-insensitive). Mutually exclusive with value/normalized."`
	Real       *float64 `json:"real,omitempty" jsonschema:"target in REAL units (Hz/dB/ms/etc.) for a live HOSTED param that has no real range in the catalog. The bridge probes the plugin's value text to map it to the right position. Run describe_param first to learn the unit/range/curve. Live only; mutually exclusive with value/normalized/choice."`
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
		line := fmt.Sprintf("LIVE %s = %s (%s)", p.ID, formatFloat(val), txt)
		// If we have already probed this param and it turned out to be a discrete-as-float control, surface the
		// observed labels so the agent knows it can drive it with choice=. (We do NOT probe here, only reuse a
		// cached inference; list_params deliberately does not do this, as it would probe every param.)
		if pi, ok := s.infer[p.ID]; ok && !pi.Numeric && len(pi.Labels) > 0 {
			line += fmt.Sprintf(" [discrete; labels: %s; set with choice=]", strings.Join(pi.Labels, ", "))
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: line}}}, out, nil
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

	if in.Real != nil {
		return s.setParamReal(p, *in.Real, in)
	}

	// choice= on a param the catalog does NOT type as "choice": many plugins expose a discrete control (a filter
	// type, an on/off toggle) as a plain float. If it probes as discrete with a matching label, set it by label.
	// The real-catalog "choice" path is left to resolveReal below.
	if strings.TrimSpace(in.Choice) != "" && p.Type != "choice" {
		return s.setParamDiscreteChoice(p, in.Choice, in)
	}

	real, errMsg := resolveReal(p, in.Value, in.Normalized, in.Choice)
	if errMsg != "" {
		return textResult("set_param: " + errMsg), nil, nil
	}

	extra := ""
	if p.Type == "choice" && int(real+0.5) >= 0 && int(real+0.5) < len(p.Choices) {
		extra = " (" + p.Choices[int(real+0.5)] + ")"
	}

	// LIVE: forward to the running instance. Do NOT mutate the headless session params - the plugin is
	// authoritative while live. For a real-value set on a HasRealRange param, forward the REAL value and let the
	// plugin apply its own (possibly skewed) curve; otherwise forward a normalized 0..1 (which round-trips
	// exactly, since a normalized/choice input never went through a curve the Go layer does not own).
	if s.live != nil {
		v, isReal := liveArg(p, real, in.Value != nil)
		val, _, _, err := s.live.SetParam(p.ID, v, isReal)
		if err != nil {
			return textResult("set_param (live) failed: " + err.Error()), nil, nil
		}
		return textResult(fmt.Sprintf("Set LIVE %s = %s%s (applied %s).", p.ID, formatFloat(real), extra, formatFloat(val))), nil, nil
	}

	s.params[p.ID] = real
	return textResult(fmt.Sprintf("Set %s = %s%s.", p.ID, formatFloat(real), extra)), nil, nil
}

// setParamReal handles a real-unit set (in.Real) by mapping the target through the param's probed value text.
// This is what lets an agent say "cutoff = 1000 Hz" on a hosted plugin whose catalog range is a bare 0..1.
// Caller holds s.mu.
func (s *session) setParamReal(p *ParamDef, target float64, in setParamIn) (*mcp.CallToolResult, any, error) {
	if in.Value != nil || in.Normalized != nil || strings.TrimSpace(in.Choice) != "" {
		return textResult("set_param: 'real' is mutually exclusive with value/normalized/choice."), nil, nil
	}
	if s.live == nil {
		return textResult("set_param: a real-unit set needs a live plugin (it maps units from the plugin's value text). connect_live first, or use normalized 0..1."), nil, nil
	}
	pi, err := s.inference(p.ID)
	if err != nil {
		return textResult("set_param (real): probe failed: " + err.Error()), nil, nil
	}
	if !pi.Numeric {
		return textResult(fmt.Sprintf("set_param: %q is discrete (%s); use choice or normalized, not real.", p.ID, strings.Join(pi.Labels, "/"))), nil, nil
	}
	if pi.Unit == "" {
		return textResult(fmt.Sprintf("set_param: %q exposes no real unit (its value text is just the number); use normalized 0..1.", p.ID)), nil, nil
	}
	if _, ok := pi.NormForReal(target); !ok {
		return textResult("set_param: could not map that real value to a normalized position."), nil, nil
	}
	var (
		norm float64
		text string
		how  string
	)
	if pi.analyticReliable() {
		// Closed-form inverse: one set, no search round-trips.
		norm, _ = pi.NormForReal(target)
		_, _, t, err := s.live.SetParam(p.ID, norm, false)
		if err != nil {
			return textResult("set_param (real) failed: " + err.Error()), nil, nil
		}
		text, how = t, pi.Fit.Model+" fit"
	} else {
		// No clean model: binary-search refine for exactness on the awkward curve.
		var err error
		norm, text, err = s.refineToReal(p.ID, pi, target)
		if err != nil {
			return textResult("set_param (real) failed: " + err.Error()), nil, nil
		}
		how = "sampled+refined"
	}
	return textResult(fmt.Sprintf("Set LIVE %s to ~%s %s (norm %.4f, %s; plugin reports %q).", p.ID, formatFloat(target), pi.Unit, norm, how, text)), nil, nil
}

// setParamDiscreteChoice sets a discrete-hiding-as-float param by label. Many plugins expose an enum/toggle as
// a plain type: float whose value text renders labels (e.g. "LP"/"BP"/"HP", "Off"/"On"); the catalog cannot see
// this, so the ordinary choice path (resolveReal) rejects it. When the param probes as discrete and one of its
// observed labels matches (case-insensitive), forward that label's representative normalized value. Live only;
// requires the inference to be discrete. Caller holds s.mu.
func (s *session) setParamDiscreteChoice(p *ParamDef, choice string, in setParamIn) (*mcp.CallToolResult, any, error) {
	if in.Value != nil || in.Normalized != nil || in.Real != nil {
		return textResult("set_param: 'choice' is mutually exclusive with value/normalized/real."), nil, nil
	}
	if s.live == nil {
		return textResult(fmt.Sprintf("set_param: %q is not a catalog choice param; setting it by label needs a live plugin (it reads the plugin's value text to map the label). connect_live first, or use normalized 0..1.", p.ID)), nil, nil
	}
	pi, err := s.inference(p.ID)
	if err != nil {
		return textResult("set_param (choice): probe failed: " + err.Error()), nil, nil
	}
	if pi.Numeric || len(pi.discreteNorms) == 0 {
		return textResult(fmt.Sprintf("set_param: %q does not probe as a discrete control, so choice= does not apply; use value/normalized/real.", p.ID)), nil, nil
	}
	norm, label, ok := lookupDiscreteNorm(pi, choice)
	if !ok {
		return textResult(fmt.Sprintf("set_param: %q is not an observed label for %q. Observed: %s", choice, p.ID, strings.Join(pi.Labels, ", "))), nil, nil
	}
	_, _, text, err := s.live.SetParam(p.ID, norm, false)
	if err != nil {
		return textResult("set_param (choice) failed: " + err.Error()), nil, nil
	}
	match := ""
	if strings.EqualFold(strings.TrimSpace(text), label) {
		match = " (plugin confirms)"
	} else {
		match = fmt.Sprintf(" (plugin reports %q)", text)
	}
	return textResult(fmt.Sprintf("Set LIVE %s to %q (norm %.4f)%s.", p.ID, label, norm, match)), nil, nil
}

// lookupDiscreteNorm resolves a label (case-insensitive) to its representative norm in a discrete inference,
// returning the canonical (observed) label spelling alongside.
func lookupDiscreteNorm(pi ParamInference, choice string) (norm float64, label string, ok bool) {
	choice = strings.TrimSpace(choice)
	for lbl, n := range pi.discreteNorms {
		if strings.EqualFold(lbl, choice) {
			return n, lbl, true
		}
	}
	return 0, "", false
}

const refineMaxIters = 16

// refineToReal lands a param on a real-unit target as exactly as the plugin allows. It seeds with the sampled
// piecewise-linear estimate (one round-trip; exact for linear params), then, if that is not close enough,
// binary-searches the norm within the sample bracket using live getText readbacks. This is curve-agnostic: it
// works on a steep log/exp region where the piecewise-linear seed alone is coarse. Bounded round-trips. Caller
// holds s.mu; the param is left at the best norm found (no restore - landing there is the point).
func (s *session) refineToReal(id string, pi ParamInference, target float64) (norm float64, text string, err error) {
	tol := math.Abs(pi.RealMax-pi.RealMin) * 1e-4
	if tol <= 0 {
		tol = 1e-9
	}
	// set a normalized value, read the plugin's rendered value back, folded into the base unit.
	read := func(n float64) (float64, string, error) {
		_, _, txt, e := s.live.SetParam(id, n, false)
		if e != nil {
			return 0, "", e
		}
		v, u, ok := parseValueText(txt)
		if !ok {
			return 0, txt, fmt.Errorf("non-numeric readback %q", txt)
		}
		fv, _ := normalizeUnit(v, u)
		return fv, txt, nil
	}

	norm, _ = pi.NormForReal(target)
	got, text, err := read(norm)
	if err != nil || math.Abs(got-target) <= tol {
		return norm, text, err
	}
	lo, hi, ok := pi.bracket(target)
	if !ok {
		return norm, text, nil // out of sampled range: keep the clamped seed
	}
	asc := pi.RealMax >= pi.RealMin
	for i := 0; i < refineMaxIters; i++ {
		mid := (lo + hi) / 2
		got, text, err = read(mid)
		if err != nil {
			return mid, text, err
		}
		norm = mid
		if math.Abs(got-target) <= tol {
			return norm, text, nil
		}
		if (got < target) == asc {
			lo = mid
		} else {
			hi = mid
		}
	}
	return norm, text, nil
}

type describeParamIn struct {
	ID string `json:"id" jsonschema:"the plugin parameter id to probe"`
}

// handleDescribeParam probes a live param's value text across its range and returns the recovered real-unit
// semantics (unit/range/curve/bipolar, or discrete labels). This is the agent's way to learn what a hosted
// param actually is before driving it by real value. Live only; the result is cached for set_param real=.
func (s *session) handleDescribeParam(ctx context.Context, _ *mcp.CallToolRequest, in describeParamIn) (*mcp.CallToolResult, any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.catalog.Get(in.ID)
	if p == nil {
		return textResult(fmt.Sprintf("describe_param: unknown id %q. Use list_params to discover ids.", in.ID)), nil, nil
	}
	if s.live == nil {
		return textResult("describe_param: not live. It reads the plugin's value text across the range, so connect_live first."), nil, nil
	}
	samples, pi, err := s.probe(in.ID)
	if err != nil {
		return textResult("describe_param failed: " + err.Error()), nil, nil
	}
	out := struct {
		ID        string         `json:"id"`
		Label     string         `json:"label"`
		Inference ParamInference `json:"inference"`
		Samples   []ValueSample  `json:"samples"`
	}{ID: p.ID, Label: p.Label, Inference: pi, Samples: samples}
	return textResult(pi.summary(p.ID, p.Label)), out, nil
}

// summary renders a one-line human description of an inference.
func (pi ParamInference) summary(id, label string) string {
	switch {
	case !pi.Numeric && len(pi.Labels) > 0:
		return fmt.Sprintf("%s (%s): discrete; values: %s. Set via choice or normalized.", id, label, strings.Join(pi.Labels, ", "))
	case !pi.Numeric:
		return fmt.Sprintf("%s (%s): no readable value text; normalized 0..1 only.", id, label)
	case pi.Unit == "":
		return fmt.Sprintf("%s (%s): numeric but unitless (plugin renders the raw number); normalized 0..1 only.", id, label)
	default:
		bip := ""
		if pi.Bipolar {
			bip = ", bipolar"
		}
		fit := "sampled" // analytic model unavailable/untrusted: inversion uses the dense samples (+ refine on single set)
		if pi.analyticReliable() {
			fit = fmt.Sprintf("%s fit ±%.2f%%", pi.Fit.Model, pi.Fit.MaxRelErr*100)
		}
		return fmt.Sprintf("%s (%s): %s %s..%s (%s%s; %s). Set by real value with real=<n>.", id, label, pi.Unit,
			formatFloat(pi.RealMin), formatFloat(pi.RealMax), pi.Curve, bip, fit)
	}
}

// ---- set_params: BATCH write (the LLM -> plugin direction; JSON or GCF input) ----

type setParamsRow struct {
	ID         string   `json:"id"`
	Value      *float64 `json:"value,omitempty"`
	Normalized *float64 `json:"normalized,omitempty"`
	Choice     string   `json:"choice,omitempty"`
	Real       *float64 `json:"real,omitempty"` // real-unit target for a live hosted param (analytic inversion; live only)
}

type setParamsIn struct {
	Params []setParamsRow `json:"params,omitempty" jsonschema:"array of {id, value|normalized|choice|real} - set many params in ONE call. 'real' targets real units on a live hosted param (analytic inversion; describe_param it first). Provide this OR gcf."`
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
		// Real-unit row: map via the analytic fit (no per-set search). Requires a live plugin; the first touch
		// of each param probes its value text (cached), so a repeated batch is cheap. No refinement here - a
		// param that needs it can be single-set with set_param real=.
		if r.Real != nil {
			if s.live == nil {
				skipped++
				skips = append(skips, r.ID+"(real-needs-live)")
				continue
			}
			pi, err := s.inference(r.ID)
			if err != nil {
				skipped++
				skips = append(skips, r.ID+"(probe-fail)")
				continue
			}
			norm, ok := pi.NormForReal(*r.Real)
			if !pi.Numeric || pi.Unit == "" || !ok {
				skipped++
				skips = append(skips, r.ID+"(no-unit)")
				continue
			}
			if _, _, _, err := s.live.SetParam(r.ID, norm, false); err != nil {
				skipped++
				skips = append(skips, r.ID+"(live-fail)")
				continue
			}
			applied++
			continue
		}
		real, errMsg := resolveReal(p, r.Value, r.Normalized, r.Choice)
		if errMsg != "" {
			skipped++
			skips = append(skips, r.ID+"("+shortReason(errMsg)+")")
			continue
		}
		if s.live != nil {
			v, isReal := liveArg(p, real, r.Value != nil)
			if _, _, _, err := s.live.SetParam(p.ID, v, isReal); err != nil {
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

// liveArg decides how to forward a resolved value to the live endpoint. For a HasRealRange param whose input
// was a real `value`, it forwards the REAL value (isReal=true) so the plugin applies its own (possibly skewed)
// real->normalized curve; for everything else - a hosted param, or a normalized/choice input - it forwards a
// normalized 0..1, which the Go layer computes exactly because no plugin-owned curve is involved.
func liveArg(p *ParamDef, real float64, fromValue bool) (v float64, isReal bool) {
	if p.HasRealRange && fromValue {
		return real, true
	}
	return p.realToNorm(real), false
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
	mcp.AddTool(srv, &mcp.Tool{Name: "get_param", Description: "Get one parameter's definition + current value by id (value plus normalized 0..1). For a hasRealRange param, value is in real units; otherwise value is the normalized 0..1 (or discrete index) and the human units are in the plugin's own text. Reads the live instance when connected."},
		func(ctx context.Context, r *mcp.CallToolRequest, in getParamIn) (*mcp.CallToolResult, any, error) {
			sync()
			return s.handleGetParam(ctx, r, in)
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "set_param", Description: "Set one parameter by id: value (for a hasRealRange param, real units such as Hz/dB; for a hosted param, the normalized 0..1 or discrete index), normalized (always 0..1), choice NAME (a catalog choice param, or a live hosted param that probes as discrete: a filter type or on/off toggle exposed as a plain float, matched against the labels in its value text), or real (target in real units for a live hosted param, mapped via its value text - run describe_param first). Validated + clamped against the plugin's range/choices."},
		func(ctx context.Context, r *mcp.CallToolRequest, in setParamIn) (*mcp.CallToolResult, any, error) {
			sync()
			return s.handleSetParam(ctx, r, in)
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "describe_param", Description: "Probe a LIVE param to recover its real-unit meaning from the plugin's own value text: unit (Hz/dB/ms/%/semitones), real range, curve (linear/log/exp), whether it is bipolar, or the labels if it is really a discrete control. Use this to learn a hosted param before set_param real=. Live only."},
		func(ctx context.Context, r *mcp.CallToolRequest, in describeParamIn) (*mcp.CallToolResult, any, error) {
			sync()
			return s.handleDescribeParam(ctx, r, in)
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "set_params", Description: "Set MANY params in ONE call (author a whole patch at once). Provide params[] (JSON array of {id, value|normalized|choice}) OR gcf (a token-compact GCF table with the same rows). Applies to the live instance when connected, else the session. Unknown/invalid rows are skipped and reported, never fatal."},
		func(ctx context.Context, r *mcp.CallToolRequest, in setParamsIn) (*mcp.CallToolResult, any, error) {
			sync()
			return s.handleSetParams(ctx, r, in)
		})
}
