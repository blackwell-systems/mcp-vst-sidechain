// render_tools_test.go - in-memory coverage for the render_and_measure tool. Drives it through a session
// connected to the fake host (which answers the `render` verb with a canned deterministic measurement, since it
// has no DSP), asserting the measurement parses off the wire and the one-line summary formats. Pure Go, no gate
// env; audio correctness lives only in the gated real-host tests (render_live_test.go / the brighter E2E).

package sidechain

import (
	"context"
	"strings"
	"testing"
)

func TestRenderAndMeasure(t *testing.T) {
	fh := startFakeHost(t)
	defer fh.stop()
	s := newLiveTestSession()
	ctx := context.Background()
	if _, _, err := s.handleConnectLive(ctx, nil, connectLiveIn{Host: "127.0.0.1", Port: fh.port()}); err != nil {
		t.Fatalf("connect_live: %v", err)
	}

	// The fake's centroid responds to the "cutoff" param (200 + norm*3800 Hz), so set a clean position to get a
	// deterministic 2100 Hz (200 + 0.5*3800). The other measurement fields are canned constants.
	half := 0.5
	if _, _, err := s.handleSetParam(ctx, nil, setParamIn{ID: "cutoff", Normalized: &half}); err != nil {
		t.Fatalf("set cutoff: %v", err)
	}

	res, out, err := s.handleRenderAndMeasure(ctx, nil, renderAndMeasureIn{Note: 60, Velocity: 0.8, GateMs: 500, DurationMs: 2000})
	if err != nil {
		t.Fatalf("render_and_measure: %v", err)
	}

	// The fake bumped its render counter (the verb reached it).
	fh.mu.Lock()
	renders := fh.renders
	fh.mu.Unlock()
	if renders != 1 {
		t.Fatalf("fake host render count = %d, want 1", renders)
	}

	// Structured output carries the parsed measurement (matching the fake's canned values).
	rr, ok := out.(renderResult)
	if !ok {
		t.Fatalf("structured output not a renderResult: %T", out)
	}
	m := rr.Measurement
	if m.SampleRate != 48000 || m.Channels != 2 {
		t.Fatalf("measurement header wrong: sr=%v ch=%d", m.SampleRate, m.Channels)
	}
	if m.PeakDb != -6.2 || m.RmsDb != -18.4 || m.Crest != 12.2 || m.CentroidHz != 2100 {
		t.Fatalf("measurement values did not parse: %+v", m)
	}
	if m.Bands.LowDb != -20.1 || m.Bands.MidDb != -16.8 || m.Bands.HighDb != -28.0 {
		t.Fatalf("bands did not parse: %+v", m.Bands)
	}
	if m.Silent || m.Clipped {
		t.Fatalf("silent/clipped flags wrong: silent=%v clipped=%v", m.Silent, m.Clipped)
	}

	// The text reply is the one-line human summary, and matches the structured Summary.
	txt := textOf(res)
	if txt != rr.Summary {
		t.Fatalf("text reply %q != structured summary %q", txt, rr.Summary)
	}
	for _, want := range []string{"peak -6.2 dBFS", "RMS -18.4 dB", "centroid 2.10 kHz", "not clipped"} {
		if !strings.Contains(txt, want) {
			t.Fatalf("summary %q missing %q", txt, want)
		}
	}

	// Non-temporal render leaves Modulation nil (the fake omits the modulation block when temporal is false).
	if m.Modulation != nil {
		t.Fatalf("non-temporal render should leave Modulation nil, got %+v", m.Modulation)
	}

	// Guard: the tool requires a live connection.
	if _, _, err := s.handleDisconnectLive(ctx, nil, emptyIn{}); err != nil {
		t.Fatalf("disconnect_live: %v", err)
	}
	if r, _, _ := s.handleRenderAndMeasure(ctx, nil, renderAndMeasureIn{Note: 60}); !strings.Contains(textOf(r), "not live") {
		t.Fatalf("render_and_measure offline = %q, want not-live guard", textOf(r))
	}
}

// TestRenderAndMeasureTemporal exercises the Tier 2.5 temporal path: requesting Temporal=true causes the fake host
// to return a modulation block, which parseMeasurement decodes into Measurement.Modulation. The fake's rate is
// 1 + cutoff*9 Hz, so at cutoff=0.5 we expect rate ~ 5.5 Hz; depth = cutoff*2000 = 1000 Hz.
func TestRenderAndMeasureTemporal(t *testing.T) {
	fh := startFakeHost(t)
	defer fh.stop()
	s := newLiveTestSession()
	ctx := context.Background()
	if _, _, err := s.handleConnectLive(ctx, nil, connectLiveIn{Host: "127.0.0.1", Port: fh.port()}); err != nil {
		t.Fatalf("connect_live: %v", err)
	}

	// Fix the cutoff at 0.5 for deterministic modulation values.
	half := 0.5
	if _, _, err := s.handleSetParam(ctx, nil, setParamIn{ID: "cutoff", Normalized: &half}); err != nil {
		t.Fatalf("set cutoff: %v", err)
	}

	res, out, err := s.handleRenderAndMeasure(ctx, nil, renderAndMeasureIn{Note: 60, Temporal: true, FrameMs: 25})
	if err != nil {
		t.Fatalf("render_and_measure temporal: %v", err)
	}
	rr, ok := out.(renderResult)
	if !ok {
		t.Fatalf("structured output not a renderResult: %T", out)
	}

	// Modulation block must be decoded.
	m := rr.Measurement
	if m.Modulation == nil {
		t.Fatalf("temporal render: Measurement.Modulation is nil, want a block")
	}
	mod := m.Modulation
	if mod.FrameMs != 25 {
		t.Fatalf("modulation.frameMs = %d, want 25", mod.FrameMs)
	}
	// centroid rate at cutoff=0.5: 1 + 0.5*9 = 5.5 Hz
	if mod.Centroid.RateHz != 5.5 {
		t.Fatalf("modulation.centroid.rateHz = %.2f, want 5.5", mod.Centroid.RateHz)
	}
	// centroid depth at cutoff=0.5: 0.5*2000 = 1000 Hz
	if mod.Centroid.Depth != 1000 {
		t.Fatalf("modulation.centroid.depth = %.0f, want 1000", mod.Centroid.Depth)
	}
	if !mod.Centroid.Regular {
		t.Fatalf("modulation.centroid.regular should be true")
	}
	if mod.Centroid.Confidence != 0.9 {
		t.Fatalf("modulation.centroid.confidence = %.2f, want 0.9", mod.Centroid.Confidence)
	}
	if mod.Dominant != "centroid" {
		t.Fatalf("modulation.dominant = %q, want centroid", mod.Dominant)
	}
	// RMS block: low confidence, irregular.
	if mod.Rms.Regular {
		t.Fatalf("modulation.rms.regular should be false (irregular)")
	}
	if mod.Rms.Confidence != 0.1 {
		t.Fatalf("modulation.rms.confidence = %.2f, want 0.1", mod.Rms.Confidence)
	}

	// Summary should mention the LFO phrase (dominant=centroid, rate 5.5 Hz, depth 1000 Hz).
	txt := textOf(res)
	for _, want := range []string{"LFO", "5.5 Hz", "centroid", "1000"} {
		if !strings.Contains(txt, want) {
			t.Fatalf("temporal summary %q missing %q", txt, want)
		}
	}

	// A non-temporal render against the same session must NOT return a modulation block.
	res2, out2, err := s.handleRenderAndMeasure(ctx, nil, renderAndMeasureIn{Note: 60})
	if err != nil {
		t.Fatalf("non-temporal render: %v", err)
	}
	rr2, ok := out2.(renderResult)
	if !ok {
		t.Fatalf("non-temporal structured output not a renderResult: %T", out2)
	}
	if rr2.Measurement.Modulation != nil {
		t.Fatalf("non-temporal render returned a Modulation block: %+v", rr2.Measurement.Modulation)
	}
	if strings.Contains(textOf(res2), "LFO") {
		t.Fatalf("non-temporal summary should not mention LFO, got %q", textOf(res2))
	}
}
