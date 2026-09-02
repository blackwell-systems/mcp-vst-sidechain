// govtools_test.go - in-memory tests for the concurrency MCP tools (govtools.go). The lease tools are driven
// through a real liveClient against the fake host (which now answers govern/get_governed); poll_events is tested
// by injecting server-pushed events into the client's channel and asserting the dedup + echo-suppression logic.

package sidechain

import (
	"context"
	"strings"
	"testing"
)

func TestGovernedTools(t *testing.T) {
	fh := startFakeHost(t)
	defer fh.stop()
	s := newLiveTestSession()
	ctx := context.Background()
	if _, _, err := s.handleConnectLive(ctx, nil, connectLiveIn{Host: "127.0.0.1", Port: fh.port()}); err != nil {
		t.Fatalf("connect_live: %v", err)
	}

	// get_leases: a client id was assigned, nothing is held, and the leasable sections are reported.
	gres, gout, _ := s.handleGetLeases(ctx, nil, emptyIn{})
	lr, ok := gout.(leaseResult)
	if !ok || lr.You == 0 {
		t.Fatalf("get_leases: expected a client id, got %+v (%s)", gout, textOf(gres))
	}
	if len(lr.Leasable) != 3 {
		t.Fatalf("get_leases leasable sections: %v", lr.Leasable)
	}
	me := lr.You

	// Acquire the whole instance -> applied, you hold it.
	ares, aout, _ := s.handleAcquireLease(ctx, nil, acquireLeaseIn{})
	ar := aout.(leaseResult)
	if ar.Resolution != "applied" || ar.Instance != me {
		t.Fatalf("acquire instance: %+v (%s)", ar, textOf(ares))
	}
	if !strings.Contains(textOf(ares), "Acquired the whole instance") {
		t.Fatalf("acquire instance text: %q", textOf(ares))
	}

	// Acquire a section as the instance holder -> applied.
	_, sout, _ := s.handleAcquireLease(ctx, nil, acquireLeaseIn{Section: "Filter"})
	if sr := sout.(leaseResult); sr.Resolution != "applied" || sr.Sections["Filter"] != me {
		t.Fatalf("acquire section: %+v", sr)
	}

	// An unknown section is refused by the host and surfaced as a failure.
	ures, _, _ := s.handleAcquireLease(ctx, nil, acquireLeaseIn{Section: "Nope"})
	if !strings.Contains(textOf(ures), "failed") {
		t.Fatalf("unknown section should fail: %q", textOf(ures))
	}

	// Release the section, then the instance.
	if _, rout, _ := s.handleReleaseLease(ctx, nil, releaseLeaseIn{Section: "Filter"}); rout.(leaseResult).Sections["Filter"] != 0 {
		t.Fatalf("section should be released")
	}
	if _, riout, _ := s.handleReleaseLease(ctx, nil, releaseLeaseIn{}); riout.(leaseResult).Instance != 0 {
		t.Fatalf("instance should be released")
	}
}

func TestPollEventsDedupAndEcho(t *testing.T) {
	fh := startFakeHost(t)
	defer fh.stop()
	s := newLiveTestSession()
	ctx := context.Background()
	if _, _, err := s.handleConnectLive(ctx, nil, connectLiveIn{Host: "127.0.0.1", Port: fh.port()}); err != nil {
		t.Fatalf("connect_live: %v", err)
	}
	lc := s.live.(*liveClient)
	me := lc.ClientID()

	// Inject server-pushed events: two for cutoff (latest wins), one self-echo, one governed change - all from
	// another controller except the echo. The fake host pushes no events of its own, so the channel holds only these.
	lc.events <- map[string]any{"event": "param_changed", "param": "cutoff", "normalized": 0.3, "value": 0.3, "text": "old", "by": float64(2)}
	lc.events <- map[string]any{"event": "param_changed", "param": "cutoff", "normalized": 0.7, "value": 0.7, "text": "new", "by": float64(2)}
	lc.events <- map[string]any{"event": "param_changed", "param": "reso", "text": "r", "by": float64(me)}
	lc.events <- map[string]any{"event": "governed_changed", "governed": map[string]any{"instance_lease": float64(2), "section_leases": map[string]any{}, "generation": float64(1)}, "by": float64(2)}

	_, pout, _ := s.handlePollEvents(ctx, nil, pollEventsIn{})
	pr := pout.(pollResult)
	if len(pr.ParamChanges) != 1 || pr.ParamChanges[0].Param != "cutoff" || pr.ParamChanges[0].Text != "new" {
		t.Fatalf("expected one deduped cutoff=new with the echo filtered, got %+v", pr.ParamChanges)
	}
	if pr.Governed == nil || pr.Governed.InstanceLease != 2 {
		t.Fatalf("expected the governed change, got %+v", pr.Governed)
	}

	// includeSelf surfaces the echo; then the channel is drained (a follow-up poll is empty).
	lc.events <- map[string]any{"event": "param_changed", "param": "reso", "text": "r", "by": float64(me)}
	if _, p2, _ := s.handlePollEvents(ctx, nil, pollEventsIn{IncludeSelf: true}); len(p2.(pollResult).ParamChanges) != 1 {
		t.Fatalf("includeSelf should surface the echo")
	}
	if _, p3, _ := s.handlePollEvents(ctx, nil, pollEventsIn{}); len(p3.(pollResult).ParamChanges) != 0 {
		t.Fatalf("channel should be drained after polling")
	}
}
