// phrase_live_test.go - GATED real-host test for phrase rendering (chords / arps / sequences). Renders a C-major
// chord and asserts the measurement is non-degenerate and differs from a single note (the extra voices change the
// spectral content). Gated on SIDECHAIN_LIVE_PORT + SIDECHAIN_LIVE_CATALOG; SKIPS when unset.

package sidechain

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestRenderPhraseLive(t *testing.T) {
	portStr := os.Getenv("SIDECHAIN_LIVE_PORT")
	catPath := os.Getenv("SIDECHAIN_LIVE_CATALOG")
	if portStr == "" || catPath == "" {
		t.Skip("set SIDECHAIN_LIVE_PORT and SIDECHAIN_LIVE_CATALOG to run the phrase render test")
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

	meas := func(in renderAndMeasureIn) Measurement {
		t.Helper()
		_, out, err := s.handleRenderAndMeasure(ctx, nil, in)
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		return out.(renderResult).Measurement
	}

	one := meas(renderAndMeasureIn{Note: 60, Velocity: 0.9, GateMs: 1500, DurationMs: 2000})
	chord := meas(renderAndMeasureIn{DurationMs: 2000, Notes: []NoteEvent{
		{Note: 60, StartMs: 0, GateMs: 1500, Velocity: 0.9},
		{Note: 64, StartMs: 0, GateMs: 1500, Velocity: 0.9},
		{Note: 67, StartMs: 0, GateMs: 1500, Velocity: 0.9},
	}})
	t.Logf("single: rms=%.1f centroid=%.0f | C-major chord: rms=%.1f centroid=%.0f", one.RmsDb, one.CentroidHz, chord.RmsDb, chord.CentroidHz)

	if chord.SampleRate <= 0 || chord.Silent {
		t.Fatalf("chord render produced a degenerate/silent measurement: %+v", chord)
	}
	// The chord's extra voices add harmonic content the single note does not have, so the spectrum must differ.
	if chord.CentroidHz == one.CentroidHz {
		t.Fatalf("chord centroid (%.0f Hz) identical to single note; the phrase may not have reached the synth", chord.CentroidHz)
	}
}
