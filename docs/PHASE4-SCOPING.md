# Phase 4 scoping: intent -> params (closing the perception loop)

Status: IN PROGRESS. Increment 1 (`tune_param`, the closed-loop optimizer) is the first landing. A buildable spec
for turning an INTENT ("make it brighter", "get it to -12 dB RMS") into concrete parameter moves, by driving the
render + measure loop as an objective function. Peer docs: [ARCHITECTURE.md](ARCHITECTURE.md) (the system),
[RENDER-SCOPING.md](RENDER-SCOPING.md) (the ears this optimizes against), [PHASE3-SCOPING.md](PHASE3-SCOPING.md)
(the semantic map the agent reasons over), [POSITIONING.md](POSITIONING.md) (why this is ours to own).

## Goal

Phases 1-3 gave the agent a map (what each param means: behavior class + agent-authored role) and Phase render
gave it ears (an objective measurement of the output). Phase 4 closes the loop: an intent becomes a param move that
is VERIFIED against a measurement, not guessed. "Make it brighter" stops being "set the knob named cutoff up a bit
and hope" and becomes "drive this param until the spectral centroid actually rose."

## Where the intelligence lives (the load-bearing decision)

The bridge enforces NO ontology (Phase 3: roles are free-form and agent-authored). Phase 4 keeps that split:

- **The agent owns the semantics.** Given a natural-language intent and `get_semantic_map`, the AGENT decides WHICH
  param(s) to move and on WHICH measurement. "Brighter" -> the param whose role is `filter.cutoff` (or a
  brightness/tilt control), measured by `centroid_hz`, direction up. This is LLM reasoning over the map; the server
  hardcodes none of it. A new plugin, a weird param name, an unusual routing: the agent adapts, no code change.
- **The server owns the mechanism.** Once the agent has picked (param, measure, direction/target), converging the
  param objectively is a deterministic bounded search over the render+measure loop. That is what the server does
  well and reproducibly, and it is what `tune_param` provides.

Anti-goal: a `match_intent("make it brighter")` tool that maps free text to params in Go. That would bake an
ontology into the bridge, contradict Phase 3, and rot the moment a plugin names things differently. The agent is
the natural-language layer; the server is the objective-search layer.

## Increment 1: `tune_param` (the closed-loop optimizer)

Drive ONE parameter toward a goal on ONE measurement, using an offline render at each step as the objective
function. The agent supplies the (param, measure, goal) triple from its reasoning over the semantic map; the tool
converges it and reports the trace.

### Input

- `id` (required): the parameter to tune.
- `measure` (required): which measurement is the objective. One of `centroid_hz` (brightness), `peak_db`,
  `rms_db` (loudness), `crest` (transient-ness), `low_db` / `mid_db` / `high_db` (band energy).
- `goal` (required): `maximize` | `minimize` | `target`.
- `target` (required iff `goal=target`): the target value in the measure's unit (e.g. `rms_db = -12`).
- `seeds` (default 5): coarse uniform samples across the normalized [0,1] range.
- `refineIters` (default 4): golden-section refinement steps after the coarse pass.
- `restore` (default false): if true, restore the param to its starting value after searching (measure-only, a
  what-if); if false, LEAVE the param at the best value found (the point of "make it brighter").
- render fields (`note`, `velocity`, `channel`, `gateMs`, `durationMs`, `inputKind`, `inputFreq`, `inputLevel`):
  identical to `render_and_measure`, held fixed across the whole search so only the tuned param varies.

### Algorithm (bounded, no monotonicity assumption)

1. Read the param's starting normalized value and measure it once (the baseline).
2. **Coarse pass:** evaluate `measure` at `seeds` uniform points across [0,1] (inclusive endpoints).
3. **Score:** `maximize` -> value; `minimize` -> -value; `target` -> -|value - target|. Higher is better.
4. Pick the best coarse point and bracket it with its immediate neighbors (clamped to [0,1]).
5. **Refine:** golden-section search within that bracket for `refineIters` steps, keeping the best.
6. Set the param to the best normalized value (or restore to the start if `restore`).
7. Return the full evaluation trace, the best value + its complete measurement, and a one-line summary.

Total renders ~ `seeds + refineIters` (about 9 by default). Bounded and reproducible. A coarse grid plus a local
refine handles both the common monotonic case (cutoff -> centroid: optimum at an endpoint) and a unimodal one (a
resonant peak) without assuming which. Non-monotonic-with-multiple-optima is out of scope for a single 1-D pass;
the trace makes that visible so the agent can decide to look elsewhere.

### Output

`{ id, measure, goal, target?, startNormalized, startValue, bestNormalized, bestValue, restored, evaluations:
[{normalized, value}], measurement, summary }`. `summary` reads e.g. `tuned Cutoff: centroid_hz 220 -> 3980 Hz over
9 renders (left at normalized 0.98)`.

### Notes and edge cases

- **Silent renders** score poorly for level/brightness goals (a silent render has a floor centroid and level), so
  the search naturally avoids dead regions; they are still recorded in the trace.
- **Choice params** are rejected with a message pointing at `set_param choice=` (a discrete list is not a 1-D
  continuum to golden-section over). Discrete-as-float params are allowed but refine coarsely by nature.
- **Live only** (it renders). Serializes with every other edit on the single applier, like `render_and_measure`.
- The tool does not touch the semantic store; it is pure mechanism. The agent may follow a successful tune with
  `annotate_params` to record what it learned (e.g. confirm a role).

## Later increments (deferred)

- **Multi-param tune** (`tune_params`): co-optimize a small set (coordinate descent over the render loop), for
  intents that need two knobs (e.g. "punchier" = attack down + drive up). 1-D first; the loop generalizes.
- **A worked "intent playbook"** (docs, not code): example agent transcripts mapping common intents (brighter,
  warmer, punchier, wider, cleaner) to (role, measure, direction) over the semantic map, so the agent has priors
  without an ontology in the bridge.
- **Richer measures** (LUFS, attack time, a finer spectrum) if intents demand them; these extend the render
  analysis set, and `tune_param` picks them up for free once `measure` accepts them.
- **Modulation intents** ("wobble", "vibrato", "movement") are now partly supported: RENDER-SCOPING Tier 2.5 (the
  `modulation` block) is IMPLEMENTED, so an LFO's rate and depth are ordinary scalars `tune_param` optimizes
  (`measure=modulation.centroid.rate_hz`, temporal auto-enabled; proven by `TestTuneModulationRateLive`). What
  remains agent-driven (correctly, no ontology in the bridge): the WAVEFORM (a discrete `set_param choice=`) and the
  ROUTING/destination (a discrete selector or mod-matrix cell the agent sets from its recipe), plus sequencing the
  1-D tunes for a compositional intent (set destination + waveform, tune rate, tune depth, verify).

## Test plan

- **In-memory (fake host):** the fake's render responds to the tuned param (centroid rises with the param), so
  `tune_param` maximize converges near the top of the range and `target` converges near the requested value,
  deterministically and with no DSP. Covers the search logic, scoring, bracketing, and set/restore.
- **Gated real-host E2E (`TestTuneBrighterLive`, on TAL Filter Cutoff):** `maximize centroid_hz` leaves the cutoff
  high and the centroid well above the starting value; `target` a mid centroid lands within tolerance. This is the
  make-it-brighter loop run AUTONOMOUSLY (the tool converges it), the Phase 4 payoff proven end to end.

## Implementation sequence

1. `tune_tools.go`: the `tune_param` tool + the bounded search, reusing `render_and_measure`'s spec and the
   session's `set_param`. Register in `NewServer`.
2. Make the fake host's render respond to the tuned param (a monotonic centroid) so the tool is unit-testable.
3. `tune_tools_test.go` (in-memory) + `tune_live_test.go` (gated E2E on TAL). Wire the E2E into `integration.yml`.
4. Docs: fold into ARCHITECTURE (tool row + a Phase-4 note), CHANGELOG, README.
