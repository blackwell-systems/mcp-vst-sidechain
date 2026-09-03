// tune_params_live_test.go - GATED real-host test for tune_params: the "wobble" compositional intent on a real LFO.
// Co-optimize TWO coupled knobs - the LFO RATE toward a target rate, and the LFO AMOUNT toward MORE modulation
// depth - by coordinate descent over the temporal render loop. Needs a running host with an LFO routed to the
// filter (the CI step arranges that on TAL). SKIPS cleanly when the env is unset.
//
// Gate: SIDECHAIN_LIVE_PORT, SIDECHAIN_LIVE_CATALOG, SIDECHAIN_LIVE_PARAM (LFO rate id), SIDECHAIN_LIVE_PARAM2
// (LFO amount id). Optional SIDECHAIN_LIVE_LFO_TARGET (rate in Hz, default 4).

package sidechain

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestTuneParamsWobbleLive(t *testing.T) {
	portStr := os.Getenv("SIDECHAIN_LIVE_PORT")
	catPath := os.Getenv("SIDECHAIN_LIVE_CATALOG")
	rateID := os.Getenv("SIDECHAIN_LIVE_PARAM")
	amountID := os.Getenv("SIDECHAIN_LIVE_PARAM2")
	if portStr == "" || catPath == "" || rateID == "" || amountID == "" {
		t.Skip("set SIDECHAIN_LIVE_PORT, SIDECHAIN_LIVE_CATALOG, SIDECHAIN_LIVE_PARAM (LFO rate), SIDECHAIN_LIVE_PARAM2 (LFO amount) to run the wobble co-tune")
	}
	target := 4.0
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

	// Start the amount low so "maximize depth" has real headroom, then co-tune rate (-> target) and amount (-> more
	// centroid modulation depth). Coordinate descent handles the coupling (amount changes the measured depth, and a
	// little the rate estimate).
	lowAmt := 0.2
	if _, _, err := s.handleSetParam(ctx, nil, setParamIn{ID: amountID, Normalized: &lowAmt}); err != nil {
		t.Fatalf("seed amount low: %v", err)
	}

	res, out, err := s.handleTuneParams(ctx, nil, tuneParamsIn{
		Note: 60, Velocity: 0.9, GateMs: 3000, DurationMs: 3500,
		Knobs: []tuneKnob{
			{ID: rateID, Measure: "modulation.centroid.rate_hz", Goal: "target", Target: target},
			{ID: amountID, Measure: "modulation.centroid.depth", Goal: "maximize"},
		},
	})
	if err != nil {
		t.Fatalf("tune_params wobble: %v", err)
	}
	rr, ok := out.(tuneParamsResult)
	if !ok {
		t.Fatalf("tune_params returned no result: %s", textOf(res))
	}
	t.Logf("wobble co-tune: %s", rr.Summary)

	rate, depth := rr.Knobs[0], rr.Knobs[1]
	// The rate landed near the target.
	if v := rate.BestValue; v < target-1.5 || v > target+1.5 {
		t.Fatalf("LFO rate landed at %.2f Hz, want within 1.5 Hz of %.1f", v, target)
	}
	// Maximizing the amount deepened the modulation vs the low starting point.
	if depth.BestValue <= depth.StartValue {
		t.Fatalf("maximizing the LFO amount did not deepen the modulation: start %.0f Hz, best %.0f Hz", depth.StartValue, depth.BestValue)
	}
}
