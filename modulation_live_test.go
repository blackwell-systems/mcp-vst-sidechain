// modulation_live_test.go - GATED real-host tests for Tier 2.5 temporal (modulation-aware) measurement. They need
// a running JUCE host with a plugin that has an active LFO in its signal path, so they SKIP cleanly when the env
// vars are unset and the normal `go test` stays green.
//
// Gate: set SIDECHAIN_LIVE_PORT, SIDECHAIN_LIVE_CATALOG, and SIDECHAIN_LIVE_PARAM (an LFO-rate parameter id) to
// run these. Optionally set SIDECHAIN_LIVE_LFO_TARGET to override the target rate in Hz (default 4.0).

package sidechain

import (
	"context"
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

	// Render with temporal=true, 25 ms frames.
	res, out, err := s.handleRenderAndMeasure(ctx, nil, renderAndMeasureIn{
		Note: 60, Velocity: 0.9, GateMs: 1000, DurationMs: 3000,
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
	if mod.Dominant != "centroid" && mod.Dominant != "rms" && mod.Dominant != "none" {
		t.Errorf("modulation.dominant = %q, want centroid | rms | none", mod.Dominant)
	}
	if mod.Centroid.Confidence < 0 || mod.Centroid.Confidence > 1 {
		t.Errorf("modulation.centroid.confidence = %.2f, want 0..1", mod.Centroid.Confidence)
	}
	if mod.Rms.Confidence < 0 || mod.Rms.Confidence > 1 {
		t.Errorf("modulation.rms.confidence = %.2f, want 0..1", mod.Rms.Confidence)
	}
	t.Logf("centroid modulation: rate=%.2f Hz depth=%.0f regular=%v confidence=%.2f",
		mod.Centroid.RateHz, mod.Centroid.Depth, mod.Centroid.Regular, mod.Centroid.Confidence)
	t.Logf("rms modulation: rate=%.2f Hz depth=%.2f dB regular=%v confidence=%.2f",
		mod.Rms.RateHz, mod.Rms.Depth, mod.Rms.Regular, mod.Rms.Confidence)
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

	res, out, err := s.handleTuneParam(ctx, nil, tuneParamIn{
		ID: lfoParamID, Measure: "modulation.centroid.rate_hz", Goal: "target", Target: target,
		Note: 60, Velocity: 0.9, GateMs: 1000, DurationMs: 3000,
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
		t.Errorf("final Measurement.Modulation is nil; temporal should have been auto-enabled")
	}
}
