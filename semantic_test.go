// semantic_test.go - the Phase 3 semantic store tests (docs/PHASE3-SCOPING.md test plan). All headless / fake
// host: fingerprint equivalence, store round-trip + atomic write + merge, behavior-class derivation, describe
// recalling from the store WITHOUT re-probing, headless annotate then reload, and non-destructive invalidation.

package sidechain

import (
	"context"
	"os"
	"strings"
	"testing"
)

func mkCat(id PluginIdentity, params ...ParamDef) *Catalog {
	c := NewCatalog(append([]ParamDef(nil), params...))
	c.Plugin = id
	return c
}

func TestFingerprintEquivalence(t *testing.T) {
	id := PluginIdentity{Name: "P", Manufacturer: "M", Format: "VST3", Version: "1.0"}
	a := ParamDef{ID: "a", Label: "A"}
	b := ParamDef{ID: "b", Label: "B"}
	c1 := mkCat(id, a, b)

	// Version bump alone -> same key (surface unchanged).
	id2 := id
	id2.Version = "2.0"
	if fingerprintCatalog(c1) != fingerprintCatalog(mkCat(id2, a, b)) {
		t.Fatal("a version bump alone should not change the fingerprint")
	}
	// Param order should not matter (ids are sorted).
	if fingerprintCatalog(c1) != fingerprintCatalog(mkCat(id, b, a)) {
		t.Fatal("param order should not affect the fingerprint")
	}
	// Added param -> different key.
	if fingerprintCatalog(c1) == fingerprintCatalog(mkCat(id, a, b, ParamDef{ID: "c", Label: "C"})) {
		t.Fatal("adding a param should change the fingerprint")
	}
	// Different plugin identity -> different key.
	if fingerprintCatalog(c1) == fingerprintCatalog(mkCat(PluginIdentity{Name: "Q", Format: "VST3"}, a, b)) {
		t.Fatal("a different plugin name should change the fingerprint")
	}
}

func TestBehaviorClassDerivation(t *testing.T) {
	cases := []struct {
		pi   ParamInference
		want string
	}{
		{ParamInference{Numeric: true, Unit: "hz", Curve: "logarithmic"}, "float:log:hz"},
		{ParamInference{Numeric: true, Unit: "db", Curve: "linear", Bipolar: true}, "float:linear:db:bipolar"},
		{ParamInference{Numeric: true, Unit: "s", Curve: "exponential"}, "float:exp:s"},
		{ParamInference{Numeric: true, Unit: "", Curve: "linear"}, "float:linear:unitless"},
		{ParamInference{Numeric: false, Labels: []string{"LP", "BP", "HP"}}, "discrete:enum"},
		{ParamInference{Numeric: false}, "opaque"},
	}
	for _, c := range cases {
		if got := behaviorClass(c.pi); got != c.want {
			t.Errorf("behaviorClass(%+v) = %q, want %q", c.pi, got, c.want)
		}
	}
}

func TestStoreRoundTripAndMerge(t *testing.T) {
	dir := t.TempDir()
	st := NewSemanticStore(dir)
	fp := "sha256:deadbeef"
	e := &SemanticEntry{Fingerprint: fp, Plugin: PluginIdentity{Name: "P"}, Params: map[string]*ParamSemantics{
		"a": {Label: "A", Role: "filter.cutoff", Aliases: []string{"brightness"}},
	}}
	if _, err := st.Save(e); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Atomic write leaves no partial/temp file behind.
	entries, _ := os.ReadDir(dir)
	for _, f := range entries {
		if strings.HasPrefix(f.Name(), ".tmp-") {
			t.Fatalf("temp file left behind: %s", f.Name())
		}
	}

	got, err := st.Load(fp)
	if err != nil || got == nil || got.Params["a"].Role != "filter.cutoff" {
		t.Fatalf("round-trip: %+v (err %v)", got, err)
	}

	// Partial update preserves untouched fields (merge).
	upd := &SemanticEntry{Fingerprint: fp, Plugin: e.Plugin, Params: map[string]*ParamSemantics{
		"a": {Polarity: "higher = brighter"},
	}}
	merged, err := st.Save(upd)
	if err != nil {
		t.Fatalf("merge save: %v", err)
	}
	if merged.Params["a"].Role != "filter.cutoff" || merged.Params["a"].Polarity != "higher = brighter" || len(merged.Params["a"].Aliases) != 1 {
		t.Fatalf("merge did not preserve+apply fields: %+v", merged.Params["a"])
	}

	// A param only on disk survives when a different param is updated (union).
	m2, _ := st.Save(&SemanticEntry{Fingerprint: fp, Plugin: e.Plugin, Params: map[string]*ParamSemantics{"b": {Role: "amp.gain"}}})
	if m2.Params["a"] == nil || m2.Params["b"] == nil {
		t.Fatalf("union should keep both params: %+v", m2.Params)
	}
}

func TestAnnotateHeadlessThenReload(t *testing.T) {
	dir := t.TempDir()
	cat := mkCat(PluginIdentity{Name: "P", Manufacturer: "M", Format: "VST3"},
		ParamDef{ID: "cutoff", Label: "Cutoff", Type: "float", Min: 20, Max: 20000})
	ctx := context.Background()

	s := newSession(cat)
	if err := s.attachStore(NewSemanticStore(dir)); err != nil {
		t.Fatal(err)
	}
	// Annotate headless (no live endpoint), then it persists.
	if _, _, err := s.handleAnnotateParams(ctx, nil, annotateParamsIn{Params: []annotateRow{
		{ID: "cutoff", Role: "filter.cutoff", Aliases: []string{"brightness"}, Polarity: "higher = brighter"},
		{ID: "nope", Role: "x"}, // unknown id -> skipped
	}}); err != nil {
		t.Fatalf("annotate: %v", err)
	}

	// A fresh session on the SAME store recalls the annotation.
	s2 := newSession(cat)
	if err := s2.attachStore(NewSemanticStore(dir)); err != nil {
		t.Fatal(err)
	}
	_, out, _ := s2.handleGetSemanticMap(ctx, nil, emptyIn{})
	m := out.(semanticMapOut)
	if len(m.Params) != 1 || m.Params[0].Role != "filter.cutoff" || m.Params[0].Polarity != "higher = brighter" {
		t.Fatalf("reloaded semantic map wrong: %+v", m.Params)
	}
}

func TestDescribeRecallsFromStoreWithoutReprobe(t *testing.T) {
	fh := startFakeHost(t)
	defer fh.stop()
	dir := t.TempDir()
	cat := mkCat(PluginIdentity{Name: "P", Format: "VST3"},
		ParamDef{ID: "expo", Label: "Cutoff", Type: "float", Min: 0, Max: 1})
	ctx := context.Background()

	// Session 1 probes (sweeps) and persists.
	s := newSession(cat)
	if err := s.attachStore(NewSemanticStore(dir)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.handleConnectLive(ctx, nil, connectLiveIn{Host: "127.0.0.1", Port: fh.port()}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.handleDescribeParam(ctx, nil, describeParamIn{ID: "expo"}); err != nil {
		t.Fatal(err)
	}
	fh.mu.Lock()
	firstSweep := fh.cmds["set_param"]
	fh.cmds["set_param"] = 0 // reset for session 2
	fh.mu.Unlock()
	if firstSweep == 0 {
		t.Fatal("the first describe should have swept the param")
	}

	// Session 2 on the SAME store recalls WITHOUT sweeping.
	s2 := newSession(cat)
	if err := s2.attachStore(NewSemanticStore(dir)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s2.handleConnectLive(ctx, nil, connectLiveIn{Host: "127.0.0.1", Port: fh.port()}); err != nil {
		t.Fatal(err)
	}
	_, dout, err := s2.handleDescribeParam(ctx, nil, describeParamIn{ID: "expo"})
	if err != nil {
		t.Fatal(err)
	}
	d := dout.(describeParamOut)
	if !d.Cached {
		t.Fatal("the second-session describe should be a store recall")
	}
	fh.mu.Lock()
	secondSweep := fh.cmds["set_param"]
	fh.mu.Unlock()
	if secondSweep != 0 {
		t.Fatalf("a store recall must not re-sweep, got %d set_params", secondSweep)
	}
	// The recalled inference is complete (the exp fit round-tripped through the store).
	if d.Inference.Fit == nil || d.Inference.Fit.Model != "exp" || d.Inference.Unit != "hz" {
		t.Fatalf("recalled inference lost data: %+v", d.Inference)
	}
	if d.BehaviorClass != "float:exp:hz" { // the fake's "expo" is real = 20*1000^norm (exponential growth)
		t.Fatalf("behavior class = %q", d.BehaviorClass)
	}
}

func TestNonDestructiveInvalidation(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	id := PluginIdentity{Name: "P", Format: "VST3"}
	cat1 := mkCat(id, ParamDef{ID: "a", Label: "A"})

	s1 := newSession(cat1)
	if err := s1.attachStore(NewSemanticStore(dir)); err != nil {
		t.Fatal(err)
	}
	s1.handleAnnotateParams(ctx, nil, annotateParamsIn{Params: []annotateRow{{ID: "a", Role: "role.a"}}})

	// Surface change (a new param) -> a different fingerprint -> a fresh, empty entry, persisted under a new file.
	cat2 := mkCat(id, ParamDef{ID: "a", Label: "A"}, ParamDef{ID: "b", Label: "B"})
	s2 := newSession(cat2)
	if err := s2.attachStore(NewSemanticStore(dir)); err != nil {
		t.Fatal(err)
	}
	if _, out2, _ := s2.handleGetSemanticMap(ctx, nil, emptyIn{}); len(out2.(semanticMapOut).Params) != 0 {
		t.Fatal("a changed surface should start a fresh entry")
	}
	s2.handleAnnotateParams(ctx, nil, annotateParamsIn{Params: []annotateRow{{ID: "b", Role: "role.b"}}})

	// The old entry is intact.
	s1b := newSession(cat1)
	if err := s1b.attachStore(NewSemanticStore(dir)); err != nil {
		t.Fatal(err)
	}
	_, out1, _ := s1b.handleGetSemanticMap(ctx, nil, emptyIn{})
	if r := out1.(semanticMapOut).Params; len(r) != 1 || r[0].Role != "role.a" {
		t.Fatalf("the old entry should survive the surface change: %+v", r)
	}

	// Two fingerprint files coexist.
	files, _ := os.ReadDir(dir)
	n := 0
	for _, f := range files {
		if strings.HasSuffix(f.Name(), ".json") {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("expected 2 fingerprint files, got %d", n)
	}
}

func TestForgetSemantics(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	cat := mkCat(PluginIdentity{Name: "P", Format: "VST3"}, ParamDef{ID: "a", Label: "A"})
	s := newSession(cat)
	if err := s.attachStore(NewSemanticStore(dir)); err != nil {
		t.Fatal(err)
	}
	s.handleAnnotateParams(ctx, nil, annotateParamsIn{Params: []annotateRow{{ID: "a", Role: "role.a"}}})
	if _, _, err := s.handleForgetSemantics(ctx, nil, emptyIn{}); err != nil {
		t.Fatal(err)
	}
	if _, out, _ := s.handleGetSemanticMap(ctx, nil, emptyIn{}); len(out.(semanticMapOut).Params) != 0 {
		t.Fatal("forget should clear the entry")
	}
}
