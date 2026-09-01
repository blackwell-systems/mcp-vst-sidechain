// governed.go - C3 governed-state model (STUB; NOT yet wired into the live control path).
//
// This is the scaffolding described in docs/CONCURRENCY.md ("C3 sequencing"): the small, discrete,
// invariant-bearing COORDINATION state a future conflict tier would govern, kept deliberately separate from the
// plugin's continuous parameters. Those stay last-writer-wins: they carry no cross-parameter invariants and could
// not be enumerated anyway. Every field here is an enum, a bool, or a bounded int, so the reachable state space is
// finite and governed_test.go can verify it EXHAUSTIVELY - reproducing gsm's build-time guarantee ("no reachable
// state violates an invariant") with an ordinary Go test, no external engine.
//
// The three pieces from the note, plus the conflict tier that uses them:
//   reduce(state, cmd) -> the raw next state (pure, total; the result MAY be illegal)
//   ok(state)          -> the invariant predicate every reachable state must satisfy
//   repair(state)      -> a deterministic, TOTAL map into the nearest legal state (the "compensation")
//   apply(state, cmd)  -> the conflict tier, run on the single applier thread: commit the next state as
//                         resApplied (already legal), resCompensated (repaired), or resRejected (a transition
//                         guard refused it; state unchanged).
//
// The governed state modeled here is illustrative (an exclusive-edit lease, a voice-mode gate, a panic/playback
// latch): enough to exercise both a reject policy and a compensate policy. It is not the final coordination
// schema; it exists so the model and its verification are in place BEFORE the first real multi-controller
// conflict, per the sequencing decision. Nothing here is referenced by the control server yet.

package sidechain

// voiceMode is a small enum domain. A single-voice mode (mono/legato) constrains the voice budget (see ok).
type voiceMode uint8

const (
	modePoly voiceMode = iota
	modeMono
	modeLegato
)

const (
	maxControllers = 2 // a fixed, small controller set so the lease field is finite and enumerable
	minVoiceBudget = 1
	maxVoiceBudget = 4
)

// govState is the governed coordination state. Enums / bools / bounded ints ONLY, so the space is finite.
type govState struct {
	soloLease   int       // controller id holding the exclusive-edit lease; 0 = none
	mode        voiceMode // voice mode
	voiceBudget int       // active-voice budget, minVoiceBudget..maxVoiceBudget
	panicLatch  bool      // global panic latch
	playing     bool      // transport is playing
}

// initialGovState is the default legal starting point.
func initialGovState() govState {
	return govState{soloLease: 0, mode: modePoly, voiceBudget: 1, panicLatch: false, playing: false}
}

// ok is the invariant predicate: every reachable state must satisfy it. Note soloLease is NOT range-checked: it
// holds an arbitrary controller id (0 = free), and its numeric value is not an invariant - lease exclusivity is a
// transition guard in apply, not a state predicate. (allGovStates/allGovCommands bound the controller set only to
// keep the enumeration finite; the real server assigns unbounded clientIds.)
func (s govState) ok() bool {
	if s.voiceBudget < minVoiceBudget || s.voiceBudget > maxVoiceBudget {
		return false
	}
	if s.mode != modePoly && s.voiceBudget != 1 {
		return false // a single-voice mode (mono/legato) must carry a budget of exactly one
	}
	if s.panicLatch && s.playing {
		return false // the panic latch and active playback are mutually exclusive
	}
	return true
}

// repair maps any state (including an illegal one) deterministically into the nearest legal state. It must be
// TOTAL (ok(repair(s)) holds for every s) and idempotent; governed_test.go asserts both. This is the compensation
// the conflict tier commits when a command would otherwise leave an invariant violated.
func (s govState) repair() govState {
	if s.voiceBudget < minVoiceBudget {
		s.voiceBudget = minVoiceBudget
	}
	if s.voiceBudget > maxVoiceBudget {
		s.voiceBudget = maxVoiceBudget
	}
	if s.mode != modePoly {
		s.voiceBudget = 1 // compensate: a single-voice mode clamps the budget
	}
	if s.panicLatch {
		s.playing = false // compensate: panic dominates playback
	}
	return s
}

// cmdKind enumerates the governed commands.
type cmdKind uint8

const (
	cmdAcquireSolo cmdKind = iota
	cmdReleaseSolo
	cmdSetMode
	cmdSetBudget
	cmdSetPanic
	cmdSetPlaying
)

// compensates reports a command's conflict policy: true means an invariant-violating result is REPAIRED into a
// legal state; false means the command is REJECTED when it cannot apply cleanly. Lease ops are guarded
// transitions (reject); the value/mode/gate commands compensate.
func (k cmdKind) compensates() bool {
	return k != cmdAcquireSolo && k != cmdReleaseSolo
}

// govCmd is one governed command. Only the fields relevant to its kind are read.
type govCmd struct {
	kind cmdKind
	by   int       // originating controller (lease ops)
	mode voiceMode // cmdSetMode
	n    int       // cmdSetBudget
	on   bool      // cmdSetPanic / cmdSetPlaying
}

// reduce computes the raw next state for a command (pure, total; the result may be illegal).
func (s govState) reduce(c govCmd) govState {
	switch c.kind {
	case cmdAcquireSolo:
		s.soloLease = c.by
	case cmdReleaseSolo:
		if s.soloLease == c.by {
			s.soloLease = 0
		}
	case cmdSetMode:
		s.mode = c.mode
	case cmdSetBudget:
		s.voiceBudget = c.n
	case cmdSetPanic:
		s.panicLatch = c.on
	case cmdSetPlaying:
		s.playing = c.on
	}
	return s
}

// resolution is how the conflict tier handled a command.
type resolution uint8

const (
	resApplied     resolution = iota // committed as-is (already legal)
	resCompensated                   // committed after repair()
	resRejected                      // refused by a transition guard; state unchanged
)

// apply is the conflict tier, run on the single applier thread (docs/CONCURRENCY.md: "where a command applies
// stays one"). It commits the next governed state and reports how it resolved the command. Exclusive-lease
// acquisition is a guarded transition (rejected if another controller holds it); everything else either applies
// cleanly or is compensated into a legal state. Because the single applier serializes every mutation, the
// check-then-commit here is atomic - there is no TOCTOU race to defend against.
func (s govState) apply(c govCmd) (govState, resolution) {
	if c.kind == cmdAcquireSolo && s.soloLease != 0 && s.soloLease != c.by {
		return s, resRejected // the lease is held by another controller
	}
	n := s.reduce(c)
	if n.ok() {
		return n, resApplied
	}
	if c.kind.compensates() {
		return n.repair(), resCompensated
	}
	return s, resRejected
}

// allGovCommands enumerates the full command alphabet, including some out-of-range budgets so repair's clamping is
// exercised by the reachability walk in governed_test.go.
func allGovCommands() []govCmd {
	var cs []govCmd
	for by := 1; by <= maxControllers; by++ {
		cs = append(cs, govCmd{kind: cmdAcquireSolo, by: by})
		cs = append(cs, govCmd{kind: cmdReleaseSolo, by: by})
	}
	for _, m := range []voiceMode{modePoly, modeMono, modeLegato} {
		cs = append(cs, govCmd{kind: cmdSetMode, mode: m})
	}
	for n := 0; n <= maxVoiceBudget+1; n++ { // includes out-of-range 0 and maxVoiceBudget+1
		cs = append(cs, govCmd{kind: cmdSetBudget, n: n})
	}
	for _, on := range []bool{false, true} {
		cs = append(cs, govCmd{kind: cmdSetPanic, on: on})
		cs = append(cs, govCmd{kind: cmdSetPlaying, on: on})
	}
	return cs
}

// allGovStates enumerates a widened product space (voice budget deliberately out of range at both ends) so a
// repair-totality test can assert ok(repair(s)) for malformed states, not only reachable ones.
func allGovStates() []govState {
	var ss []govState
	for solo := 0; solo <= maxControllers; solo++ {
		for _, m := range []voiceMode{modePoly, modeMono, modeLegato} {
			for b := minVoiceBudget - 1; b <= maxVoiceBudget+1; b++ {
				for _, pn := range []bool{false, true} {
					for _, pl := range []bool{false, true} {
						ss = append(ss, govState{soloLease: solo, mode: m, voiceBudget: b, panicLatch: pn, playing: pl})
					}
				}
			}
		}
	}
	return ss
}
