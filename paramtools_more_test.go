// paramtools_more_test.go - coverage for the generic param surface in paramtools.go that the socket tests do
// not reach: resolveReal branches, shortReason mapping, formatFloat, liveArg, headless get/set handlers,
// set_params skip reporting + GCF decode error + empty input, setParamReal error branches (driven against the
// in-package fakeHost), and ParamInference.summary for each case. No real plugin.

package sidechain

import (
	"context"
	"strings"
	"testing"
)

func TestResolveRealBranches(t *testing.T) {
	c := testCatalog()
	cutoff := c.Get("cutoff") // float 20..20000
	ft := c.Get("filterType") // choice
	v := 800.0
	n := 0.5

	// zero inputs -> "exactly one".
	if _, msg := resolveReal(cutoff, nil, nil, ""); !strings.Contains(msg, "exactly one") {
		t.Errorf("zero inputs msg = %q, want exactly-one", msg)
	}
	// two inputs -> "exactly one".
	if _, msg := resolveReal(cutoff, &v, &n, ""); !strings.Contains(msg, "exactly one") {
		t.Errorf("two inputs msg = %q, want exactly-one", msg)
	}
	// choice on a non-choice param.
	if _, msg := resolveReal(cutoff, nil, nil, "Ladder"); !strings.Contains(msg, "not a choice param") {
		t.Errorf("choice-on-float msg = %q, want not-a-choice-param", msg)
	}
	// bad choice name on a real choice param.
	if _, msg := resolveReal(ft, nil, nil, "Nope"); !strings.Contains(msg, "not a valid choice") {
		t.Errorf("bad choice msg = %q, want not-a-valid-choice", msg)
	}
	// good choice -> index.
	if real, msg := resolveReal(ft, nil, nil, "Ladder"); msg != "" || real != 2 {
		t.Errorf("choice Ladder = %v,%q want 2,''", real, msg)
	}
	// value path clamps.
	if real, msg := resolveReal(cutoff, ptrF(999999), nil, ""); msg != "" || real != 20000 {
		t.Errorf("value clamp = %v,%q want 20000,''", real, msg)
	}
	// normalized path maps to real.
	if real, msg := resolveReal(cutoff, nil, ptrF(0.5), ""); msg != "" || real != 20+0.5*(20000-20) {
		t.Errorf("normalized map = %v,%q", real, msg)
	}
}

func TestShortReason(t *testing.T) {
	cases := map[string]string{
		"provide exactly one of value / normalized / choice.": "no-value",
		`"x" is not a choice param (type float); use value.`:  "not-choice",
		`"y" is not a valid choice for z. Choices: a, b`:      "bad-choice",
		"something else entirely":                             "invalid",
	}
	for msg, want := range cases {
		if got := shortReason(msg); got != want {
			t.Errorf("shortReason(%q) = %q, want %q", msg, got, want)
		}
	}
}

func TestFormatFloat(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0"},
		{1000, "1000"},
		{-42, "-42"},
		{0.5, "0.5"},
		{1234.5, "1234.5"},
	}
	for _, c := range cases {
		if got := formatFloat(c.in); got != c.want {
			t.Errorf("formatFloat(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestLiveArg(t *testing.T) {
	// HasRealRange + value input -> forward the real value (isReal true).
	ranged := &ParamDef{ID: "cut", Type: "float", Min: 20, Max: 20000, HasRealRange: true}
	if v, isReal := liveArg(ranged, 500, true); !isReal || v != 500 {
		t.Errorf("liveArg(ranged, real value) = %v,%v want 500,true", v, isReal)
	}
	// HasRealRange but NOT from a value (e.g. normalized/choice) -> normalized.
	if v, isReal := liveArg(ranged, 20000, false); isReal || v != 1 {
		t.Errorf("liveArg(ranged, non-value) = %v,%v want 1,false", v, isReal)
	}
	// no real range -> always normalized.
	hosted := &ParamDef{ID: "mix", Type: "float", Min: 0, Max: 1}
	if v, isReal := liveArg(hosted, 0.8, true); isReal || v != 0.8 {
		t.Errorf("liveArg(hosted, value) = %v,%v want 0.8,false", v, isReal)
	}
}

func TestHeadlessGetParam(t *testing.T) {
	s := newSession(testCatalog())
	ctx := context.Background()

	// unset -> reports the default and "(default, unset)".
	res, out, err := s.handleGetParam(ctx, nil, getParamIn{ID: "cutoff"})
	if err != nil {
		t.Fatalf("get_param: %v", err)
	}
	txt := textOf(res)
	if !strings.Contains(txt, "1000") || !strings.Contains(txt, "default, unset") {
		t.Fatalf("unset get_param = %q, want default 1000 + unset marker", txt)
	}
	g := out.(struct {
		Param      ParamDef `json:"param"`
		Value      float64  `json:"value"`
		Normalized float64  `json:"normalized"`
		IsSet      bool     `json:"isSet"`
	})
	if g.IsSet || g.Value != 1000 {
		t.Fatalf("unset structured = %+v, want value 1000 isSet false", g)
	}

	// a choice param renders the extra "(Name)".
	s.params["filterType"] = 2 // Ladder
	cres, _, err := s.handleGetParam(ctx, nil, getParamIn{ID: "filterType"})
	if err != nil {
		t.Fatalf("get_param choice: %v", err)
	}
	if !strings.Contains(textOf(cres), "(Ladder)") {
		t.Fatalf("choice get_param = %q, want (Ladder)", textOf(cres))
	}

	// unknown id.
	ures, _, _ := s.handleGetParam(ctx, nil, getParamIn{ID: "ghost"})
	if !strings.Contains(textOf(ures), "unknown id") {
		t.Fatalf("unknown get_param = %q, want unknown-id", textOf(ures))
	}
}

func TestHeadlessSetParam(t *testing.T) {
	s := newSession(testCatalog())
	ctx := context.Background()

	// unknown id.
	ures, _, _ := s.handleSetParam(ctx, nil, setParamIn{ID: "ghost", Value: ptrF(1)})
	if !strings.Contains(textOf(ures), "unknown id") {
		t.Fatalf("unknown set_param = %q, want unknown-id", textOf(ures))
	}

	// value set clamps and persists.
	res, _, err := s.handleSetParam(ctx, nil, setParamIn{ID: "cutoff", Value: ptrF(999999)})
	if err != nil {
		t.Fatalf("set_param: %v", err)
	}
	if !strings.Contains(textOf(res), "20000") || s.params["cutoff"] != 20000 {
		t.Fatalf("set_param clamp = %q, stored %v", textOf(res), s.params["cutoff"])
	}

	// choice set renders the (Name) and stores the index.
	cres, _, err := s.handleSetParam(ctx, nil, setParamIn{ID: "filterType", Choice: "Bandpass"})
	if err != nil {
		t.Fatalf("set_param choice: %v", err)
	}
	if !strings.Contains(textOf(cres), "(Bandpass)") || s.params["filterType"] != 1 {
		t.Fatalf("set_param choice = %q, stored %v", textOf(cres), s.params["filterType"])
	}

	// a validation error surfaces (two inputs).
	bres, _, _ := s.handleSetParam(ctx, nil, setParamIn{ID: "cutoff", Value: ptrF(1), Normalized: ptrF(0.5)})
	if !strings.Contains(textOf(bres), "exactly one") {
		t.Fatalf("set_param two-inputs = %q, want exactly-one", textOf(bres))
	}
}

func TestHeadlessSetParamsEmptyAndBadGCF(t *testing.T) {
	s := newSession(testCatalog())
	ctx := context.Background()

	// empty input.
	eres, _, _ := s.handleSetParams(ctx, nil, setParamsIn{})
	if !strings.Contains(textOf(eres), "provide params") {
		t.Fatalf("empty set_params = %q, want provide-params", textOf(eres))
	}

	// GCF that does not decode.
	bres, _, _ := s.handleSetParams(ctx, nil, setParamsIn{GCF: "GCF profile=generic\n@@ totally broken @@"})
	if !strings.Contains(textOf(bres), "did not decode") && !strings.Contains(textOf(bres), "not an array") {
		t.Fatalf("bad GCF = %q, want a decode error", textOf(bres))
	}
}

func TestHeadlessSetParamsSkipReporting(t *testing.T) {
	s := newSession(testCatalog())
	ctx := context.Background()
	res, _, err := s.handleSetParams(ctx, nil, setParamsIn{Params: []setParamsRow{
		{ID: "cutoff", Value: ptrF(800)},   // ok
		{ID: "ghost", Value: ptrF(1)},      // unknown
		{ID: "cutoff", Choice: "Ladder"},   // not-choice
		{ID: "filterType", Choice: "Nope"}, // bad-choice
		{ID: "drive"},                      // no value/normalized/choice -> no-value
	}})
	if err != nil {
		t.Fatalf("set_params: %v", err)
	}
	txt := textOf(res)
	if !strings.Contains(txt, "applied 1") || !strings.Contains(txt, "skipped 4") {
		t.Fatalf("report = %q, want applied 1 / skipped 4", txt)
	}
	for _, tag := range []string{"ghost(unknown)", "not-choice", "bad-choice", "no-value"} {
		if !strings.Contains(txt, tag) {
			t.Errorf("skip report %q missing tag %q", txt, tag)
		}
	}
}

// setParamReal error branches, driven against the in-package fakeHost (not a real plugin).

func TestSetParamRealNotLive(t *testing.T) {
	s := newSession(testCatalog())
	ctx := context.Background()
	// Not connected: a real-unit set is rejected with a "needs a live plugin" hint.
	res, _, _ := s.handleSetParam(ctx, nil, setParamIn{ID: "cutoff", Real: ptrF(1000)})
	if !strings.Contains(textOf(res), "needs a live plugin") {
		t.Fatalf("real set offline = %q, want needs-live", textOf(res))
	}
}

func TestSetParamRealMutualExclusion(t *testing.T) {
	fh := startFakeHost(t)
	defer fh.stop()
	s := newLiveTestSession()
	ctx := context.Background()
	if _, _, err := s.handleConnectLive(ctx, nil, connectLiveIn{Host: "127.0.0.1", Port: fh.port()}); err != nil {
		t.Fatalf("connect_live: %v", err)
	}
	// real + choice together is rejected before any probe.
	res, _, _ := s.handleSetParam(ctx, nil, setParamIn{ID: "cutoff", Real: ptrF(1000), Choice: "x"})
	if !strings.Contains(textOf(res), "mutually exclusive") {
		t.Fatalf("real+choice = %q, want mutually-exclusive", textOf(res))
	}
}

func TestSetParamRealDiscrete(t *testing.T) {
	fh := startFakeHost(t)
	defer fh.stop()
	// A param the fake renders as discrete labels (Off/On): infer -> !Numeric -> "is discrete".
	s := newSession(NewCatalog([]ParamDef{{ID: "toggle", Label: "Toggle", Type: "float", Min: 0, Max: 1}}))
	ctx := context.Background()
	if _, _, err := s.handleConnectLive(ctx, nil, connectLiveIn{Host: "127.0.0.1", Port: fh.port()}); err != nil {
		t.Fatalf("connect_live: %v", err)
	}
	res, _, _ := s.handleSetParam(ctx, nil, setParamIn{ID: "toggle", Real: ptrF(1)})
	if !strings.Contains(textOf(res), "discrete") {
		t.Fatalf("discrete real set = %q, want discrete", textOf(res))
	}
}

func TestSetParamRealUnitless(t *testing.T) {
	fh := startFakeHost(t)
	defer fh.stop()
	// A param the fake renders as a bare number (no unit): infer numeric but Unit=="" -> "no real unit".
	s := newSession(NewCatalog([]ParamDef{{ID: "raw", Label: "Raw", Type: "float", Min: 0, Max: 1}}))
	ctx := context.Background()
	if _, _, err := s.handleConnectLive(ctx, nil, connectLiveIn{Host: "127.0.0.1", Port: fh.port()}); err != nil {
		t.Fatalf("connect_live: %v", err)
	}
	res, _, _ := s.handleSetParam(ctx, nil, setParamIn{ID: "raw", Real: ptrF(0.5)})
	if !strings.Contains(textOf(res), "no real unit") {
		t.Fatalf("unitless real set = %q, want no-real-unit", textOf(res))
	}
}

func TestSummaryCases(t *testing.T) {
	// discrete with labels.
	disc := ParamInference{Numeric: false, Labels: []string{"Off", "On"}}
	if got := disc.summary("t", "Toggle"); !strings.Contains(got, "discrete") || !strings.Contains(got, "Off, On") {
		t.Errorf("discrete summary = %q", got)
	}
	// non-numeric, no labels.
	none := ParamInference{Numeric: false}
	if got := none.summary("t", "T"); !strings.Contains(got, "no readable value text") {
		t.Errorf("no-label summary = %q", got)
	}
	// numeric but unitless.
	unitless := ParamInference{Numeric: true, Unit: ""}
	if got := unitless.summary("t", "T"); !strings.Contains(got, "unitless") {
		t.Errorf("unitless summary = %q", got)
	}
	// numeric with unit + reliable fit renders the fit and error.
	fit := ParamInference{
		Numeric: true, Unit: "hz", RealMin: 20, RealMax: 20000, Curve: "exponential",
		Fit: &CurveFit{Model: "exp", A: 20, B: 6.9, MaxRelErr: 0.001},
	}
	if got := fit.summary("cut", "Cutoff"); !strings.Contains(got, "hz") || !strings.Contains(got, "exp fit") {
		t.Errorf("fit summary = %q, want hz + exp fit", got)
	}
	// numeric with unit but no reliable fit -> "sampled" + bipolar marker.
	sampled := ParamInference{Numeric: true, Unit: "%", RealMin: -100, RealMax: 100, Bipolar: true, Curve: "linear"}
	got := sampled.summary("pan", "Pan")
	if !strings.Contains(got, "bipolar") {
		t.Errorf("bipolar summary = %q, want bipolar marker", got)
	}
}

func ptrF(v float64) *float64 { return &v }
