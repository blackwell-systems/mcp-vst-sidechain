// catalog.go - the plugin parameter catalog: the enumerated automatable-parameter surface of a hosted VST3/AU
// (count, ids, labels, ranges, choices, defaults, groups) plus the pure param math (normalise/clamp/choice).
//
// Provenance differs from a fixed-catalog host: Sidechain does NOT embed a catalog JSON at build time. The
// catalog is enumerated from a loaded plugin at runtime (the C++ host walks AudioProcessor::getParameters()
// and hands the rows to the Go server). The ParamDef shape is generic - nothing here knows what plugin it came
// from. A JSON loader (loadCatalogJSON) is provided for the headless server and for tests.

package sidechain

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// ParamDef is one automatable plugin parameter. Values are in REAL (denormalised) units, matching what the
// plugin's own value-to-text formatter reports (e.g. cutoff = 1200 Hz). Group is best-effort metadata: a
// well-behaved plugin exposes a group/category, otherwise it is left empty ("other").
type ParamDef struct {
	ID      string   `json:"id"`
	Label   string   `json:"label"`
	Group   string   `json:"group"` // best-effort category; may be "" / "other"
	Type    string   `json:"type"`  // float/int/bool/choice
	Min     float64  `json:"min"`
	Max     float64  `json:"max"`
	Step    float64  `json:"step"`
	Default float64  `json:"default"`
	Choices []string `json:"choices,omitempty"`
}

// ParamCatalog is the read side the generic tools depend on. One concrete impl (*Catalog) is provided; a host
// may supply its own (e.g. a live view over a running plugin). See RegisterParamTools.
type ParamCatalog interface {
	Get(id string) *ParamDef // range/type/choices for validate + clamp (nil if unknown)
	Filter(group, substr string) []ParamDef
	Groups() []string
	All() []ParamDef
}

// Catalog is the whole parameter set, indexed by ID for O(1) lookup. StateRootTag/StateVersion are optional
// provenance a host may stamp on its state blobs; the generic bridge treats full state as opaque.
type Catalog struct {
	StateRootTag string
	StateVersion int
	Params       []ParamDef
	byID         map[string]*ParamDef
}

// NewCatalog builds a Catalog from an enumerated param list (the runtime path: the C++ host enumerated a loaded
// plugin and handed these rows over). It indexes by ID.
func NewCatalog(params []ParamDef) *Catalog {
	c := &Catalog{Params: params, byID: make(map[string]*ParamDef, len(params))}
	for i := range c.Params {
		if strings.TrimSpace(c.Params[i].Group) == "" {
			c.Params[i].Group = "other"
		}
		c.byID[c.Params[i].ID] = &c.Params[i]
	}
	return c
}

// loadCatalogJSON parses a catalog JSON blob (the shape the C++ host emits: {stateRootTag, stateVersion,
// params, count}). Fails loudly on an empty/corrupt catalog.
func loadCatalogJSON(data []byte) (*Catalog, error) {
	var raw struct {
		StateRootTag string     `json:"stateRootTag"`
		StateVersion int        `json:"stateVersion"`
		Params       []ParamDef `json:"params"`
		Count        int        `json:"count"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse catalog json: %w", err)
	}
	if len(raw.Params) == 0 {
		return nil, fmt.Errorf("catalog has no params (did the plugin fail to load / expose no automatable params?)")
	}
	c := NewCatalog(raw.Params)
	c.StateRootTag = orDefault(raw.StateRootTag, "PARAMS")
	c.StateVersion = raw.StateVersion
	return c, nil
}

// Get returns the ParamDef for an ID (or nil).
func (c *Catalog) Get(id string) *ParamDef { return c.byID[id] }

// All returns every param (unfiltered).
func (c *Catalog) All() []ParamDef { return c.Params }

// Groups returns the distinct group names in a stable order.
func (c *Catalog) Groups() []string {
	seen := map[string]bool{}
	var g []string
	for i := range c.Params {
		if !seen[c.Params[i].Group] {
			seen[c.Params[i].Group] = true
			g = append(g, c.Params[i].Group)
		}
	}
	sort.Strings(g)
	return g
}

// Filter returns params matching an optional group (exact, case-insensitive) AND/OR an id/label substring
// (case-insensitive). Empty filters return everything. Result is sorted by ID for determinism.
func (c *Catalog) Filter(group, substr string) []ParamDef {
	group = strings.ToLower(strings.TrimSpace(group))
	substr = strings.ToLower(strings.TrimSpace(substr))
	var out []ParamDef
	for _, p := range c.Params {
		if group != "" && strings.ToLower(p.Group) != group {
			continue
		}
		if substr != "" && !strings.Contains(strings.ToLower(p.ID), substr) &&
			!strings.Contains(strings.ToLower(p.Label), substr) {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// clampReal snaps a real-unit value into [min,max] and, for stepped params, quantises it to a legal step.
// (choice/int/bool are stepped by 1; float uses the registered interval when > 0.)
func (p *ParamDef) clampReal(v float64) float64 {
	if v < p.Min {
		v = p.Min
	}
	if v > p.Max {
		v = p.Max
	}
	if p.Step > 0 {
		n := (v - p.Min) / p.Step
		v = p.Min + roundHalfUp(n)*p.Step
		if v > p.Max {
			v = p.Max
		}
	}
	return v
}

// normToReal maps a 0..1 normalised value to real units (linear over [min,max]; stepped params still snap).
func (p *ParamDef) normToReal(n float64) float64 {
	if n < 0 {
		n = 0
	}
	if n > 1 {
		n = 1
	}
	return p.clampReal(p.Min + n*(p.Max-p.Min))
}

// realToNorm maps a real-unit value to 0..1 (linear over [min,max]).
func (p *ParamDef) realToNorm(v float64) float64 {
	if p.Max == p.Min {
		return 0
	}
	n := (v - p.Min) / (p.Max - p.Min)
	if n < 0 {
		n = 0
	}
	if n > 1 {
		n = 1
	}
	return n
}

// choiceIndex resolves a choice value: a name (case-insensitive) -> its index. Returns (-1, false) otherwise.
func (p *ParamDef) choiceIndex(s string) (int, bool) {
	s = strings.TrimSpace(s)
	for i, c := range p.Choices {
		if strings.EqualFold(c, s) {
			return i, true
		}
	}
	return -1, false
}

func roundHalfUp(x float64) float64 {
	if x < 0 {
		return -roundHalfUp(-x)
	}
	i := float64(int64(x))
	if x-i >= 0.5 {
		return i + 1
	}
	return i
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
