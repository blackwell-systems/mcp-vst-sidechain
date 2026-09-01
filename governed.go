// governed.go - the C3 governed coordination state and its conflict tier.
//
// This is the small, discrete, invariant-bearing state that multiple controllers of ONE plugin instance must
// agree on, kept deliberately separate from the plugin's continuous parameters (those stay last-writer-wins: they
// carry no cross-parameter invariants and could not be enumerated). It models the REAL coordination concerns of
// multi-agent control, not the plugin's musical state:
//
//   - Hierarchical edit leases. A controller can take the whole-instance edit lease, or a per-section lease. The
//     invariant is hierarchical: if one controller holds the whole instance, no OTHER controller may hold a
//     section of it. Taking the instance therefore revokes others' section leases (the compensate path); trying
//     to take a section of an instance held by another is refused (a reject guard). This is how concurrent editors
//     avoid fighting over the same knobs.
//   - Patch generation. A monotone counter bumped whenever the whole patch changes (a load_state / reset_init).
//     An agent reads it to detect that the base it was editing moved under it (optimistic concurrency).
//   - Disconnect cleanup. When a controller disconnects (or crashes), every lease it held is released. Without
//     this a dead agent would hold an edit lease forever; it is the invariant that makes leases safe.
//
// Every field is a bounded int or a fixed-size array of them, so the reachable state space is finite and
// governed_test.go verifies it EXHAUSTIVELY - reproducing gsm's build-time "no reachable state violates an
// invariant" guarantee as an ordinary Go test. cpp/GovernedState.h is a faithful port wired into ControlServer's
// message-thread drain; the two must stay in lockstep so this proof continues to cover the shipped code.
//
// The three pieces from docs/CONCURRENCY.md, plus the conflict tier that uses them:
//   reduce(state, cmd) -> the raw next state (pure, total; the result MAY be illegal)
//   ok(state)          -> the invariant predicate every reachable state must satisfy
//   repair(state)      -> a deterministic, TOTAL map into the nearest legal state (the compensation)
//   apply(state, cmd)  -> the conflict tier, run on the single applier thread: resApplied (already legal),
//                         resCompensated (repaired), or resRejected (a transition guard refused it; state unchanged).

package sidechain

const (
	maxControllers = 2 // a fixed, small controller set so the lease fields are finite and enumerable
	scopeCount     = 3 // number of section scopes (an edit region; a deployment maps these to plugin param groups)
	minGeneration  = 0
	maxGeneration  = 2 // patch generation saturates here so the space stays finite (monotonicity still holds)
)

// govState is the governed coordination state. Bounded ints / fixed arrays ONLY, so the space is finite. A lease
// field holds the controller id of the holder, or 0 for free.
type govState struct {
	instanceLease int             // controller holding the whole-instance edit lease; 0 = free
	sectionLease  [scopeCount]int // controller holding each section's edit lease; 0 = free
	generation    int             // patch generation, minGeneration..maxGeneration (bumped on a whole-patch change)
}

// initialGovState is the default legal starting point (nothing leased, generation zero).
func initialGovState() govState { return govState{} }

// ok is the invariant predicate: every reachable state must satisfy it. A lease holder id is NOT range-checked -
// it is an arbitrary controller id whose magnitude is not an invariant; exclusivity is enforced by the guards and
// the hierarchy check below, not by bounding the id (the real server assigns unbounded clientIds).
func (s govState) ok() bool {
	if s.generation < minGeneration || s.generation > maxGeneration {
		return false
	}
	for i := 0; i < scopeCount; i++ {
		// Hierarchy: if the whole instance is held, no OTHER controller may hold a section of it.
		if s.instanceLease != 0 && s.sectionLease[i] != 0 && s.sectionLease[i] != s.instanceLease {
			return false
		}
	}
	return true
}

// repair maps any state (including an illegal one) deterministically into the nearest legal state. It must be
// TOTAL (ok(repair(s)) for every s) and idempotent; governed_test.go asserts both. This is the compensation the
// conflict tier commits: taking the whole instance revokes any section held by another controller.
func (s govState) repair() govState {
	if s.generation < minGeneration {
		s.generation = minGeneration
	}
	if s.generation > maxGeneration {
		s.generation = maxGeneration
	}
	if s.instanceLease != 0 {
		for i := 0; i < scopeCount; i++ {
			if s.sectionLease[i] != 0 && s.sectionLease[i] != s.instanceLease {
				s.sectionLease[i] = 0
			}
		}
	}
	return s
}

// cmdKind enumerates the governed commands. AcquireInstance/ReleaseInstance/AcquireSection/ReleaseSection are the
// controller-facing lease ops; BumpGeneration and ControllerGone are raised internally by the server (on a
// whole-patch change and on a disconnect), never by a client.
type cmdKind uint8

const (
	cmdAcquireInstance cmdKind = iota
	cmdReleaseInstance
	cmdAcquireSection
	cmdReleaseSection
	cmdBumpGeneration
	cmdControllerGone
)

// govCmd is one governed command. Only the fields relevant to its kind are read.
type govCmd struct {
	kind  cmdKind
	by    int // originating / departing controller
	scope int // cmdAcquireSection / cmdReleaseSection
}

// reduce computes the raw next state for a command (pure, total; the result may be illegal).
func (s govState) reduce(c govCmd) govState {
	switch c.kind {
	case cmdAcquireInstance:
		s.instanceLease = c.by
	case cmdReleaseInstance:
		if s.instanceLease == c.by {
			s.instanceLease = 0
		}
	case cmdAcquireSection:
		if c.scope >= 0 && c.scope < scopeCount {
			s.sectionLease[c.scope] = c.by
		}
	case cmdReleaseSection:
		if c.scope >= 0 && c.scope < scopeCount && s.sectionLease[c.scope] == c.by {
			s.sectionLease[c.scope] = 0
		}
	case cmdBumpGeneration:
		if s.generation < maxGeneration {
			s.generation++
		}
	case cmdControllerGone:
		if s.instanceLease == c.by {
			s.instanceLease = 0
		}
		for i := 0; i < scopeCount; i++ {
			if s.sectionLease[i] == c.by {
				s.sectionLease[i] = 0
			}
		}
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
// stays one"). It commits the next governed state and reports how it resolved the command. Because the single
// applier serializes every mutation, the check-then-commit here is atomic - there is no TOCTOU race to defend.
func (s govState) apply(c govCmd) (govState, resolution) {
	switch c.kind {
	case cmdAcquireInstance:
		if s.instanceLease != 0 && s.instanceLease != c.by {
			return s, resRejected // held by another controller
		}
	case cmdAcquireSection:
		if c.scope < 0 || c.scope >= scopeCount {
			return s, resRejected // no such section
		}
		if s.instanceLease != 0 && s.instanceLease != c.by {
			return s, resRejected // cannot sub-lease an instance held by another
		}
		if s.sectionLease[c.scope] != 0 && s.sectionLease[c.scope] != c.by {
			return s, resRejected // section held by another controller
		}
	}
	n := s.reduce(c)
	if n.ok() {
		return n, resApplied
	}
	// The only residual illegality the guards above do not catch is AcquireInstance while another controller holds
	// a section: compensate by revoking those section leases (repair).
	return n.repair(), resCompensated
}

// allGovCommands enumerates the full command alphabet, including an out-of-range section so the reject guard is
// exercised by the reachability walk in governed_test.go.
func allGovCommands() []govCmd {
	var cs []govCmd
	for by := 1; by <= maxControllers; by++ {
		cs = append(cs, govCmd{kind: cmdAcquireInstance, by: by})
		cs = append(cs, govCmd{kind: cmdReleaseInstance, by: by})
		for sc := 0; sc < scopeCount; sc++ {
			cs = append(cs, govCmd{kind: cmdAcquireSection, by: by, scope: sc})
			cs = append(cs, govCmd{kind: cmdReleaseSection, by: by, scope: sc})
		}
		cs = append(cs, govCmd{kind: cmdAcquireSection, by: by, scope: scopeCount}) // out of range: exercises the guard
		cs = append(cs, govCmd{kind: cmdControllerGone, by: by})
	}
	cs = append(cs, govCmd{kind: cmdBumpGeneration})
	return cs
}

// allGovStates enumerates a widened product space (generation deliberately out of range at both ends) so a
// repair-totality test can assert ok(repair(s)) for malformed states, not only reachable ones.
func allGovStates() []govState {
	var ss []govState
	base := maxControllers + 1
	combos := 1
	for i := 0; i < scopeCount; i++ {
		combos *= base
	}
	for inst := 0; inst <= maxControllers; inst++ {
		for combo := 0; combo < combos; combo++ {
			var sec [scopeCount]int
			c := combo
			for i := 0; i < scopeCount; i++ {
				sec[i] = c % base
				c /= base
			}
			for gen := minGeneration - 1; gen <= maxGeneration+1; gen++ {
				ss = append(ss, govState{instanceLease: inst, sectionLease: sec, generation: gen})
			}
		}
	}
	return ss
}
