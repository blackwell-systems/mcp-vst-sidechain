// tune_live_test.go - GATED real-host test for the tune_param optimizer: the Phase 4 make-it-brighter loop run
// AUTONOMOUSLY. It needs a running JUCE host with a plugin whose filter is in the signal path (CI points it at
// TAL-NoiseMaker's Filter Cutoff), so it SKIPS cleanly when its env is unset and the normal `go test` stays green.

package sidechain

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestTuneBrighterLive drives tune_param to MAXIMIZE the spectral centroid by searching the filter cutoff, and
// asserts it left the plugin brighter than it started (and near the top of the range). This is the closed loop from
// TestRenderBrighter, but the tool converges it instead of the test hard-coding low/high. Gated on SIDECHAIN_LIVE_*
// (SIDECHAIN_LIVE_PARAM = the filter cutoff id).
func TestTuneBrighterLive(t *testing.T) {
	portStr := os.Getenv("SIDECHAIN_LIVE_PORT")
	catPath := os.Getenv("SIDECHAIN_LIVE_CATALOG")
	cutoffID := os.Getenv("SIDECHAIN_LIVE_PARAM")
	if portStr == "" || catPath == "" || cutoffID == "" {
		t.Skip("set SIDECHAIN_LIVE_PORT, SIDECHAIN_LIVE_CATALOG, SIDECHAIN_LIVE_PARAM (the filter cutoff id) to run the autonomous make-it-brighter tune")
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

	// Start dark so there is real headroom to climb, then let the tool find the bright end.
	lo := 0.1
	if _, _, err := s.handleSetParam(ctx, nil, setParamIn{ID: cutoffID, Normalized: &lo}); err != nil {
		t.Fatalf("seed cutoff low: %v", err)
	}

	res, out, err := s.handleTuneParam(ctx, nil, tuneParamIn{
		ID: cutoffID, Measure: "centroid_hz", Goal: "maximize",
		Note: 60, Velocity: 0.9, GateMs: 800, DurationMs: 1500,
	})
	if err != nil {
		t.Fatalf("tune_param: %v", err)
	}
	rr, ok := out.(tuneResult)
	if !ok {
		t.Fatalf("tune_param returned no result: %s", textOf(res))
	}
	t.Logf("autonomous brighter: %s", rr.Summary)
	if !(rr.BestValue > rr.StartValue) {
		t.Fatalf("tune did not get brighter: start %.0f Hz, best %.0f Hz", rr.StartValue, rr.BestValue)
	}
	if rr.BestNormalized < 0.5 {
		t.Fatalf("maximizing the centroid should open the cutoff well past halfway, landed at normalized %.3f", rr.BestNormalized)
	}
}
