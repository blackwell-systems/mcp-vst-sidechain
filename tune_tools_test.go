// tune_tools_test.go - in-memory tests for the tune_param optimizer against the fake host, whose render makes the
// spectral centroid a monotonic function of the "cutoff" param (200..4000 Hz). No DSP, fully deterministic: this
// covers the search logic (coarse seed + golden refine), scoring for each goal, and the set/restore landing.
// Audio correctness of a REAL plugin lives in the gated tune_live_test.go.

package sidechain

import (
	"context"
	"math"
	"testing"
)

// tuneOn connects a session to the fake host and returns it ready to tune the "cutoff" param.
func tuneSession(t *testing.T) (*session, context.Context, func()) {
	t.Helper()
	fh := startFakeHost(t)
	s := newLiveTestSession()
	ctx := context.Background()
	if _, _, err := s.handleConnectLive(ctx, nil, connectLiveIn{Host: "127.0.0.1", Port: fh.port()}); err != nil {
		fh.stop()
		t.Fatalf("connect_live: %v", err)
	}
	return s, ctx, func() { s.handleDisconnectLive(ctx, nil, emptyIn{}); fh.stop() }
}

func tune(t *testing.T, s *session, ctx context.Context, in tuneParamIn) tuneResult {
	t.Helper()
	res, out, err := s.handleTuneParam(ctx, nil, in)
	if err != nil {
		t.Fatalf("tune_param: %v", err)
	}
	rr, ok := out.(tuneResult)
	if !ok {
		t.Fatalf("tune_param returned no result: %s", textOf(res))
	}
	return rr
}

func TestTuneMaximizeCentroid(t *testing.T) {
	s, ctx, done := tuneSession(t)
	defer done()
	rr := tune(t, s, ctx, tuneParamIn{ID: "cutoff", Measure: "centroid_hz", Goal: "maximize"})
	if rr.BestNormalized < 0.9 {
		t.Fatalf("maximize should push the cutoff to the top, got normalized %.3f (%.0f Hz)", rr.BestNormalized, rr.BestValue)
	}
	if rr.BestValue < 3800 {
		t.Fatalf("maximize centroid should approach 4000 Hz, got %.0f Hz", rr.BestValue)
	}
	// The tool leaves the param at the best value (restore=false default).
	if _, norm, _, _ := s.live.GetParam("cutoff"); math.Abs(norm-rr.BestNormalized) > 1e-6 {
		t.Fatalf("param not left at best: landed %.3f, best %.3f", norm, rr.BestNormalized)
	}
	t.Logf("maximize: %s", rr.Summary)
}

func TestTuneMinimizeCentroid(t *testing.T) {
	s, ctx, done := tuneSession(t)
	defer done()
	rr := tune(t, s, ctx, tuneParamIn{ID: "cutoff", Measure: "centroid_hz", Goal: "minimize"})
	if rr.BestNormalized > 0.1 {
		t.Fatalf("minimize should push the cutoff to the bottom, got normalized %.3f (%.0f Hz)", rr.BestNormalized, rr.BestValue)
	}
	if rr.BestValue > 400 {
		t.Fatalf("minimize centroid should approach 200 Hz, got %.0f Hz", rr.BestValue)
	}
}

func TestTuneTargetCentroid(t *testing.T) {
	s, ctx, done := tuneSession(t)
	defer done()
	rr := tune(t, s, ctx, tuneParamIn{ID: "cutoff", Measure: "centroid_hz", Goal: "target", Target: 2000})
	if math.Abs(rr.BestValue-2000) > 100 {
		t.Fatalf("target 2000 Hz should converge within 100 Hz, got %.0f Hz (normalized %.3f)", rr.BestValue, rr.BestNormalized)
	}
	// Expected position: (2000-200)/3800 = 0.4737.
	if math.Abs(rr.BestNormalized-0.4737) > 0.05 {
		t.Fatalf("target 2000 Hz should sit near normalized 0.474, got %.3f", rr.BestNormalized)
	}
	t.Logf("target: %s", rr.Summary)
}

func TestTuneRestoreLeavesStartValue(t *testing.T) {
	s, ctx, done := tuneSession(t)
	defer done()
	// Set a known starting position first.
	start := 0.3
	if _, _, err := s.handleSetParam(ctx, nil, setParamIn{ID: "cutoff", Normalized: &start}); err != nil {
		t.Fatalf("seed set_param: %v", err)
	}
	rr := tune(t, s, ctx, tuneParamIn{ID: "cutoff", Measure: "centroid_hz", Goal: "maximize", Restore: true})
	if !rr.Restored {
		t.Fatalf("restored flag not set")
	}
	if rr.BestNormalized < 0.9 {
		t.Fatalf("even with restore, the reported best should be the found optimum, got %.3f", rr.BestNormalized)
	}
	// The live param must be back at the start, not the optimum.
	if _, norm, _, _ := s.live.GetParam("cutoff"); math.Abs(norm-start) > 1e-6 {
		t.Fatalf("restore should return the param to %.3f, but it is at %.3f", start, norm)
	}
}

func TestTuneRejectsBadInput(t *testing.T) {
	s, ctx, done := tuneSession(t)
	defer done()
	// Unknown measure -> message, no result payload.
	if _, out, _ := s.handleTuneParam(ctx, nil, tuneParamIn{ID: "cutoff", Measure: "loudness", Goal: "maximize"}); out != nil {
		t.Fatalf("unknown measure should not return a tuneResult")
	}
	// Unknown goal -> message.
	if _, out, _ := s.handleTuneParam(ctx, nil, tuneParamIn{ID: "cutoff", Measure: "centroid_hz", Goal: "sideways"}); out != nil {
		t.Fatalf("unknown goal should not return a tuneResult")
	}
	// Unknown id -> message.
	if _, out, _ := s.handleTuneParam(ctx, nil, tuneParamIn{ID: "nope", Measure: "centroid_hz", Goal: "maximize"}); out != nil {
		t.Fatalf("unknown id should not return a tuneResult")
	}
}
