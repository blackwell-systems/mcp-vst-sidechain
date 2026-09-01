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

// TestGovInstanceLeaseIsExclusive is a targeted check on the REJECT policy: once a controller holds the whole
// instance, another controller's acquire is rejected and the holder is unchanged; the holder can release it.
func TestGovInstanceLeaseIsExclusive(t *testing.T) {
	s := initialGovState()

	s, r := s.apply(govCmd{kind: cmdAcquireInstance, by: 1})
	if r != resApplied || s.instanceLease != 1 {
		t.Fatalf("controller 1 should take the instance lease: state=%+v res=%v", s, r)
	}

	s2, r2 := s.apply(govCmd{kind: cmdAcquireInstance, by: 2})
	if r2 != resRejected || s2.instanceLease != 1 {
		t.Fatalf("controller 2's acquire should be rejected with the holder unchanged: state=%+v res=%v", s2, r2)
	}

	s3, r3 := s.apply(govCmd{kind: cmdReleaseInstance, by: 1})
	if r3 != resApplied || s3.instanceLease != 0 {
		t.Fatalf("controller 1 should release the instance lease: state=%+v res=%v", s3, r3)
	}
}

// TestGovSectionGuardedByInstance checks the hierarchy reject guard: a controller cannot take a section of an
// instance another controller holds, but the instance holder can take its own sections.
func TestGovSectionGuardedByInstance(t *testing.T) {
	s := initialGovState()
	s, _ = s.apply(govCmd{kind: cmdAcquireInstance, by: 1})

	s2, r := s.apply(govCmd{kind: cmdAcquireSection, by: 2, scope: 0})
	if r != resRejected || s2.sectionLease[0] != 0 {
		t.Fatalf("controller 2 should not take a section of controller 1's instance: state=%+v res=%v", s2, r)
	}

	s3, r3 := s.apply(govCmd{kind: cmdAcquireSection, by: 1, scope: 0})
	if r3 != resApplied || s3.sectionLease[0] != 1 {
		t.Fatalf("the instance holder should take its own section: state=%+v res=%v", s3, r3)
	}
}

// TestGovAcquireInstanceCompensates is a targeted check on the COMPENSATE policy: with the instance free and two
// controllers holding different sections, taking the whole instance is not rejected - it is compensated, revoking
// the other controller's section while keeping the acquirer's own.
func TestGovAcquireInstanceCompensates(t *testing.T) {
	s := initialGovState()
	s, _ = s.apply(govCmd{kind: cmdAcquireSection, by: 1, scope: 0})
	s, _ = s.apply(govCmd{kind: cmdAcquireSection, by: 2, scope: 1})

	s2, r := s.apply(govCmd{kind: cmdAcquireInstance, by: 1})
	if r != resCompensated || s2.instanceLease != 1 || s2.sectionLease[1] != 0 || s2.sectionLease[0] != 1 || !s2.ok() {
		t.Fatalf("taking the instance should compensate by revoking controller 2's section: state=%+v res=%v", s2, r)
	}
}

// TestGovSectionsIndependent makes the parametric argument executable: section leases interact only with the
// instance lease, never with each other. A command changes a section's lease only if it names that section or is
// a cross-cutting op that acts through the instance/controller (AcquireInstance's compensation, ControllerGone).
// No section command touches another section. This per-scope independence is why enumerating a fixed, small set of
// representative scopes here covers the runtime, where sections are keyed by the plugin's actual param-group names
// (cpp/GovernedState.h) - any number of them - rather than a fixed array.
func TestGovSectionsIndependent(t *testing.T) {
	for _, s := range allGovStates() {
		if !s.ok() {
			continue // only reason about legal states
		}
		for _, c := range allGovCommands() {
			n, _ := s.apply(c)
			crossCutting := c.kind == cmdAcquireInstance || c.kind == cmdControllerGone
			for i := 0; i < scopeCount; i++ {
				named := (c.kind == cmdAcquireSection || c.kind == cmdReleaseSection) && c.scope == i
				if !crossCutting && !named && n.sectionLease[i] != s.sectionLease[i] {
					t.Fatalf("section %d changed by an unrelated command %+v: %+v -> %+v", i, c, s, n)
				}
			}
		}
	}
}

// TestGovDisconnectFreesLeases checks the cleanup transition: a departing controller's leases (instance and
// sections) are all released.
func TestGovDisconnectFreesLeases(t *testing.T) {
	s := initialGovState()
	s, _ = s.apply(govCmd{kind: cmdAcquireInstance, by: 1})
	s, _ = s.apply(govCmd{kind: cmdAcquireSection, by: 1, scope: 2})

	s2, r := s.apply(govCmd{kind: cmdControllerGone, by: 1})
	if r != resApplied || s2.instanceLease != 0 || s2.sectionLease[2] != 0 {
		t.Fatalf("controller 1 leaving should free its leases: state=%+v res=%v", s2, r)
	}
}
