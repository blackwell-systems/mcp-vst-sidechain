// full_tool_surface_test.go - the completeness proof: EVERY MCP tool handler driven against a real host in one
// gated run, so no tool is only unit- or fake-host-tested. It complements the focused gated tests (sweep, state,
// governed, semantic) by asserting the whole agent-facing surface works end to end on a live plugin, including
// poll_events surfacing a REAL change pushed by a second controller. Gated on the sweep env; run per plugin by
// drive_plugin.sh.

package sidechain

import (
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestFullToolSurfaceLive(t *testing.T) {
	port, cat := sweepEnv(t, false)

	var floats []string
	for _, p := range cat.All() {
		if p.Type == "float" {
			floats = append(floats, p.ID)
		}
		if len(floats) >= 2 {
			break
		}
	}
	if len(floats) < 2 {
		t.Skip("need at least two float params to exercise the full tool surface")
	}
	pa, pb := floats[0], floats[1]
	first := cat.All()[0].ID

	s := newSession(cat)
	if err := s.attachStore(NewSemanticStore(t.TempDir())); err != nil {
		t.Fatalf("attach store: %v", err)
	}
	ctx, cleanup := connectSwept(t, s, port) // connect_live + disconnect_live
	defer cleanup()

	// ok asserts a handler returned no Go error and no failure text (handlers report tool-level failures as text
	// with a nil error), and returns the reply text.
	ok := func(tool string, res *mcp.CallToolResult, err error) string {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: err %v", tool, err)
		}
		low := strings.ToLower(textOf(res))
		if strings.Contains(low, "failed") || strings.Contains(low, "not live") || strings.Contains(low, "not enabled") {
			t.Fatalf("%s: %s", tool, textOf(res))
		}
		return textOf(res)
	}

	// Reads / catalog.
	r, _, e := s.handleListParams(ctx, nil, listParamsIn{})
	ok("list_params", r, e)
	r, _, e = s.handleGetParam(ctx, nil, getParamIn{ID: first})
	ok("get_param", r, e)
	r, _, e = s.handleDescribeParam(ctx, nil, describeParamIn{ID: pa})
	ok("describe_param", r, e)

	// Writes.
	half := 0.5
	r, _, e = s.handleSetParam(ctx, nil, setParamIn{ID: pa, Normalized: &half})
	ok("set_param", r, e)
	r, _, e = s.handleSetParams(ctx, nil, setParamsIn{Params: []setParamsRow{{ID: pa, Normalized: &half}}})
	ok("set_params", r, e)

	// Whole-patch state: save, load, reset.
	sres, sout, se := s.handleSaveState(ctx, nil, emptyIn{})
	ok("save_state", sres, se)
	blob := ""
	if o, k := sout.(struct {
		State string `json:"state"`
	}); k {
		blob = o.State
	}
	if blob == "" {
		t.Fatal("save_state returned no blob")
	}
	r, _, e = s.handleLoadState(ctx, nil, loadStateIn{State: blob})
	ok("load_state", r, e)
	r, _, e = s.handleResetInit(ctx, nil, emptyIn{})
	ok("reset_init", r, e)

	// MIDI (the host acks a note round-trip even on an effect, which has no audible voice).
	r, _, e = s.handlePlayNote(ctx, nil, playNoteIn{Note: 60, HoldMs: 20})
	ok("play_note", r, e)
	r, _, e = s.handleAllNotesOff(ctx, nil, emptyIn{})
	ok("all_notes_off", r, e)

	// Governed edit leases.
	r, lout, e := s.handleAcquireLease(ctx, nil, acquireLeaseIn{})
	ok("acquire_lease", r, e)
	if lout.(leaseResult).Resolution == "rejected" {
		t.Fatalf("acquire_lease was rejected on a fresh host: %s", textOf(r))
	}
	r, _, e = s.handleGetLeases(ctx, nil, emptyIn{})
	ok("get_leases", r, e)
	r, _, e = s.handleReleaseLease(ctx, nil, releaseLeaseIn{})
	ok("release_lease", r, e)

	// Semantics: annotate -> read back in the map -> forget.
	r, _, e = s.handleAnnotateParams(ctx, nil, annotateParamsIn{Params: []annotateRow{{ID: pa, Role: "test.role"}}})
	ok("annotate_params", r, e)
	r, mout, e := s.handleGetSemanticMap(ctx, nil, emptyIn{})
	ok("get_semantic_map", r, e)
	found := false
	for _, row := range mout.(semanticMapOut).Params {
		if row.ID == pa && row.Role == "test.role" {
			found = true
		}
	}
	if !found {
		t.Fatal("get_semantic_map did not reflect the annotation")
	}
	r, _, e = s.handleForgetSemantics(ctx, nil, emptyIn{})
	ok("forget_semantics", r, e)

	// poll_events: a change by a SECOND controller must surface through the tool (real host-pushed event).
	b, err := dialLive("127.0.0.1", port)
	if err != nil {
		t.Fatalf("second controller dial: %v", err)
	}
	defer b.Close()
	if _, _, _, err := b.SetParam(pb, 0.33, false); err != nil {
		t.Fatalf("second controller set: %v", err)
	}
	deadline := time.After(3 * time.Second)
	saw := false
	for !saw {
		select {
		case <-deadline:
			t.Fatal("poll_events never surfaced the second controller's change")
		default:
		}
		_, pout, perr := s.handlePollEvents(ctx, nil, pollEventsIn{})
		if perr != nil {
			t.Fatalf("poll_events: %v", perr)
		}
		for _, c := range pout.(pollResult).ParamChanges {
			if c.Param == pb && c.By == b.clientID {
				saw = true
			}
		}
		if !saw {
			time.Sleep(50 * time.Millisecond)
		}
	}

	t.Logf("full tool surface OK: all 19 MCP tools exercised end to end against %s", cat.Plugin.Name)
}
