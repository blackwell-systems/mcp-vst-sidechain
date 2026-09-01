// catalog_more_test.go - extra pure-math coverage for catalog.go: group defaulting, JSON error paths, stable
// group ordering, filter combinations, step quantization + clamp-after-snap, norm<->real round-trips (incl the
// Max==Min guard), roundHalfUp with negatives, choiceIndex case handling, and orDefault. No socket, no plugin.

package sidechain

import (
	"math"
	"testing"
)

func TestNewCatalogGroupDefaulting(t *testing.T) {
	c := NewCatalog([]ParamDef{
		{ID: "a", Group: ""},       // empty -> "other"
		{ID: "b", Group: "   "},    // whitespace -> "other"
		{ID: "c", Group: "filter"}, // kept
	})
	if g := c.Get("a").Group; g != "other" {
		t.Errorf("empty group = %q, want other", g)
	}
	if g := c.Get("b").Group; g != "other" {
		t.Errorf("whitespace group = %q, want other", g)
	}
	if g := c.Get("c").Group; g != "filter" {
		t.Errorf("set group = %q, want filter", g)
	}
}

func TestLoadCatalogJSONErrors(t *testing.T) {
	// Bad JSON => parse error.
	if _, err := loadCatalogJSON([]byte(`{not json`)); err == nil {
		t.Fatal("malformed json should error")
	}
	// Well-formed but empty params => explicit error.
	if _, err := loadCatalogJSON([]byte(`{"params":[]}`)); err == nil {
		t.Fatal("empty params should error")
	}
	// Missing stateRootTag => defaulted to PARAMS.
	c, err := loadCatalogJSON([]byte(`{"params":[{"id":"x","type":"float","min":0,"max":1}]}`))
	if err != nil {
		t.Fatalf("valid minimal catalog: %v", err)
	}
	if c.StateRootTag != "PARAMS" {
		t.Errorf("default stateRootTag = %q, want PARAMS", c.StateRootTag)
	}
}

func TestGroupsStableSorted(t *testing.T) {
	c := NewCatalog([]ParamDef{
		{ID: "a", Group: "zeta"},
		{ID: "b", Group: "alpha"},
		{ID: "c", Group: "zeta"}, // duplicate group collapses
		{ID: "d", Group: "mid"},
	})
	g := c.Groups()
	want := []string{"alpha", "mid", "zeta"}
	if len(g) != len(want) {
		t.Fatalf("groups = %v, want %v", g, want)
	}
	for i := range want {
		if g[i] != want[i] {
			t.Fatalf("groups = %v, want %v", g, want)
		}
	}
}

func TestFilterCombinations(t *testing.T) {
	c := testCatalog() // cutoff/filterType in "filter", drive in "amp"
	// group + substring together.
	if m := c.Filter("filter", "cut"); len(m) != 1 || m[0].ID != "cutoff" {
		t.Fatalf("filter group+substr = %v, want [cutoff]", m)
	}
	// substring matches on label too (case-insensitive).
	if m := c.Filter("", "FILTER TYPE"); len(m) != 1 || m[0].ID != "filterType" {
		t.Fatalf("label substr = %v, want [filterType]", m)
	}
	// empty filters return everything, sorted by id.
	all := c.Filter("", "")
	if len(all) != 3 || all[0].ID != "cutoff" || all[1].ID != "drive" || all[2].ID != "filterType" {
		t.Fatalf("unfiltered = %v, want cutoff/drive/filterType sorted", all)
	}
	// group with no matches.
	if m := c.Filter("nope", ""); len(m) != 0 {
		t.Fatalf("unknown group = %v, want empty", m)
	}
}

func TestClampRealStepQuantization(t *testing.T) {
	// A stepped float: min 0, max 10, step 2. Legal values 0,2,4,6,8,10.
	p := &ParamDef{ID: "step", Type: "float", Min: 0, Max: 10, Step: 2}
	if got := p.clampReal(3); got != 4 { // 3/2 = 1.5 -> roundHalfUp -> 2 -> 0+2*2 = 4
		t.Errorf("clampReal(3) = %v, want 4", got)
	}
	if got := p.clampReal(2.9); got != 2 { // 1.45 rounds down to 1 -> 2
		t.Errorf("clampReal(2.9) = %v, want 2", got)
	}
	// clamp-after-snap: a value that rounds above max is pulled back to max.
	q := &ParamDef{ID: "step2", Type: "float", Min: 0, Max: 9, Step: 2}
	if got := q.clampReal(9); got != 9 { // snap gives 0+5*2=10 > 9 -> clamped to 9
		t.Errorf("clampReal(9) = %v, want 9 (clamp after snap)", got)
	}
}

func TestNormRealRoundTrip(t *testing.T) {
	p := &ParamDef{ID: "lin", Type: "float", Min: 100, Max: 200}
	for _, n := range []float64{0, 0.25, 0.5, 0.75, 1} {
		real := p.normToReal(n)
		back := p.realToNorm(real)
		if !approx(back, n, 1e-9) {
			t.Errorf("round-trip norm %v -> real %v -> norm %v", n, real, back)
		}
	}
	// out-of-range norm clamps.
	if got := p.normToReal(-1); got != 100 {
		t.Errorf("normToReal(-1) = %v, want 100", got)
	}
	if got := p.normToReal(2); got != 200 {
		t.Errorf("normToReal(2) = %v, want 200", got)
	}
	// realToNorm clamps out-of-range reals.
	if got := p.realToNorm(50); got != 0 {
		t.Errorf("realToNorm(below min) = %v, want 0", got)
	}
	if got := p.realToNorm(500); got != 1 {
		t.Errorf("realToNorm(above max) = %v, want 1", got)
	}
}

func TestRealToNormMaxEqualsMin(t *testing.T) {
	p := &ParamDef{ID: "deg", Type: "float", Min: 5, Max: 5}
	if got := p.realToNorm(5); got != 0 {
		t.Errorf("realToNorm on Max==Min = %v, want 0 (guard)", got)
	}
}

func TestRoundHalfUp(t *testing.T) {
	cases := []struct{ in, want float64 }{
		{0, 0},
		{0.4, 0},
		{0.5, 1},
		{0.6, 1},
		{1.5, 2},
		{2.5, 3},
		{-0.5, -1}, // symmetric for negatives
		{-1.4, -1},
		{-1.5, -2},
	}
	for _, c := range cases {
		if got := roundHalfUp(c.in); got != c.want {
			t.Errorf("roundHalfUp(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestChoiceIndexCaseAndMiss(t *testing.T) {
	p := &ParamDef{ID: "c", Type: "choice", Choices: []string{"Low", "Mid", "High"}}
	if idx, ok := p.choiceIndex("MID"); !ok || idx != 1 {
		t.Errorf("choiceIndex(MID) = %d,%v want 1,true", idx, ok)
	}
	if idx, ok := p.choiceIndex("  high "); !ok || idx != 2 { // trimmed
		t.Errorf("choiceIndex(padded high) = %d,%v want 2,true", idx, ok)
	}
	if _, ok := p.choiceIndex("none"); ok {
		t.Error("miss should return ok=false")
	}
}

func TestOrDefault(t *testing.T) {
	if got := orDefault("", "def"); got != "def" {
		t.Errorf("orDefault(empty) = %q, want def", got)
	}
	if got := orDefault("   ", "def"); got != "def" {
		t.Errorf("orDefault(spaces) = %q, want def", got)
	}
	if got := orDefault("set", "def"); got != "set" {
		t.Errorf("orDefault(set) = %q, want set", got)
	}
}

func TestClampRealDescendingBounds(t *testing.T) {
	// Min > Max (degenerate ordering): clampReal applies the two clamps in sequence (raise to Min, then lower to
	// Max), so every input collapses to Max. Assert that CURRENT behavior rather than a "correct" one.
	p := &ParamDef{ID: "desc", Type: "float", Min: 100, Max: 0}
	if got := p.clampReal(50); got != 0 {
		t.Errorf("clampReal with Min>Max = %v, want 0 (sequential clamp collapses to Max)", got)
	}
	if math.IsNaN(p.normToReal(0.5)) {
		t.Fatalf("normToReal produced NaN for descending range")
	}
}
