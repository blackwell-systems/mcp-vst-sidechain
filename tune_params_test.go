// tune_params_test.go - in-memory tests for the tune_params coordinate-descent co-optimizer. The fake host's render
// makes the centroid respond to "cutoff" (200..4000 Hz) and rms to "gain" (-18.4..-0.4 dB), giving two independent
// axes to co-optimize. Deterministic, no DSP. Coupled/shared-objective behavior is covered by the gated real-host
// modulation test (LFO rate + depth on TAL).

package sidechain

import (
	"context"
	"math"
	"testing"
)

// tuneParamsSession connects a session with a two-param catalog (cutoff + gain) to the fake host.
func tuneParamsSession(t *testing.T) (*session, context.Context, func()) {
	t.Helper()
	fh := startFakeHost(t)
	cat := NewCatalog([]ParamDef{
		{ID: "cutoff", Label: "Cutoff", Type: "float", Min: 20, Max: 20000, Default: 1000, Group: "filter"},
		{ID: "gain", Label: "Gain", Type: "float", Min: 0, Max: 1, Default: 0.5, Group: "amp"},
	})
	s := newSession(cat)
	ctx := context.Background()
	if _, _, err := s.handleConnectLive(ctx, nil, connectLiveIn{Host: "127.0.0.1", Port: fh.port()}); err != nil {
		fh.stop()
		t.Fatalf("connect_live: %v", err)
	}
	return s, ctx, func() { s.handleDisconnectLive(ctx, nil, emptyIn{}); fh.stop() }
}

func tuneParams(t *testing.T, s *session, ctx context.Context, in tuneParamsIn) tuneParamsResult {
	t.Helper()
	res, out, err := s.handleTuneParams(ctx, nil, in)
	if err != nil {
		t.Fatalf("tune_params: %v", err)
	}
	rr, ok := out.(tuneParamsResult)
	if !ok {
		t.Fatalf("tune_params returned no result: %s", textOf(res))
	}
	return rr
}

func TestTuneParamsTwoTargets(t *testing.T) {
	s, ctx, done := tuneParamsSession(t)
	defer done()

	rr := tuneParams(t, s, ctx, tuneParamsIn{Knobs: []tuneKnob{
		{ID: "cutoff", Measure: "centroid_hz", Goal: "target", Target: 2000}, // -> cutoff (2000-200)/3800 = 0.474
		{ID: "gain", Measure: "rms_db", Goal: "target", Target: -10},         // -> gain (-10+18.4)/18 = 0.467
	}})
	t.Logf("co-tune: %s", rr.Summary)

	if len(rr.Knobs) != 2 {
		t.Fatalf("expected 2 knob results, got %d", len(rr.Knobs))
	}
	// Both objectives should be met independently.
	if math.Abs(rr.Knobs[0].BestValue-2000) > 100 {
		t.Errorf("cutoff knob: centroid landed at %.0f Hz, want ~2000", rr.Knobs[0].BestValue)
	}
	if math.Abs(rr.Knobs[1].BestValue-(-10)) > 1.0 {
		t.Errorf("gain knob: rms landed at %.2f dB, want ~-10", rr.Knobs[1].BestValue)
	}
	// Both live params must be left at their best (restore=false default).
	if _, n, _, _ := s.live.GetParam("cutoff"); math.Abs(n-rr.Knobs[0].BestNormalized) > 1e-6 {
		t.Errorf("cutoff not left at best: live %.3f, best %.3f", n, rr.Knobs[0].BestNormalized)
	}
	if _, n, _, _ := s.live.GetParam("gain"); math.Abs(n-rr.Knobs[1].BestNormalized) > 1e-6 {
		t.Errorf("gain not left at best: live %.3f, best %.3f", n, rr.Knobs[1].BestNormalized)
	}
	// Independent axes converge in the first round, so the early-stop should keep it to a couple of rounds.
	if rr.Rounds > 3 {
		t.Errorf("expected convergence within 3 rounds, ran %d", rr.Rounds)
	}
}

func TestTuneParamsRestore(t *testing.T) {
	s, ctx, done := tuneParamsSession(t)
	defer done()
	// Seed known starting positions.
	c, g := 0.2, 0.7
	s.handleSetParam(ctx, nil, setParamIn{ID: "cutoff", Normalized: &c})
	s.handleSetParam(ctx, nil, setParamIn{ID: "gain", Normalized: &g})

	rr := tuneParams(t, s, ctx, tuneParamsIn{Restore: true, Knobs: []tuneKnob{
		{ID: "cutoff", Measure: "centroid_hz", Goal: "maximize"},
		{ID: "gain", Measure: "rms_db", Goal: "maximize"},
	}})
	if !rr.Restored {
		t.Fatalf("restored flag not set")
	}
	// The reported best is still the found optimum (both maximize -> near 1.0).
	if rr.Knobs[0].BestNormalized < 0.9 || rr.Knobs[1].BestNormalized < 0.9 {
		t.Errorf("maximize should report best near 1.0, got cutoff=%.3f gain=%.3f", rr.Knobs[0].BestNormalized, rr.Knobs[1].BestNormalized)
	}
	// But the live params must be back at their starting positions.
	if _, n, _, _ := s.live.GetParam("cutoff"); math.Abs(n-c) > 1e-6 {
		t.Errorf("cutoff not restored: live %.3f, want %.3f", n, c)
	}
	if _, n, _, _ := s.live.GetParam("gain"); math.Abs(n-g) > 1e-6 {
		t.Errorf("gain not restored: live %.3f, want %.3f", n, g)
	}
}

func TestTuneParamsRejectsBadInput(t *testing.T) {
	s, ctx, done := tuneParamsSession(t)
	defer done()
	// No knobs.
	if _, out, _ := s.handleTuneParams(ctx, nil, tuneParamsIn{}); out != nil {
		t.Errorf("empty knobs should not return a result")
	}
	// Unknown id in one knob.
	if _, out, _ := s.handleTuneParams(ctx, nil, tuneParamsIn{Knobs: []tuneKnob{
		{ID: "cutoff", Measure: "centroid_hz", Goal: "maximize"},
		{ID: "nope", Measure: "rms_db", Goal: "maximize"},
	}}); out != nil {
		t.Errorf("unknown id should not return a result")
	}
	// Unknown measure.
	if _, out, _ := s.handleTuneParams(ctx, nil, tuneParamsIn{Knobs: []tuneKnob{
		{ID: "cutoff", Measure: "loudness", Goal: "maximize"},
	}}); out != nil {
		t.Errorf("unknown measure should not return a result")
	}
}
