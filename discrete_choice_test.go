// discrete_choice_test.go - coverage for setting a discrete-hiding-as-float param by label (set_param choice=
// on a param the catalog types as a plain float). The fake host's "filterType" id renders LP/BP/HP across
// thirds of the range and "toggle" renders Off/On; these stand in for a plugin that exposes an enum/bool as a
// float. No real plugin.

package sidechain

import (
	"context"
	"strings"
	"testing"
)

// TestSetDiscreteChoiceByLabel: describe reports discrete + labels, set_param choice="HP" lands in the HP band
// and the plugin's readback text is "HP", and an unknown label returns the observed-labels message.
func TestSetDiscreteChoiceByLabel(t *testing.T) {
	fh := startFakeHost(t)
	defer fh.stop()
	// filterType is type float in the catalog, but its value text renders LP/BP/HP: a discrete-as-float control.
	s := newSession(NewCatalog([]ParamDef{{ID: "filterType", Label: "Filter Type", Type: "float", Min: 0, Max: 1}}))
	ctx := context.Background()
	if _, _, err := s.handleConnectLive(ctx, nil, connectLiveIn{Host: "127.0.0.1", Port: fh.port()}); err != nil {
		t.Fatalf("connect_live: %v", err)
	}

	// describe_param reports it as discrete with the observed labels.
	dres, dout, err := s.handleDescribeParam(ctx, nil, describeParamIn{ID: "filterType"})
	if err != nil {
		t.Fatalf("describe_param: %v", err)
	}
	if !strings.Contains(textOf(dres), "discrete") {
		t.Fatalf("describe summary = %q, want discrete", textOf(dres))
	}
	d, _ := dout.(struct {
		ID        string         `json:"id"`
		Label     string         `json:"label"`
		Inference ParamInference `json:"inference"`
		Samples   []ValueSample  `json:"samples"`
	})
	if d.Inference.Numeric {
		t.Fatalf("filterType should probe as discrete (Numeric=false), got %+v", d.Inference)
	}
	for _, want := range []string{"LP", "BP", "HP"} {
		if !containsStr(d.Inference.Labels, want) {
			t.Fatalf("labels = %v, want to include %q", d.Inference.Labels, want)
		}
	}

	// set_param choice="HP" lands the param in the HP band (norm >= 2/3) and the plugin confirms with "HP".
	sres, _, err := s.handleSetParam(ctx, nil, setParamIn{ID: "filterType", Choice: "HP"})
	if err != nil {
		t.Fatalf("set_param choice: %v", err)
	}
	if !strings.Contains(textOf(sres), "confirms") {
		t.Fatalf("set choice reply = %q, want the plugin to confirm the HP readback", textOf(sres))
	}
	fh.mu.Lock()
	landed := fh.params["filterType"]
	fh.mu.Unlock()
	if landed < 2.0/3.0 {
		t.Fatalf("choice HP landed at norm %.4f, want the HP band (>= 0.667)", landed)
	}
	if renderFor("filterType", landed) != "HP" {
		t.Fatalf("norm %.4f renders %q, want HP", landed, renderFor("filterType", landed))
	}

	// Case-insensitive: "lp" resolves to LP and lands in the low band.
	if _, _, err := s.handleSetParam(ctx, nil, setParamIn{ID: "filterType", Choice: "lp"}); err != nil {
		t.Fatalf("set_param choice lp: %v", err)
	}
	fh.mu.Lock()
	landedLP := fh.params["filterType"]
	fh.mu.Unlock()
	if renderFor("filterType", landedLP) != "LP" {
		t.Fatalf("choice lp landed norm %.4f -> %q, want LP", landedLP, renderFor("filterType", landedLP))
	}

	// An unknown label returns the observed-labels message.
	ures, _, err := s.handleSetParam(ctx, nil, setParamIn{ID: "filterType", Choice: "Notch"})
	if err != nil {
		t.Fatalf("set_param unknown choice: %v", err)
	}
	if !strings.Contains(textOf(ures), "not an observed label") || !strings.Contains(textOf(ures), "LP") {
		t.Fatalf("unknown label reply = %q, want the observed-labels message", textOf(ures))
	}
}

// TestSetDiscreteChoiceToggle covers the simplest discrete-as-float case (an On/Off toggle exposed as a float).
func TestSetDiscreteChoiceToggle(t *testing.T) {
	fh := startFakeHost(t)
	defer fh.stop()
	s := newSession(NewCatalog([]ParamDef{{ID: "toggle", Label: "Sync", Type: "float", Min: 0, Max: 1}}))
	ctx := context.Background()
	if _, _, err := s.handleConnectLive(ctx, nil, connectLiveIn{Host: "127.0.0.1", Port: fh.port()}); err != nil {
		t.Fatalf("connect_live: %v", err)
	}
	res, _, err := s.handleSetParam(ctx, nil, setParamIn{ID: "toggle", Choice: "On"})
	if err != nil {
		t.Fatalf("set_param choice On: %v", err)
	}
	if !strings.Contains(textOf(res), "confirms") {
		t.Fatalf("toggle On reply = %q, want confirms", textOf(res))
	}
	fh.mu.Lock()
	landed := fh.params["toggle"]
	fh.mu.Unlock()
	if renderFor("toggle", landed) != "On" {
		t.Fatalf("choice On landed norm %.4f -> %q, want On", landed, renderFor("toggle", landed))
	}
}

// TestSetDiscreteChoiceNotLive: choice on a non-catalog-choice param when headless is rejected with a
// needs-live hint (it must probe the plugin's value text to map the label).
func TestSetDiscreteChoiceNotLive(t *testing.T) {
	s := newSession(NewCatalog([]ParamDef{{ID: "filterType", Label: "Filter Type", Type: "float", Min: 0, Max: 1}}))
	ctx := context.Background()
	res, _, _ := s.handleSetParam(ctx, nil, setParamIn{ID: "filterType", Choice: "HP"})
	if !strings.Contains(textOf(res), "needs a live plugin") {
		t.Fatalf("headless discrete choice = %q, want needs-live", textOf(res))
	}
}

// TestSetDiscreteChoiceNotDiscrete: choice on a live param that probes NUMERIC (a real unit-bearing float) is
// rejected with a does-not-apply message, not silently misapplied.
func TestSetDiscreteChoiceNotDiscrete(t *testing.T) {
	fh := startFakeHost(t)
	defer fh.stop()
	// "cutoff" renders "20.00 Hz".."20000.00 Hz": numeric, so choice= does not apply.
	s := newLiveTestSession()
	ctx := context.Background()
	if _, _, err := s.handleConnectLive(ctx, nil, connectLiveIn{Host: "127.0.0.1", Port: fh.port()}); err != nil {
		t.Fatalf("connect_live: %v", err)
	}
	res, _, _ := s.handleSetParam(ctx, nil, setParamIn{ID: "cutoff", Choice: "HP"})
	if !strings.Contains(textOf(res), "does not probe as a discrete control") {
		t.Fatalf("numeric-param choice = %q, want does-not-apply", textOf(res))
	}
}

// TestGetParamSurfacesDiscreteLabels: once a discrete-as-float param has been probed (its inference cached),
// get_param appends the observed labels so the agent knows choice= applies.
func TestGetParamSurfacesDiscreteLabels(t *testing.T) {
	fh := startFakeHost(t)
	defer fh.stop()
	s := newSession(NewCatalog([]ParamDef{{ID: "filterType", Label: "Filter Type", Type: "float", Min: 0, Max: 1}}))
	ctx := context.Background()
	if _, _, err := s.handleConnectLive(ctx, nil, connectLiveIn{Host: "127.0.0.1", Port: fh.port()}); err != nil {
		t.Fatalf("connect_live: %v", err)
	}

	// Before any probe, get_param does not surface labels (it must not probe every param itself).
	pre, _, err := s.handleGetParam(ctx, nil, getParamIn{ID: "filterType"})
	if err != nil {
		t.Fatalf("get_param (pre-probe): %v", err)
	}
	if strings.Contains(textOf(pre), "labels:") {
		t.Fatalf("get_param surfaced labels before probing: %q", textOf(pre))
	}

	// Probe once (describe_param caches the inference).
	if _, _, err := s.handleDescribeParam(ctx, nil, describeParamIn{ID: "filterType"}); err != nil {
		t.Fatalf("describe_param: %v", err)
	}

	// Now get_param appends the observed labels.
	post, _, err := s.handleGetParam(ctx, nil, getParamIn{ID: "filterType"})
	if err != nil {
		t.Fatalf("get_param (post-probe): %v", err)
	}
	txt := textOf(post)
	if !strings.Contains(txt, "discrete") || !strings.Contains(txt, "HP") || !strings.Contains(txt, "choice=") {
		t.Fatalf("get_param post-probe = %q, want the discrete labels + choice= hint", txt)
	}
}
