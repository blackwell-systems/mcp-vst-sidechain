// governed_test.go - the exhaustive-enumeration verification of the C3 governed-state model (governed.go),
// described in docs/CONCURRENCY.md ("C3 sequencing", step 3). Because the governed state is finite and small, a
// DFS over (reachable states x command alphabet) that checks the invariant after every apply() reproduces gsm's
// build-time guarantee - "no reachable state violates an invariant" - as an ordinary test, and catches a partial
// repair() immediately. It runs in the normal gate (and under -race), so a future edit to the model that breaks
// an invariant reds CI.

package sidechain

import "testing"

// TestGovernedReachableStatesLegal walks every state reachable from the initial state under the full command
// alphabet and asserts the invariant holds throughout. It fails loudly if repair() is partial (leaves an illegal
// state) or if a reduce path sneaks an illegal state through as resApplied.
func TestGovernedReachableStatesLegal(t *testing.T) {
	cmds := allGovCommands()
	seen := map[govState]bool{}

	var visit func(govState)
	visit = func(s govState) {
		if seen[s] {
			return
		}
		seen[s] = true
		if !s.ok() {
			t.Fatalf("reachable illegal state: %+v", s)
		}
		for _, c := range cmds {
			n, _ := s.apply(c)
			if !n.ok() {
				t.Fatalf("apply(%+v, %+v) produced an illegal state %+v", s, c, n)
			}
			visit(n)
		}
	}
	visit(initialGovState())
	t.Logf("verified %d reachable governed states legal under %d commands", len(seen), len(cmds))
}

// TestGovRepairTotality asserts repair() is TOTAL: it maps every state in a widened (deliberately malformed)
// product space into a legal one. This is the strongest single guarantee behind the compensate policy - if it
// holds, the conflict tier can never commit an illegal compensated state.
func TestGovRepairTotality(t *testing.T) {
	states := allGovStates()
	for _, s := range states {
		if r := s.repair(); !r.ok() {
			t.Fatalf("repair(%+v) = %+v is still illegal", s, r)
		}
	}
	t.Logf("repair() legalized all %d states in the widened space", len(states))
}

// TestGovRepairIdempotent asserts repair() is a fixed point on its own output, so a compensated state never drifts
// under a second repair.
func TestGovRepairIdempotent(t *testing.T) {
	for _, s := range allGovStates() {
		once := s.repair()
		if twice := once.repair(); twice != once {
			t.Fatalf("repair not idempotent: repair(%+v) = %+v, repaired again = %+v", s, once, twice)
		}
	}
}

// TestGovLeaseIsExclusive is a targeted check on the REJECT policy: once a controller holds the solo lease,
// another controller's acquire is rejected and the holder is unchanged; the holder can still release it.
func TestGovLeaseIsExclusive(t *testing.T) {
	s := initialGovState()

	s, r := s.apply(govCmd{kind: cmdAcquireSolo, by: 1})
	if r != resApplied || s.soloLease != 1 {
		t.Fatalf("controller 1 should take the lease: state=%+v res=%v", s, r)
	}

	s2, r2 := s.apply(govCmd{kind: cmdAcquireSolo, by: 2})
	if r2 != resRejected || s2.soloLease != 1 {
		t.Fatalf("controller 2's acquire should be rejected with the holder unchanged: state=%+v res=%v", s2, r2)
	}

	s3, r3 := s.apply(govCmd{kind: cmdReleaseSolo, by: 1})
	if r3 != resApplied || s3.soloLease != 0 {
		t.Fatalf("controller 1 should release the lease: state=%+v res=%v", s3, r3)
	}
}

// TestGovModeCompensates is a targeted check on the COMPENSATE policy: switching to a single-voice mode while the
// budget is >1 is not rejected - it is compensated, landing legal with the budget clamped to 1.
func TestGovModeCompensates(t *testing.T) {
	s := initialGovState()

	s, _ = s.apply(govCmd{kind: cmdSetBudget, n: 4})
	if s.voiceBudget != 4 {
		t.Fatalf("budget should be 4 in poly mode, got %+v", s)
	}

	s2, r := s.apply(govCmd{kind: cmdSetMode, mode: modeMono})
	if r != resCompensated || s2.mode != modeMono || s2.voiceBudget != 1 || !s2.ok() {
		t.Fatalf("switching to mono should compensate the budget to 1: state=%+v res=%v", s2, r)
	}
}
