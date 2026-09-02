// live_more_test.go - fakeHost-driven coverage for the live handlers the main loopback test does not exercise:
// the "not live" guards on the note/state verbs, the play_note holdMs auto-release path, the load_state empty
// guard, and the refineToReal out-of-sampled-range branch (a target past the sampled max keeps the clamped seed
// instead of bracketing). Uses the in-package fakeHost (live_test.go); no real plugin.

package sidechain

import (
	"context"
	"strings"
	"testing"
)

func TestLiveHandlerNotLiveGuards(t *testing.T) {
	s := newLiveTestSession() // never connected
	ctx := context.Background()

	pres, _, _ := s.handlePlayNote(ctx, nil, playNoteIn{Note: 60})
	ares, _, _ := s.handleAllNotesOff(ctx, nil, emptyIn{})
	lres, _, _ := s.handleLoadState(ctx, nil, loadStateIn{State: "<X/>"})
	rres, _, _ := s.handleResetInit(ctx, nil, emptyIn{})

	for name, txt := range map[string]string{
		"play_note":     textOf(pres),
		"all_notes_off": textOf(ares),
		"load_state":    textOf(lres),
		"reset_init":    textOf(rres),
	} {
		if !strings.Contains(txt, "not live") {
			t.Errorf("%s offline = %q, want not-live guard", name, txt)
		}
	}
}

func TestLoadStateEmptyGuard(t *testing.T) {
	// The empty-state guard fires before the live check (no connection needed).
	s := newLiveTestSession()
	res, _, _ := s.handleLoadState(context.Background(), nil, loadStateIn{State: "   "})
	if !strings.Contains(textOf(res), "provide state") {
		t.Fatalf("empty load_state = %q, want provide-state", textOf(res))
	}
}

func TestPlayNoteHoldMsAutoRelease(t *testing.T) {
	fh := startFakeHost(t)
	defer fh.stop()
	s := newLiveTestSession()
	ctx := context.Background()
	if _, _, err := s.handleConnectLive(ctx, nil, connectLiveIn{Host: "127.0.0.1", Port: fh.port()}); err != nil {
		t.Fatalf("connect_live: %v", err)
	}

	// holdMs > 0 -> note_on then an auto note_off after the hold; the reply reports the held duration.
	res, _, err := s.handlePlayNote(ctx, nil, playNoteIn{Note: 64, Vel: 0.5, HoldMs: 1})
	if err != nil {
		t.Fatalf("play_note holdMs: %v", err)
	}
	if !strings.Contains(textOf(res), "held 1 ms") {
		t.Fatalf("holdMs reply = %q, want held-1-ms", textOf(res))
	}
	fh.mu.Lock()
	on, off := fh.cmds["note_on"], fh.cmds["note_off"]
	firstNote := -1
	if len(fh.notes) > 0 {
		firstNote = fh.notes[0]
	}
	fh.mu.Unlock()
	if on != 1 || off != 1 {
		t.Fatalf("holdMs should note_on then note_off once each, got on=%d off=%d", on, off)
	}
	if firstNote != 64 {
		t.Fatalf("fake host note = %d, want 64", firstNote)
	}
}

func TestPlayNoteDefaultChannelAndVelocity(t *testing.T) {
	fh := startFakeHost(t)
	defer fh.stop()
	s := newLiveTestSession()
	ctx := context.Background()
	if _, _, err := s.handleConnectLive(ctx, nil, connectLiveIn{Host: "127.0.0.1", Port: fh.port()}); err != nil {
		t.Fatalf("connect_live: %v", err)
	}
	// Zero chan / zero vel default to channel 1 / velocity 0.8; the reply surfaces them.
	res, _, err := s.handlePlayNote(ctx, nil, playNoteIn{Note: 48})
	if err != nil {
		t.Fatalf("play_note: %v", err)
	}
	txt := textOf(res)
	if !strings.Contains(txt, "vel 0.80") || !strings.Contains(txt, "ch 1") {
		t.Fatalf("default play_note = %q, want vel 0.80 + ch 1", txt)
	}
}

func TestRefineOutOfSampledRange(t *testing.T) {
	fh := startFakeHost(t)
	defer fh.stop()
	// sigmoid fits no closed-form model (see fakeHzSigmoid), so set_param real= drives the binary-search refine.
	// Its sampled range tops out near 1000 Hz; a target well above that lands out of every sample bracket, so
	// refineToReal keeps the clamped seed (the bracket-fail branch) rather than searching.
	s := newSession(NewCatalog([]ParamDef{{ID: "sigmoid", Label: "Weird", Type: "float", Min: 0, Max: 1}}))
	ctx := context.Background()
	if _, _, err := s.handleConnectLive(ctx, nil, connectLiveIn{Host: "127.0.0.1", Port: fh.port()}); err != nil {
		t.Fatalf("connect_live: %v", err)
	}
	target := 5000.0 // far above the sigmoid's ~1000 Hz ceiling
	res, _, err := s.handleSetParam(ctx, nil, setParamIn{ID: "sigmoid", Real: &target})
	if err != nil {
		t.Fatalf("set real out-of-range: %v", err)
	}
	if !strings.Contains(textOf(res), "sampled+refined") {
		t.Fatalf("out-of-range refine reply = %q, want the sampled+refined path", textOf(res))
	}
	// It should have clamped to the top of the range (norm ~1), not errored.
	fh.mu.Lock()
	landed := fh.params["sigmoid"]
	fh.mu.Unlock()
	if landed < 0.99 {
		t.Fatalf("out-of-range target should clamp to norm ~1, got %.4f", landed)
	}
}

func TestSetParamsRealSkipTags(t *testing.T) {
	fh := startFakeHost(t)
	defer fh.stop()
	ctx := context.Background()

	// Offline: a real-unit batch row is skipped with the real-needs-live tag.
	off := newLiveTestSession()
	ores, _, _ := off.handleSetParams(ctx, nil, setParamsIn{Params: []setParamsRow{{ID: "cutoff", Real: ptrF(1000)}}})
	if !strings.Contains(textOf(ores), "real-needs-live") {
		t.Fatalf("offline real batch = %q, want real-needs-live tag", textOf(ores))
	}

	// Live but on a unitless param (the fake's "raw" renders a bare number): NormForReal fails -> no-unit tag.
	s := newSession(NewCatalog([]ParamDef{{ID: "raw", Label: "Raw", Type: "float", Min: 0, Max: 1}}))
	if _, _, err := s.handleConnectLive(ctx, nil, connectLiveIn{Host: "127.0.0.1", Port: fh.port()}); err != nil {
		t.Fatalf("connect_live: %v", err)
	}
	res, _, _ := s.handleSetParams(ctx, nil, setParamsIn{Params: []setParamsRow{{ID: "raw", Real: ptrF(0.5)}}})
	if !strings.Contains(textOf(res), "no-unit") {
		t.Fatalf("unitless real batch = %q, want no-unit tag", textOf(res))
	}
}
