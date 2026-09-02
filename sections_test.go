// sections_test.go - label-prefix sectioning (sections.go) as a navigation fallback for ungrouped plugins:
// a flat TAL-like catalog derives Filter/Amp/Osc sections; a catalog with real groups is left untouched; and
// the K=sectionMinShare threshold and no-shared-prefix edge cases are asserted. Pure Go, no plugin, no socket.

package sidechain

import (
	"sort"
	"testing"
)

// TestHostSectionPreferred: when the catalog carries host-emitted Section fields, the effective-group view uses
// them directly (the host is the single source of truth) instead of re-deriving from labels. Here the labels would
// derive to a single "Xyz" section, but the host's Section says Filter/Amp - and the host wins.
func TestHostSectionPreferred(t *testing.T) {
	c := NewCatalog([]ParamDef{
		{ID: "a", Label: "Xyz One", Section: "Filter", Type: "float"},
		{ID: "b", Label: "Xyz Two", Section: "Filter", Type: "float"},
		{ID: "c", Label: "Xyz Three", Section: "Amp", Type: "float"},
	})
	if g := c.Groups(); len(g) != 2 || g[0] != "Amp" || g[1] != "Filter" {
		t.Fatalf("Groups should come from the host Section, got %v", g)
	}
	if m := c.Filter("Filter", ""); len(m) != 2 {
		t.Fatalf("Filter by host Section: got %d want 2", len(m))
	}
	if m := c.Filter("amp", ""); len(m) != 1 || m[0].ID != "c" {
		t.Fatalf("Filter amp by host Section: got %v", m)
	}
}

// flatCatalog is a TAL-NoiseMaker-like catalog: NO groups reported, structure lives only in the labels.
func flatCatalog() *Catalog {
	return NewCatalog([]ParamDef{
		{ID: "masterVol", Label: "Master Volume", Type: "float"},
		{ID: "filterType", Label: "Filter Type", Type: "float"},
		{ID: "filterCutoff", Label: "Filter Cutoff", Type: "float"},
		{ID: "filterRes", Label: "Filter Resonance", Type: "float"},
		{ID: "filterAtt", Label: "Filter Attack", Type: "float"},
		{ID: "ampAtt", Label: "Amp Attack", Type: "float"},
		{ID: "ampDec", Label: "Amp Decay", Type: "float"},
		{ID: "ampSus", Label: "Amp Sustain", Type: "float"},
		{ID: "osc1Tune", Label: "Osc 1 Tune", Type: "float"},
		{ID: "osc2Tune", Label: "Osc 2 Tune", Type: "float"},
		{ID: "lfo1Rate", Label: "LFO 1 Rate", Type: "float"},
		{ID: "portamento", Label: "Portamento", Type: "float"}, // singleton
		{ID: "voices", Label: "Voices", Type: "float"},         // singleton
	})
}

func TestDerivedSectionsFlatCatalog(t *testing.T) {
	c := flatCatalog()

	// Filter (4) and Amp (3) qualify at K=3. Osc has only 2 params ("Osc 1 Tune"/"Osc 2 Tune"), which is below
	// K, so no Osc section forms and both fall to "other". LFO has 1. Assert what the K threshold yields.
	groups := c.Groups()
	want := []string{"Amp", "Filter", "other"}
	if len(groups) != len(want) {
		t.Fatalf("groups = %v, want %v", groups, want)
	}
	for i := range want {
		if groups[i] != want[i] {
			t.Fatalf("groups = %v, want %v", groups, want)
		}
	}

	// Filter section returns exactly the four Filter-prefixed params, sorted by id.
	fm := c.Filter("Filter", "")
	if len(fm) != 4 {
		t.Fatalf("Filter section = %d params, want 4: %v", len(fm), fm)
	}
	wantIDs := []string{"filterAtt", "filterCutoff", "filterRes", "filterType"}
	for i, id := range wantIDs {
		if fm[i].ID != id {
			t.Fatalf("Filter[%d].ID = %q, want %q (full %v)", i, fm[i].ID, id, fm)
		}
	}

	// case-insensitive group match.
	if lower := c.Filter("filter", ""); len(lower) != 4 {
		t.Fatalf("case-insensitive Filter = %d, want 4", len(lower))
	}

	// Amp section: 3 params.
	if am := c.Filter("Amp", ""); len(am) != 3 {
		t.Fatalf("Amp section = %d params, want 3: %v", len(am), am)
	}

	// The two singletons and the two Osc/LFO params fall into "other".
	other := c.Filter("other", "")
	otherIDs := map[string]bool{}
	for _, p := range other {
		otherIDs[p.ID] = true
	}
	for _, id := range []string{"masterVol", "portamento", "voices", "osc1Tune", "osc2Tune", "lfo1Rate"} {
		if !otherIDs[id] {
			t.Fatalf("expected %q in 'other', got %v", id, other)
		}
	}

	// The wire shape is untouched: derivation never fabricates a real Group.
	if g := c.Get("filterCutoff").Group; g != "other" {
		t.Fatalf("ParamDef.Group mutated to %q, want 'other' (derivation is a view only)", g)
	}
}

// A catalog that DOES qualify Osc at K=3: three oscillators. Because the numeric index is dropped, all three
// labels tokenize to [Osc, Tune], so the LONGEST qualifying prefix is the pair "Osc Tune" (shared by all 3),
// which is more precise than the single leading token. Assert that.
func TestDerivedSectionsThreeOsc(t *testing.T) {
	c := NewCatalog([]ParamDef{
		{ID: "osc1", Label: "Osc 1 Tune", Type: "float"},
		{ID: "osc2", Label: "Osc 2 Tune", Type: "float"},
		{ID: "osc3", Label: "Osc 3 Tune", Type: "float"},
		{ID: "solo", Label: "Master Gain", Type: "float"},
	})
	groups := c.Groups()
	want := []string{"Osc Tune", "other"}
	if len(groups) != len(want) || groups[0] != want[0] || groups[1] != want[1] {
		t.Fatalf("groups = %v, want %v", groups, want)
	}
	if m := c.Filter("Osc Tune", ""); len(m) != 3 {
		t.Fatalf("Osc Tune section = %d, want 3", len(m))
	}
}

// TestDerivationNotAppliedWithRealGroups: a catalog with real groups keeps them; derivation is skipped and
// Groups()/Filter() behave exactly as the pre-existing TestFilterAndGroups expects.
func TestDerivationNotAppliedWithRealGroups(t *testing.T) {
	c := testCatalog() // cutoff/filterType in "filter", drive in "amp"
	if c.derived {
		t.Fatal("catalog with real groups should NOT use derived sections")
	}
	if g := c.Groups(); len(g) != 2 || g[0] != "amp" || g[1] != "filter" {
		t.Fatalf("groups = %v, want [amp filter]", g)
	}
	if m := c.Filter("filter", ""); len(m) != 2 {
		t.Fatalf("filter group=filter = %d, want 2", len(m))
	}
	if m := c.Filter("amp", ""); len(m) != 1 || m[0].ID != "drive" {
		t.Fatalf("filter group=amp = %v, want [drive]", m)
	}
}

// TestDerivationMixedGroups: even a SINGLE real group disables derivation (the plugin clearly reports groups),
// so params left in "other" stay in "other" rather than getting label-prefix sections.
func TestDerivationMixedGroups(t *testing.T) {
	c := NewCatalog([]ParamDef{
		{ID: "a", Label: "Filter Cutoff", Type: "float", Group: "tone"},
		{ID: "b", Label: "Filter Resonance", Type: "float"}, // no group -> "other"
		{ID: "c", Label: "Filter Drive", Type: "float"},     // no group -> "other"
	})
	if c.derived {
		t.Fatal("presence of a real group must disable derivation")
	}
	g := c.Groups()
	want := []string{"other", "tone"} // plain sort.Strings, no 'other'-last rule for real groups
	if len(g) != len(want) || g[0] != want[0] || g[1] != want[1] {
		t.Fatalf("groups = %v, want %v", g, want)
	}
	if m := c.Filter("other", ""); len(m) != 2 {
		t.Fatalf("other = %d, want 2 (b,c stay in other, no derived Filter)", len(m))
	}
}

// TestNoSharedPrefix: labels with no shared leading token all fall to "other".
func TestNoSharedPrefix(t *testing.T) {
	c := NewCatalog([]ParamDef{
		{ID: "a", Label: "Gain", Type: "float"},
		{ID: "b", Label: "Pan", Type: "float"},
		{ID: "c", Label: "Width", Type: "float"},
	})
	if g := c.Groups(); len(g) != 1 || g[0] != "other" {
		t.Fatalf("groups = %v, want [other]", g)
	}
	if m := c.Filter("other", ""); len(m) != 3 {
		t.Fatalf("other = %d, want 3", len(m))
	}
}

// TestKThreshold: exactly K-1 shared -> "other"; exactly K shared -> its own section.
func TestKThreshold(t *testing.T) {
	if sectionMinShare != 3 {
		t.Fatalf("this test assumes sectionMinShare==3, got %d", sectionMinShare)
	}
	// Two params share "Reverb" -> below K -> both "other".
	two := deriveSections([]ParamDef{
		{Label: "Reverb Size"},
		{Label: "Reverb Mix"},
	})
	for i, s := range two {
		if s != "other" {
			t.Fatalf("2 shared: section[%d] = %q, want other (below K=3)", i, s)
		}
	}
	// Three params share "Reverb" -> at K -> "Reverb".
	three := deriveSections([]ParamDef{
		{Label: "Reverb Size"},
		{Label: "Reverb Mix"},
		{Label: "Reverb Decay"},
	})
	for i, s := range three {
		if s != "Reverb" {
			t.Fatalf("3 shared: section[%d] = %q, want Reverb (at K=3)", i, s)
		}
	}
}

// TestLongestQualifyingPrefix: when both a short and a longer prefix qualify, the LONGER wins. Here 6 params all
// start "Env", and 3 of them share the longer "Env 1" (numeric token dropped -> tokens are Env,Attack etc., so
// build a case where the second token is non-numeric).
func TestLongestQualifyingPrefix(t *testing.T) {
	// "Mod Env" x3 and "Amp Env" x3: leading token differs, so no over-merge into a single "Env".
	c := NewCatalog([]ParamDef{
		{ID: "m1", Label: "Mod Env Attack", Type: "float"},
		{ID: "m2", Label: "Mod Env Decay", Type: "float"},
		{ID: "m3", Label: "Mod Env Sustain", Type: "float"},
		{ID: "a1", Label: "Amp Env Attack", Type: "float"},
		{ID: "a2", Label: "Amp Env Decay", Type: "float"},
		{ID: "a3", Label: "Amp Env Sustain", Type: "float"},
	})
	g := c.Groups()
	want := []string{"Amp Env", "Mod Env"}
	if len(g) != len(want) {
		t.Fatalf("groups = %v, want %v", g, want)
	}
	sort.Strings(want)
	for i := range want {
		if g[i] != want[i] {
			t.Fatalf("groups = %v, want %v", g, want)
		}
	}
	if m := c.Filter("Mod Env", ""); len(m) != 3 {
		t.Fatalf("Mod Env = %d, want 3", len(m))
	}
}

// TestLabelTokens: tokenization drops pure-number tokens and splits on non-alphanumeric separators.
func TestLabelTokens(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"Osc 1 Tune", []string{"Osc", "Tune"}},
		{"Filter-Cutoff", []string{"Filter", "Cutoff"}},
		{"LFO 1 Rate", []string{"LFO", "Rate"}},
		{"  ", nil},
		{"123", nil},
		{"Env2 Attack", []string{"Env2", "Attack"}}, // Env2 is not pure digits -> kept whole
	}
	for _, tc := range cases {
		got := labelTokens(tc.in)
		if len(got) != len(tc.want) {
			t.Fatalf("labelTokens(%q) = %v, want %v", tc.in, got, tc.want)
		}
		for i := range tc.want {
			if got[i] != tc.want[i] {
				t.Fatalf("labelTokens(%q) = %v, want %v", tc.in, got, tc.want)
			}
		}
	}
}
