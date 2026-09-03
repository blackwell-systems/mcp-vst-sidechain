// pitch_live_test.go - GATED real-host tests for the pitch (f0) modulation signal added in the phrase-render
// upgrade. They need a running JUCE host with a plugin that has an active pitch LFO (vibrato) in its signal path,
// so they SKIP cleanly when the env vars are unset and the normal `go test` stays green.
//
// Gate: set SIDECHAIN_LIVE_PORT and SIDECHAIN_LIVE_CATALOG to run these. The harness must arrange LFO -> pitch
// routing on the plugin before running (e.g. TAL-NoiseMaker vibrato depth > 0, LFO 1 -> pitch, rate set).
//
// Optionally set SIDECHAIN_LIVE_PITCH_PARAM (a vibrato/LFO-rate param id) to also run TestTunePitchRateLive.

package sidechain

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestRenderPitchBlockLive renders with Temporal=true and asserts that Measurement.Modulation.Pitch comes back
// with sane fields: present, confidence in 0..1, depth non-negative. Gated on SIDECHAIN_LIVE_PORT + CATALOG;
// the LFO -> pitch routing is expected to be arranged by the harness.
func TestRenderPitchBlockLive(t *testing.T) {
	portStr := os.Getenv("SIDECHAIN_LIVE_PORT")
	catPath := os.Getenv("SIDECHAIN_LIVE_CATALOG")
	if portStr == "" || catPath == "" {
		t.Skip("set SIDECHAIN_LIVE_PORT and SIDECHAIN_LIVE_CATALOG to run the pitch live test")
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

	// Render with temporal analysis on; a long gate gives the sustain window several vibrato cycles to resolve.
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
	t.Logf("temporal render (pitch): %s", rr.Summary)

	m := rr.Measurement
	if m.Modulation == nil {
		t.Fatalf("temporal render: Measurement.Modulation is nil; host may not support Tier 2.5")
	}
	mod := m.Modulation
	t.Logf("pitch modulation: rate=%.2f Hz depth=%.3f st regular=%v confidence=%.2f",
		mod.Pitch.RateHz, mod.Pitch.Depth, mod.Pitch.Regular, mod.Pitch.Confidence)
	t.Logf("centroid modulation: rate=%.2f Hz depth=%.0f Hz regular=%v confidence=%.2f",
		mod.Centroid.RateHz, mod.Centroid.Depth, mod.Centroid.Regular, mod.Centroid.Confidence)

	// Sanity assertions: the pitch block must be present with sane field values.
	if mod.Pitch.Confidence < 0 || mod.Pitch.Confidence > 1 {
		t.Errorf("modulation.pitch.confidence = %.2f, want 0..1", mod.Pitch.Confidence)
	}
	if mod.Pitch.Depth < 0 {
		t.Errorf("modulation.pitch.depth = %.3f st, want >= 0", mod.Pitch.Depth)
	}
	if mod.Pitch.RateHz < 0 {
		t.Errorf("modulation.pitch.rate_hz = %.2f, want >= 0", mod.Pitch.RateHz)
	}
	// With an LFO routed to pitch, expect a clean vibrato: regular, within LFO-band rate, non-zero depth.
	if !mod.Pitch.Regular {
		t.Logf("warning: pitch modulation not flagged regular (rate=%.2f Hz, depth=%.3f st, conf=%.2f); is a vibrato LFO active?",
			mod.Pitch.RateHz, mod.Pitch.Depth, mod.Pitch.Confidence)
	}
	if mod.Pitch.RateHz > 0 && (mod.Pitch.RateHz < 0.1 || mod.Pitch.RateHz > 20.0) {
		t.Logf("warning: pitch rate %.2f Hz is outside the typical vibrato band (0.1..20 Hz)", mod.Pitch.RateHz)
	}
}
