// sweep_live_test.go - GENERIC, catalog-driven end-to-end tests that work against ANY hosted plugin via its
// enumerated catalog (no plugin-specific ids). They prove the whole control surface is drivable without a
// crash, that the opaque save/load state path round-trips, that batch set_params applies, and that MIDI acks.
//
// Everything here is GATED on env so the normal `go test` stays green:
//   SIDECHAIN_SWEEP_PORT     - control port of a running host
//   SIDECHAIN_SWEEP_CATALOG  - path to the catalog JSON that host wrote
//   SIDECHAIN_SWEEP_MIDI=1   - additionally run the MIDI smoke (instruments only; effects have no MIDI in)
//
// The C++ host serves ONE client at a time, so every test that connects MUST disconnect (deferred) or it blocks
// the next connect. The integration workflow points these env vars at each plugin in turn (see integration.yml).

package sidechain

import (
	"context"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
)

// sweepEnv reads the shared gate (port + catalog) and skips cleanly when unset. midiOnly=true additionally
// requires SIDECHAIN_SWEEP_MIDI=1 so the MIDI test only runs for instruments.
func sweepEnv(t *testing.T, midiOnly bool) (port int, cat *Catalog) {
	t.Helper()
	portStr := os.Getenv("SIDECHAIN_SWEEP_PORT")
	catPath := os.Getenv("SIDECHAIN_SWEEP_CATALOG")
	if portStr == "" || catPath == "" {
		t.Skip("set SIDECHAIN_SWEEP_PORT and SIDECHAIN_SWEEP_CATALOG to run the generic live sweep")
	}
	if midiOnly && os.Getenv("SIDECHAIN_SWEEP_MIDI") != "1" {
		t.Skip("set SIDECHAIN_SWEEP_MIDI=1 to run the MIDI smoke (instruments only)")
	}
	p, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("bad SIDECHAIN_SWEEP_PORT %q: %v", portStr, err)
	}
	data, err := os.ReadFile(catPath)
	if err != nil {
		t.Fatalf("read catalog %q: %v", catPath, err)
	}
	cat, err = loadCatalogJSON(data)
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	return p, cat
}

// connectSwept dials the host and asserts the connect stuck. The returned cleanup MUST be deferred by the caller
// so the single-client host is freed for the next test.
func connectSwept(t *testing.T, s *session, port int) (ctx context.Context, cleanup func()) {
	t.Helper()
	ctx = context.Background()
	cres, _, err := s.handleConnectLive(ctx, nil, connectLiveIn{Host: "127.0.0.1", Port: port})
	if err != nil {
		t.Fatalf("connect_live: %v", err)
	}
	if !strings.Contains(textOf(cres), "Connected LIVE") {
		t.Fatalf("connect_live did not connect: %s", textOf(cres))
	}
	return ctx, func() { s.handleDisconnectLive(ctx, nil, emptyIn{}) }
}

// TestFullSurfaceSweep drives EVERY param in the catalog: for each, set normalized {0, 0.5, 1} and read it
// back. It asserts no handler errored and that the plugin returned a live value/text for each set. A tally is
// kept and the test FAILS if any param errored or the sweep did not cover the whole surface. This is the "can
// the agent touch the entire control surface without crashing the plugin" proof; thousands of localhost
// round-trips take a few seconds even on a ~774-param synth.
func TestFullSurfaceSweep(t *testing.T) {
	port, cat := sweepEnv(t, false)
	s := newSession(cat)
	_, cleanup := connectSwept(t, s, port)
	defer cleanup()
	ctx := context.Background()

	all := cat.All()
	if len(all) == 0 {
		t.Fatal("catalog enumerated zero params")
	}
	levels := []float64{0, 0.5, 1}
	var covered, setErrs, getErrs int
	var failures []string
	for _, p := range all {
		id := p.ID
		covered++
		for _, lvl := range levels {
			n := lvl
			sres, _, err := s.handleSetParam(ctx, nil, setParamIn{ID: id, Normalized: &n})
			if err != nil {
				setErrs++
				failures = append(failures, id+" set err: "+err.Error())
				continue
			}
			// A live set reports "Set LIVE ..."; a text guard (unknown id, bad input) never reaches the plugin,
			// so treat a non-"Set LIVE" reply on a known id as a failure to drive the surface.
			if !strings.Contains(textOf(sres), "Set LIVE") {
				setErrs++
				failures = append(failures, id+" set not live: "+textOf(sres))
				continue
			}
			gres, gout, err := s.handleGetParam(ctx, nil, getParamIn{ID: id})
			if err != nil {
				getErrs++
				failures = append(failures, id+" get err: "+err.Error())
				continue
			}
			if gout == nil || textOf(gres) == "" {
				getErrs++
				failures = append(failures, id+" get returned no value/text")
			}
		}
	}

	if covered != len(all) {
		t.Fatalf("sweep covered %d params but the catalog has %d", covered, len(all))
	}
	if setErrs > 0 || getErrs > 0 {
		const maxShow = 20
		show := failures
		if len(show) > maxShow {
			show = show[:maxShow]
		}
		t.Fatalf("full-surface sweep hit errors on %d params (%d set, %d get). First %d:\n%s",
			len(failures), setErrs, getErrs, len(show), strings.Join(show, "\n"))
	}
	t.Logf("full-surface sweep OK: %d params x %d levels = %d set/get round-trips, no errors",
		covered, len(levels), covered*len(levels))
}

// TestStateRoundTrip proves the opaque save/load path end-to-end. It snapshots a handful of numeric params'
// live readings, saves the whole state, mutates those params to clearly different normalized values (confirming
// they actually changed), loads the saved blob back, and asserts each param is restored to its snapshot value.
func TestStateRoundTrip(t *testing.T) {
	port, cat := sweepEnv(t, false)
	s := newSession(cat)
	ctx, cleanup := connectSwept(t, s, port)
	defer cleanup()

	// Pick the first ~15 numeric (float/int) params: choice/bool params can be coarsely quantized, which makes
	// the "mutated to a clearly different value" step unreliable, so numeric params give the cleanest proof.
	var ids []string
	for _, p := range cat.All() {
		if p.Type == "float" || p.Type == "int" {
			ids = append(ids, p.ID)
		}
		if len(ids) >= 15 {
			break
		}
	}
	if len(ids) == 0 {
		t.Skip("catalog exposes no numeric params to round-trip")
	}

	// getNorm reads a param's current normalized value straight off the running instance.
	getNorm := func(id string) (float64, bool) {
		_, gout, err := s.handleGetParam(ctx, nil, getParamIn{ID: id})
		if err != nil || gout == nil {
			return 0, false
		}
		g, ok := gout.(struct {
			Param      ParamDef `json:"param"`
			Value      float64  `json:"value"`
			Normalized float64  `json:"normalized"`
			IsSet      bool     `json:"isSet"`
			Live       bool     `json:"live"`
		})
		if !ok {
			return 0, false
		}
		return g.Normalized, true
	}

	snapshot := make(map[string]float64, len(ids))
	for _, id := range ids {
		n, ok := getNorm(id)
		if !ok {
			t.Fatalf("could not read initial normalized value for %s", id)
		}
		snapshot[id] = n
	}

	// save_state: pull the whole patch as one opaque blob.
	sres, sout, err := s.handleSaveState(ctx, nil, emptyIn{})
	if err != nil {
		t.Fatalf("save_state: %v", err)
	}
	if !strings.Contains(textOf(sres), "Saved live state") {
		t.Fatalf("save_state reply = %q", textOf(sres))
	}
	saved, ok := sout.(struct {
		State string `json:"state"`
	})
	if !ok || strings.TrimSpace(saved.State) == "" {
		t.Fatalf("save_state produced no blob (out=%v)", sout)
	}

	// Mutate every snapshotted param to a clearly different normalized value and confirm at least one moved.
	// (An individual param may already sit at an endpoint and not budge; requiring ALL to move would be flaky,
	// but if NONE moved the mutation step is not actually exercising anything, which is worth failing on.)
	var moved int
	for _, id := range ids {
		target := 0.9
		if snapshot[id] > 0.5 {
			target = 0.1 // push away from wherever it currently sits
		}
		n := target
		if _, _, err := s.handleSetParam(ctx, nil, setParamIn{ID: id, Normalized: &n}); err != nil {
			t.Fatalf("mutate %s: %v", id, err)
		}
		if cur, ok := getNorm(id); ok && math.Abs(cur-snapshot[id]) > 0.05 {
			moved++
		}
	}
	if moved == 0 {
		t.Fatalf("mutation step changed none of %d params; the round-trip would prove nothing", len(ids))
	}

	// load_state: recall the saved blob.
	if _, _, err := s.handleLoadState(ctx, nil, loadStateIn{State: saved.State}); err != nil {
		t.Fatalf("load_state: %v", err)
	}

	// Re-read and assert each param is restored near its snapshot. A plugin may quantize on state recall, so
	// allow a small tolerance rather than bit-exact equality.
	const tol = 0.02
	var restoreFails []string
	for _, id := range ids {
		cur, ok := getNorm(id)
		if !ok {
			restoreFails = append(restoreFails, id+" (unreadable after load)")
			continue
		}
		if math.Abs(cur-snapshot[id]) > tol {
			restoreFails = append(restoreFails, id+" want "+formatFloat(snapshot[id])+" got "+formatFloat(cur))
		}
	}
	if len(restoreFails) > 0 {
		t.Fatalf("state did not restore %d/%d params within %.2f:\n%s", len(restoreFails), len(ids), tol,
			strings.Join(restoreFails, "\n"))
	}
	t.Logf("state round-trip OK: snapshot/save/mutate(%d moved)/load/restore across %d numeric params, blob %d bytes",
		moved, len(ids), len(saved.State))
}

// TestBatchSetParams builds a set_params call with N normalized rows for real catalog ids and asserts the report
// says it applied all N and skipped 0. This proves the batch authoring path (the one-call-per-patch direction)
// against a real plugin.
func TestBatchSetParams(t *testing.T) {
	port, cat := sweepEnv(t, false)
	s := newSession(cat)
	ctx, cleanup := connectSwept(t, s, port)
	defer cleanup()

	all := cat.All()
	n := 10
	if len(all) < n {
		n = len(all)
	}
	if n == 0 {
		t.Fatal("catalog enumerated zero params")
	}
	rows := make([]setParamsRow, 0, n)
	for i := 0; i < n; i++ {
		half := 0.5
		rows = append(rows, setParamsRow{ID: all[i].ID, Normalized: &half})
	}
	res, _, err := s.handleSetParams(ctx, nil, setParamsIn{Params: rows})
	if err != nil {
		t.Fatalf("set_params: %v", err)
	}
	txt := textOf(res)
	// The report reads "set_params: applied N to the LIVE instance, skipped 0." We assert both halves so a
	// silent skip (e.g. an id the live path rejected) fails loudly.
	wantApplied := "applied " + strconv.Itoa(n) + " to the LIVE instance"
	if !strings.Contains(txt, wantApplied) || !strings.Contains(txt, "skipped 0") {
		t.Fatalf("set_params did not apply all %d rows cleanly: %s", n, txt)
	}
	t.Logf("batch set_params OK: %s", txt)
}

// TestMidiSmoke plays a middle-C for a short hold and then panics with all_notes_off, asserting both ack without
// error. Gated additionally on SIDECHAIN_SWEEP_MIDI=1 so it only runs for instruments (an effect has no MIDI in
// and would leave the note dangling / ack meaninglessly).
func TestMidiSmoke(t *testing.T) {
	port, cat := sweepEnv(t, true)
	s := newSession(cat)
	ctx, cleanup := connectSwept(t, s, port)
	defer cleanup()

	pres, _, err := s.handlePlayNote(ctx, nil, playNoteIn{Note: 60, Vel: 0.8, Chan: 1, HoldMs: 50})
	if err != nil {
		t.Fatalf("play_note: %v", err)
	}
	if !strings.Contains(textOf(pres), "Played note 60") {
		t.Fatalf("play_note did not ack a held note: %s", textOf(pres))
	}
	ares, _, err := s.handleAllNotesOff(ctx, nil, emptyIn{})
	if err != nil {
		t.Fatalf("all_notes_off: %v", err)
	}
	if !strings.Contains(textOf(ares), "All notes off") {
		t.Fatalf("all_notes_off did not ack: %s", textOf(ares))
	}
	t.Logf("MIDI smoke OK: %s / %s", textOf(pres), textOf(ares))
}
