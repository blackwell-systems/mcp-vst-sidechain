// infer.go - Phase 1 of the semantic layer: recover real-unit meaning from a hosted plugin's own value text.
//
// A hosted VST3/AU param only exposes a normalized 0..1 scalar through the base API, so the catalog reports it
// as "0..1, mystery". But the plugin ships its own value-to-text formatter (getText), and sweeping it across
// the 0..1 range often reveals everything we actually want: the real unit (Hz/dB/ms/%/semitones), the range,
// whether it is bipolar, the shape of the curve, and even params that are really discrete toggles/enums hiding
// as floats. This is deterministic - no model needed. It is also plugin-dependent: a plugin that does not
// implement getText (many params in TAL-NoiseMaker) yields unitless numbers and we simply learn nothing, which
// is fine - the layer degrades to "normalized only" rather than guessing.
//
// The inverse (NormForReal) is what turns this into control: given "set cutoff to 1000 Hz", interpolate the
// sampled (norm, realValue) table to the normalized value to send. Accuracy scales with sample count; a
// production probe could binary-search getText for an exact hit.

package sidechain

import (
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ValueSample is one (normalized, rendered-text) point from a getText sweep.
type ValueSample struct {
	Norm float64
	Text string
}

// ParamInference is what a sweep tells us about a param. Numeric=false means the plugin renders labels, not
// numbers (a discrete/enum/bool disguised as a float); Labels then holds the distinct rendered strings.
type ParamInference struct {
	Numeric bool      `json:"numeric"`
	Unit    string    `json:"unit,omitempty"`    // base unit after family normalization (hz, db, s, %, semitones); "" = unitless
	RealMin float64   `json:"realMin,omitempty"` // real value at norm 0 (in Unit)
	RealMax float64   `json:"realMax,omitempty"` // real value at norm 1
	Bipolar bool      `json:"bipolar,omitempty"` // RealMin < 0 < RealMax
	Curve   string    `json:"curve,omitempty"`   // linear | logarithmic | exponential | flat | unknown
	Labels  []string  `json:"labels,omitempty"`  // distinct rendered strings when !Numeric
	Fit     *CurveFit `json:"fit,omitempty"`     // closed-form model for norm<->real, when one fits well

	table         []realSample       // sorted by Norm; retained for inversion
	discreteNorms map[string]float64 // when !Numeric: label -> representative (median) norm that renders it, for set_param choice=
}

type realSample struct{ norm, real float64 }

// CurveFit is a closed-form model of the norm->real mapping recovered by least squares. "linear" is
// real = A + B*norm; "exp" is real = A*exp(B*norm) (fit in log space, valid when all reals are positive);
// "power" is real = A*norm^B (fit in log-log space, for zero-crossing curves like a time knob 0 ms -> 32 s
// where exp cannot apply because log(0) is undefined). MaxRelErr is the worst sample error as a fraction of the
// real range; a small value means the model captures the plugin's curve and can be inverted analytically (no
// per-set probing).
type CurveFit struct {
	Model     string  `json:"model"`
	A         float64 `json:"a"`
	B         float64 `json:"b"`
	MaxRelErr float64 `json:"maxRelErr"`
}

// fitTol: analytic inversion is trusted only when the fit's worst error is within this fraction of the range.
const fitTol = 0.01

func (f *CurveFit) eval(norm float64) float64 {
	switch f.Model {
	case "exp":
		return f.A * math.Exp(f.B*norm)
	case "power":
		if norm <= 0 { // real(0) = 0 by definition (norm^B is undefined/degenerate at 0)
			return 0
		}
		return f.A * math.Pow(norm, f.B)
	default:
		return f.A + f.B*norm
	}
}

// invert solves norm for a target real value. ok=false when the model cannot represent the target (e.g. a
// non-positive target on an exp model, or a degenerate slope).
func (f *CurveFit) invert(target float64) (norm float64, ok bool) {
	switch f.Model {
	case "exp":
		if f.A <= 0 || f.B == 0 || target <= 0 {
			return 0, false
		}
		return math.Log(target/f.A) / f.B, true
	case "power":
		if f.A <= 0 || f.B == 0 || target < 0 {
			return 0, false
		}
		if target == 0 {
			return 0, true // real(0)=0, so norm 0 lands it exactly
		}
		return math.Pow(target/f.A, 1/f.B), true
	default:
		if f.B == 0 {
			return 0, false
		}
		return (target - f.A) / f.B, true
	}
}

// analyticReliable reports whether the fitted model is good enough to invert in closed form.
func (pi ParamInference) analyticReliable() bool {
	return pi.Fit != nil && pi.Fit.MaxRelErr <= fitTol && pi.Fit.B != 0
}

// lsq is an ordinary least-squares line fit y = a + b*x.
func lsq(xs, ys []float64) (a, b float64, ok bool) {
	n := float64(len(xs))
	if n < 2 {
		return 0, 0, false
	}
	var sx, sy, sxx, sxy float64
	for i := range xs {
		sx += xs[i]
		sy += ys[i]
		sxx += xs[i] * xs[i]
		sxy += xs[i] * ys[i]
	}
	d := n*sxx - sx*sx
	if d == 0 {
		return 0, 0, false
	}
	b = (n*sxy - sx*sy) / d
	a = (sy - b*sx) / n
	return a, b, true
}

// fitCurve fits a linear and (when all reals are positive) an exponential model, and returns whichever has the
// lower error over the samples. nil if the range is degenerate or there are too few samples.
func fitCurve(table []realSample, rng float64) *CurveFit {
	if len(table) < 2 || rng <= 0 {
		return nil
	}
	xs := make([]float64, len(table))
	ys := make([]float64, len(table))
	for i, s := range table {
		xs[i], ys[i] = s.norm, s.real
	}
	var best *CurveFit
	consider := func(f *CurveFit) {
		var maxe float64
		for _, s := range table {
			if e := math.Abs(f.eval(s.norm)-s.real) / rng; e > maxe {
				maxe = e
			}
		}
		f.MaxRelErr = maxe
		if best == nil || f.MaxRelErr < best.MaxRelErr {
			best = f
		}
	}
	if a, b, ok := lsq(xs, ys); ok {
		consider(&CurveFit{Model: "linear", A: a, B: b})
	}
	allPos := true
	lys := make([]float64, len(ys))
	for i, y := range ys {
		if y <= 0 {
			allPos = false
			break
		}
		lys[i] = math.Log(y)
	}
	if allPos {
		if p, q, ok := lsq(xs, lys); ok {
			consider(&CurveFit{Model: "exp", A: math.Exp(p), B: q})
		}
	}
	if f := fitPower(table); f != nil {
		consider(f)
	}
	return best
}

// fitPower fits real = A*norm^P in log-log space (log(real) = log(A) + P*log(norm)) by least squares over the
// samples with norm > 0 AND real > 0. Zero-crossing curves (a time knob that renders "0 ms" at norm 0 and
// "32 s" at norm 1) suit this model where exp cannot: exp needs all reals positive and cannot pass through the
// origin, whereas real=A*norm^P gives real(0)=0 for free. Non-positive reals are skipped from the fit, but any
// negative real disqualifies the model entirely (a bipolar param is not a power law); the norm==0 endpoint is
// skipped from the fit and captured by eval(0)=0. Needs at least 2 usable (norm>0, real>0) points. Returns nil
// (leaving the fit to linear/exp) when it does not apply. Caller (fitCurve) scores MaxRelErr over ALL samples.
func fitPower(table []realSample) *CurveFit {
	var lx, ly []float64
	for _, s := range table {
		if s.real < 0 {
			return nil // bipolar / sign-crossing: power law does not represent it
		}
		if s.norm > 0 && s.real > 0 {
			lx = append(lx, math.Log(s.norm))
			ly = append(ly, math.Log(s.real))
		}
	}
	if len(lx) < 2 {
		return nil
	}
	a, b, ok := lsq(lx, ly)
	if !ok {
		return nil
	}
	return &CurveFit{Model: "power", A: math.Exp(a), B: b}
}

// leading signed number + optional unit token (letters or %). Anything after (e.g. " (Left)") is stripped first.
var valueTextRe = regexp.MustCompile(`^([+-]?[0-9]+(?:\.[0-9]+)?)\s*([A-Za-z%]+)?`)

// parseValueText pulls a numeric value + unit out of one rendered string. numeric=false when the string does
// not begin with a number (an enum/bool label like "Off" or "Ladder").
func parseValueText(s string) (val float64, unit string, numeric bool) {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '('); i >= 0 { // drop a trailing annotation: "-100.00 % (Left)" -> "-100.00 %"
		s = strings.TrimSpace(s[:i])
	}
	m := valueTextRe.FindStringSubmatch(s)
	if m == nil {
		return 0, "", false
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, "", false
	}
	return v, strings.ToLower(m[2]), true
}

// normalizeUnit folds a value+unit into a single base unit per family so a sweep that switches units (ms -> s,
// Hz -> kHz) still builds a monotonic real table.
func normalizeUnit(val float64, unit string) (float64, string) {
	switch unit {
	case "ms":
		return val / 1000.0, "s"
	case "khz":
		return val * 1000.0, "hz"
	default:
		return val, unit
	}
}

// inferParam turns a getText sweep into a ParamInference.
func inferParam(samples []ValueSample) ParamInference {
	var table []realSample
	var labels []string
	unitCount := map[string]int{}
	labelNorms := map[string][]float64{} // label -> the norms that rendered it (for a discrete-as-float set path)
	for _, s := range samples {
		v, u, numeric := parseValueText(s.Text)
		if !numeric {
			lbl := strings.TrimSpace(s.Text)
			if !containsStr(labels, lbl) {
				labels = append(labels, lbl)
			}
			labelNorms[lbl] = append(labelNorms[lbl], s.Norm)
			continue
		}
		nv, bu := normalizeUnit(v, u)
		table = append(table, realSample{s.Norm, nv})
		if bu != "" {
			unitCount[bu]++
		}
	}

	// Any non-numeric sample => this is really a discrete control (Off/On, waveform names, ...). Record a
	// representative norm per label (the median of the norms that rendered it) so set_param choice= can land the
	// control on a chosen label even though the catalog only exposes it as a float.
	if len(labels) > 0 {
		dn := make(map[string]float64, len(labels))
		for lbl, ns := range labelNorms {
			dn[lbl] = medianOf(ns)
		}
		return ParamInference{Numeric: false, Labels: labels, discreteNorms: dn}
	}
	if len(table) == 0 {
		return ParamInference{Numeric: false}
	}

	sort.Slice(table, func(i, j int) bool { return table[i].norm < table[j].norm })
	base := ""
	for u, c := range unitCount {
		if c > unitCount[base] {
			base = u
		}
	}
	pi := ParamInference{
		Numeric: true,
		Unit:    base,
		RealMin: table[0].real,
		RealMax: table[len(table)-1].real,
		table:   table,
	}
	pi.Bipolar = pi.RealMin < 0 && pi.RealMax > 0
	pi.Curve = classifyCurve(table)
	pi.Fit = fitCurve(table, math.Abs(pi.RealMax-pi.RealMin))
	return pi
}

// classifyCurve reads the shape from where the midpoint lands relative to a straight line between the ends.
func classifyCurve(t []realSample) string {
	if len(t) < 3 {
		return "unknown"
	}
	r0, r1 := t[0].real, t[len(t)-1].real
	if r1 == r0 {
		return "flat"
	}
	mid, best := t[0], math.Inf(1)
	for _, s := range t {
		if d := math.Abs(s.norm - 0.5); d < best {
			best, mid = d, s
		}
	}
	switch frac := (mid.real - r0) / (r1 - r0); {
	case frac < 0.4:
		return "exponential" // slow start, fast end (e.g. frequency in Hz, time)
	case frac > 0.6:
		return "logarithmic" // fast start, slow end
	default:
		return "linear"
	}
}

// NormForReal inverts the sampled table: the normalized value to send to land near a target real value. Uses
// piecewise-linear interpolation over the samples (assumes a monotonic mapping), clamped to the range. ok=false
// if the param is discrete or under-sampled.
func (pi ParamInference) NormForReal(target float64) (norm float64, ok bool) {
	t := pi.table
	if !pi.Numeric || len(t) < 2 {
		return 0, false
	}
	// Analytic first: when a closed-form model fits the samples well, invert it directly (no round-trips, and
	// this is what makes batch real-unit authoring cheap). Falls through to piecewise-linear when the fit is
	// poor or cannot represent the target.
	if pi.analyticReliable() {
		if n, ok := pi.Fit.invert(target); ok {
			return clamp01(n), true
		}
	}
	for i := 0; i+1 < len(t); i++ {
		a, b := t[i], t[i+1]
		lo, hi := a.real, b.real
		if lo > hi {
			lo, hi = hi, lo
		}
		if target >= lo && target <= hi {
			if b.real == a.real {
				return a.norm, true
			}
			f := (target - a.real) / (b.real - a.real)
			return clamp01(a.norm + f*(b.norm-a.norm)), true
		}
	}
	// Out of range: clamp to whichever end is nearer in real terms.
	asc := t[len(t)-1].real >= t[0].real
	if (asc && target <= t[0].real) || (!asc && target >= t[0].real) {
		return t[0].norm, true
	}
	return t[len(t)-1].norm, true
}

// bracket returns the normalized interval [lo,hi] whose sampled real values straddle target - the starting
// bracket for a binary-search refinement. ok=false if target is outside the sampled range or under-sampled.
func (pi ParamInference) bracket(target float64) (lo, hi float64, ok bool) {
	t := pi.table
	if !pi.Numeric || len(t) < 2 {
		return 0, 0, false
	}
	for i := 0; i+1 < len(t); i++ {
		a, b := t[i], t[i+1]
		loR, hiR := a.real, b.real
		if loR > hiR {
			loR, hiR = hiR, loR
		}
		if target >= loR && target <= hiR {
			return a.norm, b.norm, true
		}
	}
	return 0, 0, false
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// medianOf returns the median of a non-empty slice (average of the two middle values for an even count). It
// sorts a copy, so the caller's slice order is preserved.
func medianOf(vs []float64) float64 {
	c := append([]float64(nil), vs...)
	sort.Float64s(c)
	n := len(c)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return c[n/2]
	}
	return (c[n/2-1] + c[n/2]) / 2
}

func containsStr(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
