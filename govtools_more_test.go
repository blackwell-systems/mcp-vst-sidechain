// govtools_more_test.go - pure-logic coverage for the concurrency surface that the socket-driven tests do not
// reach: parseGoverned wire decoding (nil / degenerate / full), govSummary rendering across the lease states
// (free / you / another client / multiple sections sorted), and the "not live" guards on all four gov tools.
// No socket: parseGoverned and govSummary are pure, and the guards return before any endpoint call.

package sidechain

import (
	"context"
	"strings"
	"testing"
)

func TestParseGovernedWire(t *testing.T) {
	// nil / non-map -> zero state with a non-nil (empty) section map.
	for _, v := range []any{nil, "not-a-map", 42} {
		g := parseGoverned(v)
		if g.InstanceLease != 0 || g.Generation != 0 || g.SectionLeases == nil || len(g.SectionLeases) != 0 {
			t.Fatalf("parseGoverned(%v) = %+v, want zero state with empty section map", v, g)
		}
	}

	// A full object decodes each field; non-float section holders are dropped.
	g := parseGoverned(map[string]any{
		"instance_lease": float64(3),
		"generation":     float64(7),
		"section_leases": map[string]any{"Filter": float64(3), "Amp": float64(5), "Bad": "x"},
	})
	if g.InstanceLease != 3 || g.Generation != 7 {
		t.Fatalf("scalar fields = %+v, want instance 3 / generation 7", g)
	}
	if g.SectionLeases["Filter"] != 3 || g.SectionLeases["Amp"] != 5 {
		t.Fatalf("section leases = %v, want Filter->3 Amp->5", g.SectionLeases)
	}
	if _, ok := g.SectionLeases["Bad"]; ok {
		t.Fatalf("a non-numeric holder should be dropped, got %v", g.SectionLeases)
	}

	// Wrong types on the scalar fields leave them at zero (no panic).
	g2 := parseGoverned(map[string]any{"instance_lease": "nope", "section_leases": []any{1, 2}})
	if g2.InstanceLease != 0 || len(g2.SectionLeases) != 0 {
		t.Fatalf("ill-typed object = %+v, want zero", g2)
	}
}

func TestGovSummaryStates(t *testing.T) {
	me := 4

	// Instance free.
	if got := govSummary(me, GovernedState{Generation: 1}); !strings.Contains(got, "instance lease: free") || !strings.Contains(got, "generation 1") {
		t.Fatalf("free = %q", got)
	}
	// Instance held by you.
	if got := govSummary(me, GovernedState{InstanceLease: me}); !strings.Contains(got, "instance lease: you") {
		t.Fatalf("you = %q", got)
	}
	// Instance held by another client.
	if got := govSummary(me, GovernedState{InstanceLease: 9}); !strings.Contains(got, "instance lease: client 9") {
		t.Fatalf("other = %q", got)
	}

	// Sections render sorted, with "you" vs "client N" per holder.
	got := govSummary(me, GovernedState{
		InstanceLease: me,
		SectionLeases: map[string]int{"Osc": 2, "Amp": me},
		Generation:    5,
	})
	if !strings.Contains(got, `"Amp"->you`) || !strings.Contains(got, `"Osc"->client 2`) {
		t.Fatalf("section rendering = %q", got)
	}
	// Amp sorts before Osc.
	if strings.Index(got, "Amp") > strings.Index(got, "Osc") {
		t.Fatalf("sections not sorted: %q", got)
	}
	if !strings.Contains(got, "generation 5") {
		t.Fatalf("generation missing: %q", got)
	}
}

func TestGovToolsNotLiveGuards(t *testing.T) {
	s := newLiveTestSession() // never connected -> s.live == nil
	ctx := context.Background()

	ares, _, _ := s.handleAcquireLease(ctx, nil, acquireLeaseIn{})
	rres, _, _ := s.handleReleaseLease(ctx, nil, releaseLeaseIn{})
	gres, _, _ := s.handleGetLeases(ctx, nil, emptyIn{})
	pres, _, _ := s.handlePollEvents(ctx, nil, pollEventsIn{})

	guards := map[string]string{
		"acquire_lease": textOf(ares),
		"release_lease": textOf(rres),
		"get_leases":    textOf(gres),
		"poll_events":   textOf(pres),
	}
	for name, txt := range guards {
		if !strings.Contains(txt, "not live") {
			t.Errorf("%s offline = %q, want not-live guard", name, txt)
		}
	}
}
