// infer_more_test.go - deeper coverage of infer.go internals: parseValueText edge cases, normalizeUnit
// families, inferParam with mixed/empty samples, classifyCurve shapes, NormForReal clamping + descending +
// piecewise fallback, bracket, CurveFit.invert edge cases, lsq degenerate, fitCurve model selection, and
// analyticReliable threshold behavior. Pure math; no plugin.

package sidechain

import (
	"fmt"
	"math"
	"testing"
)

func TestParseValueTextMore(t *testing.T) {
	cases := []struct {
		in      string
		val     float64
		unit    string
		numeric bool
	}{
		{"2.5 kHz", 2.5, "khz", true},            // unit family folded later by normalizeUnit
		{"-3.5 dB", -3.5, "db", true},            // negative
		{"+12 semitones", 12, "semitones", true}, // leading +
		{"48000", 48000, "", true},               // no unit
		{"20 Hz (low)", 20, "hz", true},          // trailing annotation stripped
		{"", 0, "", false},                       // empty
		{"On", 0, "", false},                     // non-numeric label
		{"n/a", 0, "", false},                    // leads with a letter
	}
	for _, c := range cases {
		v, u, num := parseValueText(c.in)
		if num != c.numeric || u != c.unit || (num && !approx(v, c.val, 1e-9)) {
			t.Errorf("parseValueText(%q) = (%v,%q,%v), want (%v,%q,%v)", c.in, v, u, num, c.val, c.unit, c.numeric)
		}
	}
}

func TestNormalizeUnitFamilies(t *testing.T) {
	if v, u := normalizeUnit(353.6, "ms"); !approx(v, 0.3536, 1e-9) || u != "s" {
		t.Errorf("ms->s = %v,%q want 0.3536,s", v, u)
	}
	if v, u := normalizeUnit(2.5, "khz"); !approx(v, 2500, 1e-9) || u != "hz" {
		t.Errorf("khz->hz = %v,%q want 2500,hz", v, u)
	}
	// passthrough: unknown/base units are unchanged.
	if v, u := normalizeUnit(42, "db"); v != 42 || u != "db" {
		t.Errorf("db passthrough = %v,%q want 42,db", v, u)
	}
	if v, u := normalizeUnit(1, ""); v != 1 || u != "" {
		t.Errorf("empty passthrough = %v,%q want 1,''", v, u)
	}
}

func TestInferParamMixedAndEmpty(t *testing.T) {
	// A mix of numeric and label samples => any label wins => discrete (Numeric false).
	mixed := inferParam([]ValueSample{{0, "10 Hz"}, {0.5, "Bypass"}, {1, "100 Hz"}})
	if mixed.Numeric {
		t.Errorf("mixed samples should infer discrete, got %+v", mixed)
	}
	if len(mixed.Labels) != 1 || mixed.Labels[0] != "Bypass" {
		t.Errorf("mixed labels = %v, want [Bypass]", mixed.Labels)
	}
	// No samples at all => non-numeric, no labels.
	empty := inferParam(nil)
	if empty.Numeric || len(empty.Labels) != 0 {
		t.Errorf("empty inference = %+v, want non-numeric no labels", empty)
	}
}

func TestClassifyCurve(t *testing.T) {
	// linear: midpoint halfway.
	lin := []realSample{{0, 0}, {0.5, 50}, {1, 100}}
	if got := classifyCurveT(lin); got != "linear" {
		t.Errorf("linear classify = %q", got)
	}
	// exponential: slow start (midpoint low).
	exp := []realSample{{0, 0}, {0.5, 10}, {1, 100}}
	if got := classifyCurveT(exp); got != "exponential" {
		t.Errorf("exp classify = %q", got)
	}
	// logarithmic: fast start (midpoint high).
	log := []realSample{{0, 0}, {0.5, 90}, {1, 100}}
	if got := classifyCurveT(log); got != "logarithmic" {
		t.Errorf("log classify = %q", got)
	}
	// flat: ends equal.
	flat := []realSample{{0, 5}, {0.5, 5}, {1, 5}}
	if got := classifyCurveT(flat); got != "flat" {
		t.Errorf("flat classify = %q", got)
	}
	// too few points => unknown.
	if got := classifyCurveT([]realSample{{0, 0}, {1, 1}}); got != "unknown" {
		t.Errorf("2-point classify = %q, want unknown", got)
	}
}

// classifyCurveT is a thin adapter so the test can pass realSample tables to the unexported classifier.
func classifyCurveT(t []realSample) string { return classifyCurve(t) }

func TestNormForRealClampAndDescending(t *testing.T) {
	// Ascending linear 0..100 over norm 0..1; force piecewise path (no reliable fit by using 2 points only? no,
	// 2 points still fit linear exactly). Use a param without a fit path by clearing Fit.
	pi := ParamInference{Numeric: true, RealMin: 0, RealMax: 100, table: []realSample{{0, 0}, {0.5, 50}, {1, 100}}}
	// in-range piecewise.
	if n, ok := pi.NormForReal(25); !ok || !approx(n, 0.25, 1e-9) {
		t.Errorf("NormForReal(25) = %v,%v want 0.25,true", n, ok)
	}
	// below range clamps to low end.
	if n, ok := pi.NormForReal(-10); !ok || n != 0 {
		t.Errorf("NormForReal(below) = %v,%v want 0,true", n, ok)
	}
	// above range clamps to high end.
	if n, ok := pi.NormForReal(9999); !ok || n != 1 {
		t.Errorf("NormForReal(above) = %v,%v want 1,true", n, ok)
	}

	// Descending table: real decreases as norm increases.
	desc := ParamInference{Numeric: true, RealMin: 100, RealMax: 0, table: []realSample{{0, 100}, {0.5, 50}, {1, 0}}}
	if n, ok := desc.NormForReal(75); !ok || !approx(n, 0.25, 1e-9) {
		t.Errorf("descending NormForReal(75) = %v,%v want 0.25", n, ok)
	}
	// out of range on a descending table clamps to the near end.
	if n, ok := desc.NormForReal(200); !ok || n != 0 {
		t.Errorf("descending NormForReal(above max real) = %v,%v want norm 0", n, ok)
	}

	// discrete / under-sampled => ok false.
	if _, ok := (ParamInference{Numeric: false}).NormForReal(1); ok {
		t.Error("discrete NormForReal should be ok=false")
	}
	if _, ok := (ParamInference{Numeric: true, table: []realSample{{0, 0}}}).NormForReal(1); ok {
		t.Error("under-sampled NormForReal should be ok=false")
	}
}

func TestBracket(t *testing.T) {
	pi := ParamInference{Numeric: true, table: []realSample{{0, 0}, {0.5, 50}, {1, 100}}}
	if lo, hi, ok := pi.bracket(25); !ok || lo != 0 || hi != 0.5 {
		t.Errorf("bracket(25) = %v,%v,%v want 0,0.5,true", lo, hi, ok)
	}
	if lo, hi, ok := pi.bracket(75); !ok || lo != 0.5 || hi != 1 {
		t.Errorf("bracket(75) = %v,%v,%v want 0.5,1,true", lo, hi, ok)
	}
	// out of range.
	if _, _, ok := pi.bracket(200); ok {
		t.Error("bracket(out of range) should be ok=false")
	}
	// under-sampled.
	if _, _, ok := (ParamInference{Numeric: true, table: []realSample{{0, 0}}}).bracket(1); ok {
		t.Error("bracket(under-sampled) should be ok=false")
	}
}

func TestCurveFitInvert(t *testing.T) {
	// linear invert.
	lin := &CurveFit{Model: "linear", A: 10, B: 2}
	if n, ok := lin.invert(20); !ok || !approx(n, 5, 1e-9) {
		t.Errorf("linear invert(20) = %v,%v want 5,true", n, ok)
	}
	// linear degenerate slope.
	if _, ok := (&CurveFit{Model: "linear", A: 1, B: 0}).invert(5); ok {
		t.Error("linear B==0 invert should be ok=false")
	}
	// exp invert.
	exp := &CurveFit{Model: "exp", A: 20, B: 2}
	// eval(0.5) = 20*e^1; invert should return 0.5.
	tgt := exp.eval(0.5)
	if n, ok := exp.invert(tgt); !ok || !approx(n, 0.5, 1e-9) {
		t.Errorf("exp invert roundtrip = %v,%v want 0.5,true", n, ok)
	}
	// exp target <= 0.
	if _, ok := exp.invert(0); ok {
		t.Error("exp invert(0) should be ok=false")
	}
	if _, ok := exp.invert(-5); ok {
		t.Error("exp invert(neg) should be ok=false")
	}
	// exp A <= 0.
	if _, ok := (&CurveFit{Model: "exp", A: 0, B: 2}).invert(5); ok {
		t.Error("exp A==0 invert should be ok=false")
	}
	// exp B == 0.
	if _, ok := (&CurveFit{Model: "exp", A: 20, B: 0}).invert(5); ok {
		t.Error("exp B==0 invert should be ok=false")
	}
}

func TestLsqDegenerate(t *testing.T) {
	// all-equal x => zero determinant => ok false.
	if _, _, ok := lsq([]float64{1, 1, 1}, []float64{2, 3, 4}); ok {
		t.Error("lsq with constant x should be ok=false")
	}
	// too few points.
	if _, _, ok := lsq([]float64{1}, []float64{2}); ok {
		t.Error("lsq with 1 point should be ok=false")
	}
	// a clean line fits exactly.
	if a, b, ok := lsq([]float64{0, 1, 2}, []float64{1, 3, 5}); !ok || !approx(a, 1, 1e-9) || !approx(b, 2, 1e-9) {
		t.Errorf("lsq clean line = %v,%v,%v want a=1 b=2 true", a, b, ok)
	}
}

func TestFitCurveSelection(t *testing.T) {
	// all-negative reals: exp is rejected (log undefined for non-positive), so linear wins.
	neg := []realSample{{0, -100}, {0.5, -60}, {1, -20}}
	f := fitCurve(neg, 80)
	if f == nil || f.Model != "linear" {
		t.Fatalf("all-negative fit = %+v, want linear", f)
	}
	// range==0 => nil.
	if fitCurve([]realSample{{0, 5}, {1, 5}}, 0) != nil {
		t.Error("fitCurve with rng 0 should be nil")
	}
	// too few samples => nil.
	if fitCurve([]realSample{{0, 5}}, 5) != nil {
		t.Error("fitCurve with 1 sample should be nil")
	}
	// a clean exp curve picks exp.
	expTable := []realSample{
		{0, 20}, {0.25, 20 * math.Pow(1000, 0.25)}, {0.5, 20 * math.Pow(1000, 0.5)}, {1, 20000},
	}
	fe := fitCurve(expTable, 20000-20)
	if fe == nil || fe.Model != "exp" {
		t.Fatalf("clean exp fit = %+v, want exp", fe)
	}
}

func TestFitPowerZeroCrossing(t *testing.T) {
	// A time knob that renders "0 ms" at norm 0 and grows steeply to "32 s" at norm 1, sampled on real =
	// 32*norm^6.5 (rendered ms below 1 s, s above). exp cannot fit (log(0) at the origin) and linear is far off,
	// but the power model captures it and inverts closed-form.
	real := func(n float64) float64 { return 32 * math.Pow(n, 6.5) }
	render := func(n float64) string {
		v := real(n)
		if v < 1 {
			return fmt.Sprintf("%.1f ms", v*1000) // 0.3536 s -> "353.6 ms"
		}
		return fmt.Sprintf("%.2f s", v)
	}
	var samples []ValueSample
	for _, n := range []float64{0, 0.1, 0.25, 0.5, 0.75, 0.9, 1} {
		samples = append(samples, ValueSample{Norm: n, Text: render(n)})
	}
	pi := inferParam(samples)
	if pi.Unit != "s" {
		t.Fatalf("time knob unit = %q, want s", pi.Unit)
	}
	if pi.Fit == nil || pi.Fit.Model != "power" {
		t.Fatalf("time knob fit = %+v, want a power fit", pi.Fit)
	}
	if !pi.analyticReliable() {
		t.Fatalf("power fit should be reliable (rendering rounds slightly), err=%.6f", pi.Fit.MaxRelErr)
	}
	// Invert to a few targets and confirm the analytic path lands close in norm space.
	for _, n := range []float64{0.25, 0.5, 0.75} {
		target := real(n)
		if got, ok := pi.NormForReal(target); !ok || !approx(got, n, 0.01) {
			t.Errorf("power invert(%.4f s) = %v (ok=%v), want norm ~%.4f", target, got, ok, n)
		}
	}
	// norm==0 endpoint: real(0)=0 must invert back to 0 exactly.
	if got, ok := pi.NormForReal(0); !ok || got != 0 {
		t.Errorf("power invert(0) = %v (ok=%v), want 0", got, ok)
	}
}

func TestFitPowerRejectsBipolar(t *testing.T) {
	// A table with a negative real must NOT be modeled as a power law (norm^P is single-signed).
	if f := fitPower([]realSample{{0, -10}, {0.5, 0}, {1, 10}}); f != nil {
		t.Errorf("fitPower on a bipolar table = %+v, want nil", f)
	}
	// Fewer than 2 usable (norm>0, real>0) points => nil (only the norm==0 endpoint is positive here).
	if f := fitPower([]realSample{{0, 0}, {0.0, 0}}); f != nil {
		t.Errorf("fitPower with <2 usable points = %+v, want nil", f)
	}
}

func TestCurveFitInvertPower(t *testing.T) {
	p := &CurveFit{Model: "power", A: 32, B: 6.5}
	// eval(0) is 0 by definition; invert(0) round-trips to 0.
	if p.eval(0) != 0 {
		t.Errorf("power eval(0) = %v, want 0", p.eval(0))
	}
	if n, ok := p.invert(0); !ok || n != 0 {
		t.Errorf("power invert(0) = %v,%v want 0,true", n, ok)
	}
	// round-trip a mid value.
	tgt := p.eval(0.5)
	if n, ok := p.invert(tgt); !ok || !approx(n, 0.5, 1e-9) {
		t.Errorf("power invert roundtrip = %v,%v want 0.5,true", n, ok)
	}
	// negative target: unrepresentable.
	if _, ok := p.invert(-1); ok {
		t.Error("power invert(neg) should be ok=false")
	}
	// degenerate A / B.
	if _, ok := (&CurveFit{Model: "power", A: 0, B: 2}).invert(5); ok {
		t.Error("power A<=0 invert should be ok=false")
	}
	if _, ok := (&CurveFit{Model: "power", A: 2, B: 0}).invert(5); ok {
		t.Error("power B==0 invert should be ok=false")
	}
}

func TestAnalyticReliableThreshold(t *testing.T) {
	// error just under tolerance => reliable.
	ok := ParamInference{Fit: &CurveFit{Model: "linear", A: 0, B: 1, MaxRelErr: fitTol - 1e-6}}
	if !ok.analyticReliable() {
		t.Error("fit just under fitTol should be reliable")
	}
	// error just over tolerance => not reliable.
	over := ParamInference{Fit: &CurveFit{Model: "linear", A: 0, B: 1, MaxRelErr: fitTol + 1e-6}}
	if over.analyticReliable() {
		t.Error("fit just over fitTol should NOT be reliable")
	}
	// B == 0 => not reliable regardless of error.
	flat := ParamInference{Fit: &CurveFit{Model: "linear", A: 1, B: 0, MaxRelErr: 0}}
	if flat.analyticReliable() {
		t.Error("B==0 fit should NOT be reliable")
	}
	// nil fit => not reliable.
	if (ParamInference{}).analyticReliable() {
		t.Error("nil fit should NOT be reliable")
	}
}
