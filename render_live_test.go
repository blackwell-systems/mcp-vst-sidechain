// render_live_test.go - GATED real-host tests for the render + analysis path. They need a running JUCE host with
// a plugin loaded (the fake host has no DSP), so they SKIP cleanly when their env is unset and the normal
// `go test` stays green.
//
// TestRenderSmoke (generic, SIDECHAIN_SWEEP_* gate, wired into drive_plugin.sh so it runs per plugin): renders a
// note and asserts a measurement came back and is not an error. It does NOT assert specific dB/centroid values -
// those are plugin-specific; the point is that the render path is drivable end to end against any hosted plugin.
//
// TestRenderBrighter (the payoff, SIDECHAIN_LIVE_* gate): the canonical make-it-brighter proof. Set the filter
// cutoff LOW, render a held note, record the spectral centroid; set the cutoff HIGH, render again; assert the
// centroid INCREASED. This is the objective, end-to-end demonstration that "brighter" is measurable. Point
// SIDECHAIN_LIVE_PARAM at a param that actually opens the spectrum: CI uses TAL-NoiseMaker's Filter Cutoff, whose
// init patch has an ACTIVE lowpass (Surge XT's init-patch filter is Off, so its cutoff is inaudible unmodified).

package sidechain

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestRenderSmoke drives render_and_measure against whatever plugin the generic sweep gate points at. It renders
// a middle-C note (the host auto-detects instrument vs effect and applies its own defaults for an effect, so a
// bare render works for either) and asserts a measurement returned without error. Values are plugin-specific and
// deliberately NOT asserted.
func TestRenderSmoke(t *testing.T) {
	port, cat := sweepEnv(t, false)
	s := newSession(cat)
	ctx, cleanup := connectSwept(t, s, port)
	defer cleanup()

	res, out, err := s.handleRenderAndMeasure(ctx, nil, renderAndMeasureIn{Note: 60, Velocity: 0.8, GateMs: 500, DurationMs: 2000})
	if err != nil {
		t.Fatalf("render_and_measure: %v", err)
	}
	rr, ok := out.(renderResult)
	if !ok {
		t.Fatalf("render_and_measure returned no measurement (reply: %s)", textOf(res))
	}
	m := rr.Measurement
	if m.SampleRate <= 0 || m.DurationSec <= 0 {
		t.Fatalf("render produced a degenerate measurement (sr=%v dur=%v): %s", m.SampleRate, m.DurationSec, textOf(res))
	}
	t.Logf("render smoke OK: %s (sr=%.0f, %d ch, %.2fs)", rr.Summary, m.SampleRate, m.Channels, m.DurationSec)
}

// TestRenderBrighter is the canonical make-it-brighter loop, end to end on a real plugin. Gated on the
// SIDECHAIN_LIVE_* Surge-capability env (SIDECHAIN_LIVE_PORT/CATALOG plus SIDECHAIN_LIVE_PARAM = the filter cutoff
// id). It renders the SAME held note twice - once with the cutoff low, once high - and asserts the spectral
// centroid rose. It sets the cutoff via a NORMALIZED value (~0.2 / ~0.9), which is plugin-agnostic: no dependence
// on the cutoff's real unit or curve, only that up-the-normalized-range opens the filter.
func TestRenderBrighter(t *testing.T) {
	portStr := os.Getenv("SIDECHAIN_LIVE_PORT")
	catPath := os.Getenv("SIDECHAIN_LIVE_CATALOG")
	cutoffID := os.Getenv("SIDECHAIN_LIVE_PARAM")
	if portStr == "" || catPath == "" || cutoffID == "" {
		t.Skip("set SIDECHAIN_LIVE_PORT, SIDECHAIN_LIVE_CATALOG, SIDECHAIN_LIVE_PARAM (the filter cutoff id) to run the make-it-brighter E2E")
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
	// The C++ host serves ONE client at a time; always disconnect so a later gated test can connect.
	defer s.handleDisconnectLive(ctx, nil, emptyIn{})

	// renderCentroid sets the cutoff to a normalized position, renders a held note, and returns the measured
	// spectral centroid. A fixed note + gate keeps the two renders comparable (only the cutoff differs).
	renderCentroid := func(norm float64) float64 {
		t.Helper()
		if _, _, err := s.handleSetParam(ctx, nil, setParamIn{ID: cutoffID, Normalized: &norm}); err != nil {
			t.Fatalf("set cutoff to %.2f: %v", norm, err)
		}
		res, out, err := s.handleRenderAndMeasure(ctx, nil, renderAndMeasureIn{Note: 60, Velocity: 0.9, GateMs: 800, DurationMs: 1500})
		if err != nil {
			t.Fatalf("render at cutoff %.2f: %v", norm, err)
		}
		rr, ok := out.(renderResult)
		if !ok {
			t.Fatalf("render at cutoff %.2f returned no measurement: %s", norm, textOf(res))
		}
		return rr.Measurement.CentroidHz
	}

	lowNorm, highNorm := 0.2, 0.9
	lowCentroid := renderCentroid(lowNorm)
	highCentroid := renderCentroid(highNorm)
	t.Logf("make-it-brighter: cutoff %.2f -> centroid %.1f Hz; cutoff %.2f -> centroid %.1f Hz",
		lowNorm, lowCentroid, highNorm, highCentroid)
	if !(highCentroid > lowCentroid) {
		t.Fatalf("opening the cutoff did not raise the spectral centroid: low(%.2f)=%.1f Hz, high(%.2f)=%.1f Hz "+
			"(expected the high-cutoff render to be brighter)", lowNorm, lowCentroid, highNorm, highCentroid)
	}
}
