// infer_test.go - Phase-1 inference tests. The fixture tests use REAL rendered strings captured from Surge XT
// and TAL-NoiseMaker over the control socket, so they prove the inference against actual plugin output without
// needing a live plugin in CI. TestInferLive is an optional end-to-end that drives a running host (skipped
// unless SIDECHAIN_LIVE_PORT is set) and demonstrates real-value control: infer -> invert -> set -> read back.

package sidechain

import (
	"context"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
)

func approx(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

func TestParseValueText(t *testing.T) {
	cases := []struct {
		in      string
		val     float64
		unit    string
		numeric bool
	}{
		{"13.75 Hz", 13.75, "hz", true},
		{"-48.00 dB", -48, "db", true},
		{"353.6 ms", 353.6, "ms", true},
		{"32.00 s", 32, "s", true},
		{"-100.00 % (Left)", -100, "%", true}, // parenthetical annotation stripped
		{"0 semitones", 0, "semitones", true},
		{"0.5000000", 0.5, "", true}, // unitless (plugin renders the raw number, e.g. TAL cutoff)
		{"Off", 0, "", false},        // enum/bool label
		{"Ladder", 0, "", false},
	}
	for _, c := range cases {
		v, u, num := parseValueText(c.in)
		if num != c.numeric || u != c.unit || (num && !approx(v, c.val, 1e-6)) {
			t.Errorf("parseValueText(%q) = (%v,%q,%v), want (%v,%q,%v)", c.in, v, u, num, c.val, c.unit, c.numeric)
		}
	}
}

func TestInferSurgeCutoff(t *testing.T) {
	// A Filter 1 Cutoff: real Surge sweep. Log-frequency => exponential curve.
	pi := inferParam([]ValueSample{{0, "13.75 Hz"}, {0.5, "587.33 Hz"}, {1, "25087.71 Hz"}})
	if !pi.Numeric || pi.Unit != "hz" {
		t.Fatalf("cutoff: numeric=%v unit=%q, want true/hz", pi.Numeric, pi.Unit)
	}
	if !approx(pi.RealMin, 13.75, 0.01) || !approx(pi.RealMax, 25087.71, 0.01) {
		t.Errorf("cutoff range = %v..%v", pi.RealMin, pi.RealMax)
	}
	if pi.Curve != "exponential" {
		t.Errorf("cutoff curve = %q, want exponential", pi.Curve)
	}
	if pi.Bipolar {
		t.Error("cutoff should not be bipolar")
	}
}

func TestInferBipolarAndUnits(t *testing.T) {
	pan := inferParam([]ValueSample{{0, "-100.00 % (Left)"}, {0.5, "0.00 % (Center)"}, {1, "100.00 % (Right)"}})
	if !pan.Bipolar || pan.Unit != "%" || pan.Curve != "linear" {
		t.Errorf("pan = %+v, want bipolar %% linear", pan)
	}
	// Linear inverse: 50%% is 3/4 of the way from -100 to +100.
	if n, ok := pan.NormForReal(50); !ok || !approx(n, 0.75, 1e-6) {
		t.Errorf("pan NormForReal(50) = %v (ok=%v), want 0.75", n, ok)
	}

	vol := inferParam([]ValueSample{{0, "-48.00 dB"}, {0.5, "-24.00 dB"}, {1, "0.00 dB"}})
	if vol.Unit != "db" || vol.Curve != "linear" || vol.Bipolar {
		t.Errorf("vol = %+v, want db linear non-bipolar", vol)
	}
	if n, ok := vol.NormForReal(-24); !ok || !approx(n, 0.5, 1e-6) {
		t.Errorf("vol NormForReal(-24 dB) = %v, want 0.5", n)
	}
}

func TestInferTimeUnitSwitch(t *testing.T) {
	// Amp EG Attack: ms at the low end, s at the top. Must fold into one base unit (seconds) and stay monotonic.
	pi := inferParam([]ValueSample{{0, "0.0 ms"}, {0.5, "353.6 ms"}, {1, "32.00 s"}})
	if pi.Unit != "s" {
		t.Fatalf("attack unit = %q, want s", pi.Unit)
	}
	if !approx(pi.RealMin, 0, 1e-9) || !approx(pi.RealMax, 32, 1e-9) {
		t.Errorf("attack range = %v..%v s, want 0..32", pi.RealMin, pi.RealMax)
	}
	if pi.Curve != "exponential" {
		t.Errorf("attack curve = %q, want exponential", pi.Curve)
	}
}

func TestInferDiscreteHidingAsFloat(t *testing.T) {
	// Surge "A Link Resonance": a bool rendered as a float. Detected as discrete via non-numeric text.
	pi := inferParam([]ValueSample{{0, "Off"}, {0.5, "Off"}, {1, "On"}})
	if pi.Numeric {
		t.Fatal("Off/On param should be inferred non-numeric (discrete)")
	}
	if len(pi.Labels) != 2 || pi.Labels[0] != "Off" || pi.Labels[1] != "On" {
		t.Errorf("labels = %v, want [Off On]", pi.Labels)
	}
}

func TestInferUnitlessNumeric(t *testing.T) {
	// TAL-NoiseMaker Filter Cutoff: getText just echoes the normalized number. Numeric but no unit => we learn
	// nothing about real units and fall back to normalized control (0..1), which still inverts.
	pi := inferParam([]ValueSample{{0, "0.0000000"}, {0.5, "0.5000000"}, {1, "1.0000000"}})
	if !pi.Numeric || pi.Unit != "" {
		t.Fatalf("unitless = %+v, want numeric with empty unit", pi)
	}
	if n, ok := pi.NormForReal(0.5); !ok || !approx(n, 0.5, 1e-6) {
		t.Errorf("unitless NormForReal(0.5) = %v, want 0.5", n)
	}
}

// TestInferLive drives a running host: SIDECHAIN_LIVE_PORT=<port> SIDECHAIN_LIVE_PARAM=<id> go test -run Live.
// It sweeps the param, prints the inference, and (for a unit-bearing param) demonstrates real-value control by
// inverting to a target and reading the plugin's value back.
func TestInferLive(t *testing.T) {
	portStr := os.Getenv("SIDECHAIN_LIVE_PORT")
	if portStr == "" {
		t.Skip("set SIDECHAIN_LIVE_PORT (and SIDECHAIN_LIVE_PARAM) to run the live probe against a host")
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("bad SIDECHAIN_LIVE_PORT: %v", err)
	}
	id := os.Getenv("SIDECHAIN_LIVE_PARAM")
	if id == "" {
		t.Fatal("set SIDECHAIN_LIVE_PARAM to a param id")
	}
	lc, err := dialLive("127.0.0.1", port)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer lc.Close()

	points := []float64{0, .1, .2, .3, .4, .5, .6, .7, .8, .9, 1}
	samples, err := lc.SampleText(id, points)
	if err != nil {
		t.Fatalf("sample: %v", err)
	}
	pi := inferParam(samples)
	t.Logf("param %s: numeric=%v unit=%q range=%.3f..%.3f bipolar=%v curve=%s labels=%v",
		id, pi.Numeric, pi.Unit, pi.RealMin, pi.RealMax, pi.Bipolar, pi.Curve, pi.Labels)
	for _, s := range samples {
		t.Logf("   norm %.2f -> %q", s.Norm, s.Text)
	}

	if !pi.Numeric || pi.Unit == "" {
		t.Logf("no real unit to demonstrate control against (fine: normalized-only param)")
		return
	}
	// Aim for a value 30%% of the way into the range (in real terms) and see how close we land.
	target := pi.RealMin + 0.30*(pi.RealMax-pi.RealMin)
	norm, ok := pi.NormForReal(target)
	if !ok {
		t.Fatal("NormForReal failed on a numeric param")
	}
	_, _, text, err := lc.SetParam(id, norm, false)
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	gv, gu, _ := parseValueText(text)
	got, _ := normalizeUnit(gv, gu) // fold readback into the same base unit as target (ms->s, kHz->Hz)
	rng := math.Abs(pi.RealMax - pi.RealMin)
	t.Logf("control demo: target %.3f %s -> norm %.4f -> plugin says %q (parsed %.3f)", target, pi.Unit, norm, text, got)
	if rng > 0 && math.Abs(got-target)/rng > 0.15 {
		t.Errorf("real-value control off by >15%% of range: target %.3f, got %.3f (%s)", target, got, text)
	}
}

func TestCurveFit(t *testing.T) {
	// A log-frequency cutoff (geometric 20 -> 20000 Hz, i.e. real = 20*1000^norm) should fit an exp model with
	// tiny error and invert closed-form. This is the shape Surge's real cutoff took (its live fit hit ~0.007%).
	cut := inferParam([]ValueSample{
		{0, "20.00 Hz"}, {0.25, "112.47 Hz"}, {0.5, "632.46 Hz"}, {0.75, "3556.56 Hz"}, {1, "20000.00 Hz"},
	})
	if cut.Fit == nil || cut.Fit.Model != "exp" {
		t.Fatalf("cutoff fit = %+v, want exp", cut.Fit)
	}
	if !cut.analyticReliable() {
		t.Fatalf("cutoff exp fit should be reliable, err=%.4f", cut.Fit.MaxRelErr)
	}
	if n, ok := cut.NormForReal(632.46); !ok || !approx(n, 0.5, 0.01) {
		t.Errorf("analytic invert(632 Hz) = %v, want ~0.5", n)
	}

	// A linear dB curve fits linear exactly.
	vol := inferParam([]ValueSample{{0, "-48 dB"}, {0.5, "-24 dB"}, {1, "0 dB"}})
	if vol.Fit == nil || vol.Fit.Model != "linear" || vol.Fit.MaxRelErr > 1e-9 {
		t.Fatalf("vol fit = %+v, want exact linear", vol.Fit)
	}

	// A curve through zero (a time knob 0 ms -> 353.6 ms -> 32 s) cannot be exp (log undefined at 0) and is too
	// curved for linear, but the power model real = A*norm^P captures it (real(0)=0 naturally). 353.6 ms = 0.3536
	// s = 32 * 0.5^6.5, so the samples lie exactly on real = 32*norm^6.5 and the fit should be reliable.
	tm := inferParam([]ValueSample{{0, "0 ms"}, {0.5, "353.6 ms"}, {1, "32.0 s"}})
	if tm.Fit == nil || tm.Fit.Model != "power" {
		t.Fatalf("0-based steep time curve should fit power, got %+v", tm.Fit)
	}
	if !tm.analyticReliable() {
		t.Errorf("power fit on the 0-based time curve should be reliable, err=%.6f", tm.Fit.MaxRelErr)
	}
	if n, ok := tm.NormForReal(0.3536); !ok || !approx(n, 0.5, 0.01) {
		t.Errorf("analytic power invert(0.3536 s) = %v, want ~0.5", n)
	}
}

// TestWiredLive drives the actual MCP handlers (describe_param + set_param real=) against a running host, to
// prove the wired path end-to-end on a real plugin. Gated on SIDECHAIN_LIVE_PORT + SIDECHAIN_LIVE_CATALOG
// (+ SIDECHAIN_LIVE_PARAM).
func TestWiredLive(t *testing.T) {
	port := os.Getenv("SIDECHAIN_LIVE_PORT")
	catPath := os.Getenv("SIDECHAIN_LIVE_CATALOG")
	id := os.Getenv("SIDECHAIN_LIVE_PARAM")
	if port == "" || catPath == "" || id == "" {
		t.Skip("set SIDECHAIN_LIVE_PORT, SIDECHAIN_LIVE_CATALOG, SIDECHAIN_LIVE_PARAM to run")
	}
	data, err := os.ReadFile(catPath)
	if err != nil {
		t.Fatalf("read catalog: %v", err)
	}
	cat, err := loadCatalogJSON(data)
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	s := newSession(cat)
	ctx := context.Background()
	p, _ := strconv.Atoi(port)
	// handleConnectLive reports a failed dial in its TEXT (err stays nil, by tool convention), so assert on the
	// reply. Without this, a broken plugin/socket path silently no-ops and the test false-greens.
	cres, _, err := s.handleConnectLive(ctx, nil, connectLiveIn{Host: "127.0.0.1", Port: p})
	if err != nil {
		t.Fatalf("connect_live: %v", err)
	}
	if !strings.Contains(textOf(cres), "Connected LIVE") {
		t.Fatalf("connect_live did not connect: %s", textOf(cres))
	}

	dres, _, err := s.handleDescribeParam(ctx, nil, describeParamIn{ID: id})
	if err != nil {
		t.Fatalf("describe_param: %v", err)
	}
	t.Logf("describe_param -> %s", textOf(dres))
	if strings.Contains(textOf(dres), "not live") {
		t.Fatalf("describe_param ran while not live: %s", textOf(dres))
	}

	// The CI target (a filter cutoff) is a numeric, unit-bearing param, so we REQUIRE the real-unit path to work
	// here rather than skipping. This is the assertion that actually exercises describe -> infer -> set real.
	pi := s.infer[id]
	if !pi.Numeric || pi.Unit == "" {
		t.Fatalf("expected a numeric, unit-bearing param, got numeric=%v unit=%q", pi.Numeric, pi.Unit)
	}
	target := pi.RealMin + 0.30*(pi.RealMax-pi.RealMin)
	sres, _, err := s.handleSetParam(ctx, nil, setParamIn{ID: id, Real: &target})
	if err != nil {
		t.Fatalf("set_param real: %v", err)
	}
	if !strings.Contains(textOf(sres), "Set LIVE") {
		t.Fatalf("set_param real did not drive the plugin: %s", textOf(sres))
	}
	t.Logf("set_param real=%.3f %s -> %s", target, pi.Unit, textOf(sres))
}
