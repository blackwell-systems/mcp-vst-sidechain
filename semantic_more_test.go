// semantic_more_test.go - pure-logic coverage for the semantic store internals the round-trip tests do not reach
// directly: mergeParam field-level rules (nil base + each field), mergeEntry with no disk entry, the
// persistableInference <-> ParamInference conversions (samples + discreteNorms), writeAtomic's failure path,
// defaultSemanticDir's env override, and the "store not enabled" guards on the three semantic tools. All headless.

package sidechain

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestMergeParamNilBase(t *testing.T) {
	upd := &ParamSemantics{Role: "filter.cutoff", Aliases: []string{"brightness"}}
	got := mergeParam(nil, upd)
	if got == upd {
		t.Fatal("mergeParam(nil, upd) must copy, not alias upd")
	}
	if got.Role != "filter.cutoff" || len(got.Aliases) != 1 {
		t.Fatalf("nil-base merge = %+v", got)
	}
}

func TestMergeParamFieldPrecedence(t *testing.T) {
	base := &ParamSemantics{
		Label: "L0", BehaviorClass: "float:log:hz", Role: "r0", Aliases: []string{"a0"},
		Polarity: "p0", Section: "s0", Confidence: 0.2, Notes: "n0",
	}
	// Every set field on upd overrides; a zero field leaves base intact.
	upd := &ParamSemantics{
		Label: "L1", Role: "r1", Aliases: []string{"a1", "a2"}, Polarity: "p1",
		Section: "s1", Confidence: 0.9, Notes: "n1",
	}
	got := mergeParam(base, upd)
	want := &ParamSemantics{
		Label: "L1", BehaviorClass: "float:log:hz", Role: "r1", Aliases: []string{"a1", "a2"},
		Polarity: "p1", Section: "s1", Confidence: 0.9, Notes: "n1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merge precedence:\n got  %+v\n want %+v", got, want)
	}
	// The base must not be mutated (merge returns a copy).
	if base.Role != "r0" || base.Label != "L0" {
		t.Fatalf("merge mutated base: %+v", base)
	}

	// An all-empty update preserves the base verbatim (including Inference and each field).
	inf := &persistableInference{Numeric: true, Unit: "db"}
	base2 := &ParamSemantics{Role: "keep", Inference: inf}
	keep := mergeParam(base2, &ParamSemantics{})
	if keep.Role != "keep" || keep.Inference != inf {
		t.Fatalf("empty update should preserve base fields, got %+v", keep)
	}
}

func TestMergeParamInferenceReplaced(t *testing.T) {
	base := &ParamSemantics{Inference: &persistableInference{Unit: "old"}}
	upd := &ParamSemantics{Inference: &persistableInference{Unit: "new"}}
	if got := mergeParam(base, upd); got.Inference == nil || got.Inference.Unit != "new" {
		t.Fatalf("a non-nil update inference should replace, got %+v", got.Inference)
	}
}

func TestMergeEntryNoDisk(t *testing.T) {
	ours := &SemanticEntry{Fingerprint: "fp", Params: map[string]*ParamSemantics{"a": {Role: "x"}}}
	if got := mergeEntry(nil, ours); got != ours {
		t.Fatal("mergeEntry(nil, ours) should return ours unchanged")
	}
}

func TestPersistableRoundTrip(t *testing.T) {
	pi := ParamInference{
		Numeric: true, Unit: "hz", RealMin: 20, RealMax: 20000, Bipolar: false, Curve: "logarithmic",
		Labels:        []string{"LP", "HP"},
		Fit:           &CurveFit{Model: "exp", A: 20, B: 6.9, MaxRelErr: 0.001},
		table:         []realSample{{0, 20}, {0.5, 632}, {1, 20000}},
		discreteNorms: map[string]float64{"LP": 0.1, "HP": 0.9},
	}
	p := toPersistable(pi)
	if len(p.Samples) != 3 || p.Samples[1] != [2]float64{0.5, 632} {
		t.Fatalf("samples not carried into persistable: %+v", p.Samples)
	}
	if !reflect.DeepEqual(p.DiscreteNorms, pi.discreteNorms) {
		t.Fatalf("discreteNorms lost: %+v", p.DiscreteNorms)
	}

	back := p.toInference()
	if len(back.table) != 3 || back.table[2] != (realSample{1, 20000}) {
		t.Fatalf("table not reconstructed: %+v", back.table)
	}
	if back.Unit != "hz" || back.Curve != "logarithmic" || back.Fit == nil || back.Fit.Model != "exp" {
		t.Fatalf("scalar fields lost on round-trip: %+v", back)
	}
	if !reflect.DeepEqual(back.discreteNorms, pi.discreteNorms) {
		t.Fatalf("discreteNorms lost on round-trip: %+v", back.discreteNorms)
	}
}

func TestWriteAtomicBadDir(t *testing.T) {
	// The parent directory does not exist, so CreateTemp fails and the error is surfaced (no panic, no partial file).
	missing := filepath.Join(t.TempDir(), "does-not-exist", "f.json")
	if err := writeAtomic(missing, []byte("x")); err == nil {
		t.Fatal("writeAtomic into a missing directory should error")
	}
}

func TestDefaultSemanticDirEnvOverride(t *testing.T) {
	t.Setenv("SIDECHAIN_SEMANTIC_DIR", "  /custom/sem/dir  ")
	if got := defaultSemanticDir(); got != "/custom/sem/dir" {
		t.Fatalf("env override = %q, want trimmed /custom/sem/dir", got)
	}
	// Empty override falls back to a non-empty default (cache or temp).
	t.Setenv("SIDECHAIN_SEMANTIC_DIR", "   ")
	if got := defaultSemanticDir(); got == "" {
		t.Fatal("empty override should fall back to a non-empty default dir")
	}
}

func TestSemanticToolsStoreNotEnabled(t *testing.T) {
	s := newSession(testCatalog()) // no attachStore -> s.store / s.entry are nil
	ctx := context.Background()

	ares, _, _ := s.handleAnnotateParams(ctx, nil, annotateParamsIn{Params: []annotateRow{{ID: "cutoff", Role: "r"}}})
	gres, _, _ := s.handleGetSemanticMap(ctx, nil, emptyIn{})
	fres, _, _ := s.handleForgetSemantics(ctx, nil, emptyIn{})

	for name, txt := range map[string]string{
		"annotate_params":  textOf(ares),
		"get_semantic_map": textOf(gres),
		"forget_semantics": textOf(fres),
	} {
		if !strings.Contains(txt, "not enabled") {
			t.Errorf("%s without a store = %q, want not-enabled", name, txt)
		}
	}
}

func TestAnnotatePersistFailure(t *testing.T) {
	// Attach a store to a clean dir (so Load succeeds and the entry is created), then replace the dir with a FILE
	// so the subsequent Save's MkdirAll fails: the annotation applies in memory but the persist error is reported.
	dir := t.TempDir()
	storeDir := filepath.Join(dir, "store")
	cat := mkCat(PluginIdentity{Name: "P", Format: "VST3"}, ParamDef{ID: "cutoff", Label: "Cutoff"})
	s := newSession(cat)
	if err := s.attachStore(NewSemanticStore(storeDir)); err != nil {
		t.Fatal(err)
	}
	// Now make storeDir a regular file: MkdirAll(storeDir) will fail inside Save.
	if err := os.WriteFile(storeDir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, _, _ := s.handleAnnotateParams(context.Background(), nil, annotateParamsIn{Params: []annotateRow{{ID: "cutoff", Role: "r"}}})
	if !strings.Contains(textOf(res), "persist failed") {
		t.Fatalf("annotate over a bad store dir = %q, want a persist-failed report", textOf(res))
	}
}

func TestLoadCorruptEntry(t *testing.T) {
	// A corrupt on-disk entry surfaces a parse error rather than a nil entry.
	dir := t.TempDir()
	st := NewSemanticStore(dir)
	fp := "sha256:deadbeef"
	if err := os.WriteFile(st.path(fp), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Load(fp); err == nil || !strings.Contains(err.Error(), "parse semantic store") {
		t.Fatalf("Load of a corrupt entry = %v, want a parse error", err)
	}
}
