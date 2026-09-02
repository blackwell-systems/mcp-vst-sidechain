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
	if m.PeakDb != -6.2 || m.RmsDb != -18.4 || m.Crest != 12.2 || m.CentroidHz != 1840 {
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
	for _, want := range []string{"peak -6.2 dBFS", "RMS -18.4 dB", "centroid 1.84 kHz", "not clipped"} {
		if !strings.Contains(txt, want) {
			t.Fatalf("summary %q missing %q", txt, want)
		}
	}

	// Guard: the tool requires a live connection.
	if _, _, err := s.handleDisconnectLive(ctx, nil, emptyIn{}); err != nil {
		t.Fatalf("disconnect_live: %v", err)
	}
	if r, _, _ := s.handleRenderAndMeasure(ctx, nil, renderAndMeasureIn{Note: 60}); !strings.Contains(textOf(r), "not live") {
		t.Fatalf("render_and_measure offline = %q, want not-live guard", textOf(r))
	}
}
