// modulation_live_test.go - GATED real-host tests for Tier 2.5 temporal (modulation-aware) measurement. They need
// a running JUCE host with a plugin that has an active LFO in its signal path, so they SKIP cleanly when the env
// vars are unset and the normal `go test` stays green.
//
// Gate: set SIDECHAIN_LIVE_PORT, SIDECHAIN_LIVE_CATALOG, and SIDECHAIN_LIVE_PARAM (an LFO-rate parameter id) to
// run these. Optionally set SIDECHAIN_LIVE_LFO_TARGET to override the target rate in Hz (default 4.0).

package sidechain

import (
	"context"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestRenderTemporalLive renders with Temporal=true and asserts the returned modulation block has sane fields: a
// non-nil block, a frameMs matching the request, and a dominant that is one of the expected values.
func TestRenderTemporalLive(t *testing.T) {
	portStr := os.Getenv("SIDECHAIN_LIVE_PORT")
	catPath := os.Getenv("SIDECHAIN_LIVE_CATALOG")
	if portStr == "" || catPath == "" {
		t.Skip("set SIDECHAIN_LIVE_PORT and SIDECHAIN_LIVE_CATALOG to run the temporal live test")
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("bad SIDECHAIN_LIVE_PORT %q: %v", portStr, err)
	}
	data, err := os.ReadFile(catPath)
	if err != nil {
		t.Fatalf("read catalog %q: %v", catPath, err)
	}
	cat, err := loadCatalogJSON(data)
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	s := newSession(cat)
	ctx := context.Background()
	cres, _, err := s.handleConnectLive(ctx, nil, connectLiveIn{Host: "127.0.0.1", Port: port})
	if err != nil {
		t.Fatalf("connect_live: %v", err)
	}
	if !strings.Contains(textOf(cres), "Connected LIVE") {
		t.Fatalf("connect_live did not connect: %s", textOf(cres))
	}
	defer s.handleDisconnectLive(ctx, nil, emptyIn{})

	// Render a HELD note with temporal analysis on. A long gate gives the sustain window several LFO cycles to
	// resolve the rate (the analysis skips the attack and, for an instrument, the post-note-off release tail). This
	// test expects an LFO routed to the filter cutoff (the CI step arranges that on TAL); it asserts the LFO is
	// actually measured, not merely that the block is well-formed.
	res, out, err := s.handleRenderAndMeasure(ctx, nil, renderAndMeasureIn{
		Note: 60, Velocity: 0.9, GateMs: 3000, DurationMs: 3500,
		Temporal: true, FrameMs: 25,
	})
	if err != nil {
		t.Fatalf("render temporal: %v", err)
	}
	rr, ok := out.(renderResult)
	if !ok {
		t.Fatalf("structured output not a renderResult: %s", textOf(res))
	}
	t.Logf("temporal render: %s", rr.Summary)

	m := rr.Measurement
	if m.Modulation == nil {
		t.Fatalf("temporal render: Measurement.Modulation is nil; host may not support Tier 2.5")
	}
	mod := m.Modulation
	if mod.FrameMs != 25 {
		t.Errorf("modulation.frameMs = %d, want 25", mod.FrameMs)
	}
	if mod.Centroid.Confidence < 0 || mod.Centroid.Confidence > 1 {
		t.Errorf("modulation.centroid.confidence = %.2f, want 0..1", mod.Centroid.Confidence)
	}
	t.Logf("centroid modulation: rate=%.2f Hz depth=%.0f regular=%v confidence=%.2f",
		mod.Centroid.RateHz, mod.Centroid.Depth, mod.Centroid.Regular, mod.Centroid.Confidence)
	t.Logf("rms modulation: rate=%.2f Hz depth=%.2f dB regular=%v confidence=%.2f",
		mod.Rms.RateHz, mod.Rms.Depth, mod.Rms.Regular, mod.Rms.Confidence)

	// With an LFO routed to the filter, the centroid must show a clean, periodic modulation at an LFO-band rate.
	if mod.Dominant != "centroid" {
		t.Fatalf("expected the filter LFO to dominate the centroid, got dominant=%q (is an LFO routed to the cutoff?)", mod.Dominant)
	}
	if !mod.Centroid.Regular {
		t.Fatalf("centroid modulation not flagged regular (rate=%.2f Hz, depth=%.0f Hz, conf=%.2f)", mod.Centroid.RateHz, mod.Centroid.Depth, mod.Centroid.Confidence)
	}
	if mod.Centroid.RateHz < 0.5 || mod.Centroid.RateHz > 15.0 {
		t.Fatalf("centroid LFO rate %.2f Hz is outside the plausible LFO band (0.5..15 Hz)", mod.Centroid.RateHz)
	}
}

// TestTuneModulationRateLive tunes an LFO-rate parameter toward a target using measure=modulation.centroid.rate_hz
// and asserts the tune converged. This is the "vibrato near 6 Hz" example from the spec: tune_param with a
// modulation measure, temporal auto-enabled. Gated on SIDECHAIN_LIVE_PARAM (the LFO-rate param id).
func TestTuneModulationRateLive(t *testing.T) {
	portStr := os.Getenv("SIDECHAIN_LIVE_PORT")
	catPath := os.Getenv("SIDECHAIN_LIVE_CATALOG")
	lfoParamID := os.Getenv("SIDECHAIN_LIVE_PARAM")
	if portStr == "" || catPath == "" || lfoParamID == "" {
		t.Skip("set SIDECHAIN_LIVE_PORT, SIDECHAIN_LIVE_CATALOG, SIDECHAIN_LIVE_PARAM (LFO rate param) to run the modulation tune live test")
	}
	target := 4.0 // Hz; a reasonable LFO rate to aim for
	if ts := os.Getenv("SIDECHAIN_LIVE_LFO_TARGET"); ts != "" {
		if v, e := strconv.ParseFloat(ts, 64); e == nil {
			target = v
		}
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("bad SIDECHAIN_LIVE_PORT %q: %v", portStr, err)
	}
	data, err := os.ReadFile(catPath)
	if err != nil {
		t.Fatalf("read catalog %q: %v", catPath, err)
	}
	cat, err := loadCatalogJSON(data)
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	s := newSession(cat)
	ctx := context.Background()
	cres, _, err := s.handleConnectLive(ctx, nil, connectLiveIn{Host: "127.0.0.1", Port: port})
	if err != nil {
		t.Fatalf("connect_live: %v", err)
	}
	if !strings.Contains(textOf(cres), "Connected LIVE") {
		t.Fatalf("connect_live: %s", textOf(cres))
	}
	defer s.handleDisconnectLive(ctx, nil, emptyIn{})

	// A long gate so each render resolves the LFO rate well. Temporal is auto-enabled by the modulation measure.
	res, out, err := s.handleTuneParam(ctx, nil, tuneParamIn{
		ID: lfoParamID, Measure: "modulation.centroid.rate_hz", Goal: "target", Target: target,
		Note: 60, Velocity: 0.9, GateMs: 3000, DurationMs: 3500,
	})
	if err != nil {
		t.Fatalf("tune_param modulation: %v", err)
	}
	rr, ok := out.(tuneResult)
	if !ok {
		t.Fatalf("tune_param returned no result: %s", textOf(res))
	}
	t.Logf("modulation rate tune: %s", rr.Summary)
	if rr.Measurement.Modulation == nil {
		t.Fatalf("final Measurement.Modulation is nil; temporal should have been auto-enabled")
	}
	// The tune should land the measured LFO rate near the target. The rate estimate is quantized (fsEnv / integer
	// lag), so allow a generous tolerance; the point is that it converged toward the target, not exact equality.
	if math.Abs(rr.BestValue-target) > 1.5 {
		t.Fatalf("tune to %.1f Hz LFO rate landed at %.2f Hz (best normalized %.3f); expected within 1.5 Hz", target, rr.BestValue, rr.BestNormalized)
	}
}
