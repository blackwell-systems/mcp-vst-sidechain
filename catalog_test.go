// catalog_test.go - the catalog math (clamp/normalize/choice), JSON loading, filtering, and the batch
// set_params path (JSON and GCF input) against a synthetic catalog. No socket, no plugin.

package sidechain

import (
	"context"
	"strings"
	"testing"
)

func testCatalog() *Catalog {
	return NewCatalog([]ParamDef{
		{ID: "cutoff", Label: "Cutoff", Type: "float", Min: 20, Max: 20000, Default: 1000, Group: "filter"},
		{ID: "filterType", Label: "Filter Type", Type: "choice", Min: 0, Max: 2, Step: 1, Default: 0, Group: "filter",
			Choices: []string{"Lowpass", "Bandpass", "Ladder"}},
		{ID: "drive", Label: "Drive", Type: "float", Min: 0, Max: 1, Default: 0, Group: "amp"},
	})
}

func TestLoadCatalogJSON(t *testing.T) {
	blob := []byte(`{"stateRootTag":"PARAMS","stateVersion":3,"count":1,"params":[
		{"id":"cutoff","label":"Cutoff","group":"filter","type":"float","min":20,"max":20000,"default":1000}]}`)
	c, err := loadCatalogJSON(blob)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(c.All()) != 1 || c.Get("cutoff") == nil {
		t.Fatalf("catalog missing cutoff: %+v", c.All())
	}
	if c.StateVersion != 3 || c.StateRootTag != "PARAMS" {
		t.Fatalf("provenance not parsed: %d %q", c.StateVersion, c.StateRootTag)
	}
	if _, err := loadCatalogJSON([]byte(`{"params":[]}`)); err == nil {
		t.Fatal("empty catalog should error")
	}
}

func TestClampAndChoice(t *testing.T) {
	c := testCatalog()
	cut := c.Get("cutoff")
	if got := cut.clampReal(999999); got != 20000 {
		t.Fatalf("clamp high = %v, want 20000", got)
	}
	if got := cut.clampReal(-5); got != 20 {
		t.Fatalf("clamp low = %v, want 20", got)
	}
	ft := c.Get("filterType")
	if idx, ok := ft.choiceIndex("ladder"); !ok || idx != 2 {
		t.Fatalf("choiceIndex(ladder) = %d,%v want 2,true", idx, ok)
	}
	if _, ok := ft.choiceIndex("nope"); ok {
		t.Fatal("bogus choice should not resolve")
	}
}

func TestFilterAndGroups(t *testing.T) {
	c := testCatalog()
	if g := c.Groups(); len(g) != 2 || g[0] != "amp" || g[1] != "filter" {
		t.Fatalf("groups = %v, want [amp filter]", g)
	}
	if m := c.Filter("filter", ""); len(m) != 2 {
		t.Fatalf("filter group=filter = %d params, want 2", len(m))
	}
	if m := c.Filter("", "drive"); len(m) != 1 || m[0].ID != "drive" {
		t.Fatalf("filter substr=drive = %v, want [drive]", m)
	}
}

func TestHeadlessSetParams_JSON(t *testing.T) {
	s := newSession(testCatalog())
	ctx := context.Background()
	cutoff := 800.0
	res, _, err := s.handleSetParams(ctx, nil, setParamsIn{Params: []setParamsRow{
		{ID: "cutoff", Value: &cutoff},
		{ID: "filterType", Choice: "Ladder"},
		{ID: "bogus", Value: &cutoff},    // unknown -> skipped
		{ID: "cutoff", Choice: "Ladder"}, // not a choice param -> skipped
	}})
	if err != nil {
		t.Fatalf("set_params: %v", err)
	}
	txt := textOf(res)
	if !strings.Contains(txt, "applied 2") || !strings.Contains(txt, "skipped 2") {
		t.Fatalf("set_params report = %q, want applied 2 / skipped 2", txt)
	}
	if s.params["cutoff"] != 800 {
		t.Fatalf("cutoff = %v, want 800", s.params["cutoff"])
	}
	if s.params["filterType"] != 2 {
		t.Fatalf("filterType = %v, want 2 (Ladder)", s.params["filterType"])
	}
}

func TestHeadlessSetParams_GCF(t *testing.T) {
	s := newSession(testCatalog())
	ctx := context.Background()
	// A bare GCF generic table (no header - the handler prepends it). Columns: id, value, choice; ~ = omit.
	gcfTable := "## [2]{id,value,choice}\ncutoff|1200|~\nfilterType|~|Bandpass\n"
	res, _, err := s.handleSetParams(ctx, nil, setParamsIn{GCF: gcfTable})
	if err != nil {
		t.Fatalf("set_params gcf: %v", err)
	}
	if txt := textOf(res); !strings.Contains(txt, "applied 2") {
		t.Fatalf("gcf set_params report = %q, want applied 2", txt)
	}
	if s.params["cutoff"] != 1200 {
		t.Fatalf("cutoff = %v, want 1200", s.params["cutoff"])
	}
	if s.params["filterType"] != 1 {
		t.Fatalf("filterType = %v, want 1 (Bandpass)", s.params["filterType"])
	}
}
