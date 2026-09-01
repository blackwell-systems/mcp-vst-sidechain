// e2e_live_test.go - real-plugin E2E for the Phase-1 features that the cutoff test does not cover: setting a
// discrete-hiding-as-float param by label, and real-unit control on a steep zero-based time curve (which the
// analytic fits do not capture, so it drives the binary-search refinement fallback). Gated on env, driven by
// the integration workflow against real Surge XT; skipped otherwise.

package sidechain

import (
	"context"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
)

// parenText returns the text inside the first (...) pair, e.g. `LIVE x = 0.15 (Wavetable)` -> `Wavetable`.
// get_param (live) embeds the plugin's own value text there, which is the label for a discrete param and the
// real-unit reading for a numeric one.
func parenText(s string) string {
	i := strings.IndexByte(s, '(')
	if i < 0 {
		return ""
	}
	j := strings.IndexByte(s[i+1:], ')')
	if j < 0 {
		return ""
	}
	return strings.TrimSpace(s[i+1 : i+1+j])
}

func TestE2ESurgeExtras(t *testing.T) {
	port := os.Getenv("SIDECHAIN_LIVE_PORT")
	catPath := os.Getenv("SIDECHAIN_LIVE_CATALOG")
	discreteID := os.Getenv("SIDECHAIN_LIVE_DISCRETE")
	timeID := os.Getenv("SIDECHAIN_LIVE_TIME")
	if port == "" || catPath == "" || discreteID == "" || timeID == "" {
		t.Skip("set SIDECHAIN_LIVE_PORT/CATALOG/DISCRETE/TIME to run the extra real-plugin E2E")
	}
	data, err := os.ReadFile(catPath)
	if err != nil {
		t.Fatalf("read catalog: %v", err)
	}
	cat, err := loadCatalogJSON(data)
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	s := newSession(cat)
	ctx := context.Background()
	p, _ := strconv.Atoi(port)
	cres, _, err := s.handleConnectLive(ctx, nil, connectLiveIn{Host: "127.0.0.1", Port: p})
	if err != nil {
		t.Fatalf("connect_live: %v", err)
	}
	if !strings.Contains(textOf(cres), "Connected LIVE") {
		t.Fatalf("connect_live did not connect: %s", textOf(cres))
	}

	// --- discrete-hiding-as-float: probe reveals labels; set by a label; the plugin readback confirms it. ---
	if _, _, err := s.handleDescribeParam(ctx, nil, describeParamIn{ID: discreteID}); err != nil {
		t.Fatalf("describe discrete: %v", err)
	}
	dpi := s.infer[discreteID]
	if dpi.Numeric || len(dpi.Labels) < 2 {
		t.Fatalf("discrete param %s should probe as discrete with >=2 labels, got numeric=%v labels=%v", discreteID, dpi.Numeric, dpi.Labels)
	}
	target := dpi.Labels[1] // a non-endpoint label
	sres, _, err := s.handleSetParam(ctx, nil, setParamIn{ID: discreteID, Choice: target})
	if err != nil {
		t.Fatalf("set discrete by label: %v", err)
	}
	t.Logf("set choice=%q -> %s", target, textOf(sres))
	gres, _, err := s.handleGetParam(ctx, nil, getParamIn{ID: discreteID})
	if err != nil {
		t.Fatalf("get discrete: %v", err)
	}
	if got := parenText(textOf(gres)); !strings.EqualFold(got, target) {
		t.Fatalf("discrete set to %q did not stick; plugin now reads %q (full: %s)", target, got, textOf(gres))
	}

	// --- steep zero-based time curve: real-unit set that drives the refinement fallback. ---
	if _, _, err := s.handleDescribeParam(ctx, nil, describeParamIn{ID: timeID}); err != nil {
		t.Fatalf("describe time: %v", err)
	}
	tpi := s.infer[timeID]
	if !tpi.Numeric || tpi.Unit == "" {
		t.Fatalf("time param %s should probe as numeric with a unit, got %+v", timeID, tpi)
	}
	rng := math.Abs(tpi.RealMax - tpi.RealMin)
	want := tpi.RealMin + 0.30*rng
	tres, _, err := s.handleSetParam(ctx, nil, setParamIn{ID: timeID, Real: &want})
	if err != nil {
		t.Fatalf("set time real: %v", err)
	}
	if !strings.Contains(textOf(tres), "Set LIVE") {
		t.Fatalf("time real set did not drive the plugin: %s", textOf(tres))
	}
	t.Logf("set real=%.3f %s -> %s", want, tpi.Unit, textOf(tres))
	// Confirm via read-back: the plugin's value text, folded to the base unit, is near the target.
	gres2, _, err := s.handleGetParam(ctx, nil, getParamIn{ID: timeID})
	if err != nil {
		t.Fatalf("get time: %v", err)
	}
	gv, gu, ok := parseValueText(parenText(textOf(gres2)))
	if !ok {
		t.Fatalf("could not parse time read-back from %q", textOf(gres2))
	}
	got, _ := normalizeUnit(gv, gu)
	if rng > 0 && math.Abs(got-want)/rng > 0.20 {
		t.Fatalf("time real control off by >20%% of range: want %.3f %s, got %.3f (%s)", want, tpi.Unit, got, textOf(gres2))
	}
}
