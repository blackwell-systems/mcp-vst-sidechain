# Roadmap

Where mcp-vst-sidechain is, and where it goes next. This is a living document; horizons are direction, not dates.
Peer docs: [POSITIONING.md](POSITIONING.md) (why this wedge), [ARCHITECTURE.md](ARCHITECTURE.md) (the system),
[RENDER-SCOPING.md](RENDER-SCOPING.md) / [PHASE3-SCOPING.md](PHASE3-SCOPING.md) / [PHASE4-SCOPING.md](PHASE4-SCOPING.md)
(the scoping specs).

Status tags: `[done]` shipped and CI-green, `[next]` the near-term queue, `[planned]` scoped or clearly shaped,
`[deferred]` intentionally later. Effort: S (hours), M (a day or two), L (a week or more).

## The through-line

The bet (see POSITIONING): the one universal, standardized control surface in audio is the PLUGIN parameter/state
API (VST3/AU). Sidechain owns DEEP, introspected, single-plugin control with a perception loop, which is the layer
no DAW-MCP or ML-as-DSP tool occupies. Everything below either deepens that wedge, ships it, or extends it to where
plugins live (in a DAW). Non-goal: becoming a DAW (arrangement, transport, timeline).

## Where we are (shipped)

- **Control plane (C0-C3).** `[done]` Multi-controller hosting, change-notification events, and a governed conflict
  tier (hierarchical edit leases, generation, crash-safe cleanup), all agent-reachable via MCP tools. See
  [CONCURRENCY.md](CONCURRENCY.md).
- **Semantic layer (Phases 1-3).** `[done]` Value-text inference (unit/range/curve/discrete), label-prefix
  sectioning, and a persistent per-fingerprint semantic store with agent-authored roles (`annotate_params`,
  `get_semantic_map`). The bridge holds no ontology; meaning lives in the agent and persists across sessions.
- **Perception: render + analysis (Tiers 1, 2, 2.5).** `[done]` Offline render on the message thread (no audio
  device); objective measures (peak/RMS/crest, spectral centroid, 3-band, silent/clipped); modulation-aware temporal
  analysis (`modulation` block with centroid/rms/pitch LFO rate + depth); phrase render (chords/arps, per-note
  expression, MPE); f0 pitch tracking (vibrato).
- **Autonomy: Phase 4 closed loop.** `[done]` `tune_param` (single-param coarse-seed + golden-section search) and
  `tune_params` (multi-param coordinate descent) drive a param toward a goal on a measure and converge it. "Make it
  brighter", "set the LFO to 6 Hz", "add vibrato", "punchier" are the same objective loop; the agent picks the knobs.
- **Foundation.** `[done]` Managed-host mode (`sidechain --plugin X.vst3`), a tag-triggered release pipeline
  (native matrix, smoke-gated), a 90% coverage floor, and a hardened CI (scoped `-Werror`, real-plugin integration
  across Surge/TAL/Dexed).

## Horizon 1: ship it and pay down the seam (near-term)

Release order: **`go:embed` single-file packaging lands BEFORE `v0.1.0`** so the first release ships as one binary,
not two version-matched artifacts. Everything else in this horizon can follow the tag.

- **Single-file distribution via `go:embed` (v0.1.0 PREREQUISITE).** `[done]` The `sidechain-host` is embedded into
  the Go binary (`internal/hostbin`, build tag `embedhost`, `scripts/build-embedded.sh`) and self-extracts to a
  cache dir at startup; managed-mode discovery uses it, so a shipped build is ONE file that needs no adjacent host.
  The release pipeline builds the single-file bundle (host embedded at package time), and the local packaging path
  is validated end to end (package -> unpack -> `--selftest`). This collapses the two-binary version-matching +
  bundling problem, the only cheaply-reversible part of the Go/C++ build cost (the native/JUCE/signing burden is
  inherent to self-hosting, so a pure-C++ rewrite would not fix it while discarding the proven, tested Go layer).
- **Cut `v0.1.0`.** `[next]` S. Prerequisite (`go:embed` single-file) is now done, so this is unblocked: the
  foundation is green, three capability layers are proven, and the release ships as a single embedded binary.
  Shipping makes it installable and dogfoodable, the fastest way to learn what matters next. Recommended next move.
- **Opaque-measurement forwarding.** `[next]` M. Today the Go side mirrors every DSP measurement field as a typed
  struct, so each new C++ measure forces a two-sided change plus a wire-contract sync. Make Go forward the
  `measurement`/`modulation` JSON more opaquely and only type the fields a tool reasons over. This cuts most of the
  recurring Go/C++ seam tax (see the cross-cutting note) so future audio work stops taxing the Go layer.
- **Intent playbook / knowledge layers.** `[next]` S to M. The first layers of the knowledge architecture in
  [KNOWLEDGE-SCOPING.md](KNOWLEDGE-SCOPING.md): synthesis-paradigm theory (recognize + reason about an unseen
  plugin), a cross-paradigm intent -> mechanism map, and interpretation heuristics, as retrievable data the agent
  consults (not ontology in the bridge). This is the embedded special knowledge that makes the agent behave like a
  producer, and the transparent, model-portable alternative to fine-tuning. The empirical per-plugin store (bottom
  layer) and the render/tune verifier already exist; reference signatures (a bridge to "taste") are the later
  addition.
- **Adjacent-projects landscape in POSITIONING.** `[next]` S. A layer map (anira = ML-as-DSP, midi2-hub =
  generation/sync/collab, per-DAW MCPs = arrangement, sidechain = the plugin-depth gap) so the positioning stays
  current as the "AI + audio" corner fills in.

## Horizon 2: deepen the wedge (mid-term)

- **Richer measures.** `[planned]` M each. LUFS (loudness intents done right), attack-time / transient (punchier
  done right), stereo width (the "wider" intent, needs per-channel not mono-sum). `tune_param`/`tune_params` pick
  each up for free once the measure exists. Best done after opaque-forwarding so they are C++-only additions.
- **Compatibility matrix.** `[planned]` M. Drive commercial plugins (Serum, Vital, Diva, FabFilter) to surface
  real-world breakage (state round-trips, sectioning, param quirks) and publish a support matrix. The actual product
  risk for a bring-your-own-licensed-plugin tool; proves the "any plugin" claim beyond the open-source set.
- **CLAP hosting.** `[planned]` M-L. Add the open, extensible plugin standard (Bitwig/Reaper/u-he/Surge/Vital ship
  it) alongside VST3/AU, widening the universal-plugin layer the whole thesis rests on. Idea reinforced by the
  landscape (midi2-hub targets CLAP).
- **f0 detector robustness.** `[deferred]` M. The frame-based pitch tracker smears for wide/fast vibrato (the
  `regular` flag can flip). A pitch-synchronous or shorter-frame method would tighten it; also a `modulation.pitch`
  hard real-host gate once detection is robust.

## Horizon 3: strategic bets (expand)

- **Bridge-plugin (the any-DAW wedge).** `[planned]` L. Ship the host wrapped as a VST3/AU/CLAP dropped onto a DAW
  track, hosting the target plugin inside it, so the agent drives the plugin chain in-DAW while the DAW does
  transport/arrangement/record. Every DAW hosts plugins, so this is universal with no per-DAW adapter (there is no
  universal DAW-control API, which is exactly why no "any-DAW MCP" exists). Architecturally close to today's host
  (already hosts a plugin and serves a control socket; the JUCE plugin wrapper is the delta), and a natural point to
  consolidate more logic in C++. Honest limit: a hosted plugin is sandboxed (no sibling plugins, no arrangement).
  Candidate for a `docs/BRIDGE-PLUGIN-SCOPING.md`.
- **Preset / state handoff.** `[planned]` M. "Save this tuned patch to a file the human or DAW can load." `save_state`
  and (optional) WAV export already exist; wiring the handoff is squarely on the differentiation axis (POSITIONING
  move 2: bridge with preset/state handoff rather than fight on arrangement).
- **Tier 3: plugin chain.** `[deferred]` L. Render a linear chain (synth -> effect -> measure) with per-node param
  addressing and a small multi-instance host. Graph routing (fan-out, true sidechain inputs) is a further step and
  out of scope. See RENDER-SCOPING Tier 3.
- **Phase 5 (open).** `[deferred]` The Phase arc is P1 inference, P2 sectioning, P3 store, P4 intent -> params (all
  done). P5 is undefined on purpose; the most likely shape is higher-level design assistance (goal -> a full patch
  via many tune loops + preset handoff), which composes the pieces above rather than adding a new primitive.

## Distribution and polish (as adoption warrants)

- **Signing / notarization** `[deferred]` M, **Homebrew tap** `[deferred]` S, **universal-mac binary** (`lipo` on
  macos-14) `[deferred]` S. Deferred until there is a `v0.1.0` and users who need frictionless install.

## Cross-cutting: the Go/C++ seam

The Go + C++ split was the right call for the server-heavy first phase (semantics, store, concurrency, GCF, MCP are
far cheaper and safer in Go). As the center of gravity moved into DSP (render, modulation, phrase, pitch), the split
now taxes every feature that crosses the audio/control boundary: two-sided changes, a pinned wire contract, and
CI-only compiler differences (a `<cstdint>` and a `-Wfloat-equal` slip this cycle, invisible to a local clang build).
The mitigation is explicitly NOT a pure-C++ rewrite: that would discard thousands of lines of proven, tested Go
(the MCP surface, semantic store, concurrency/governance, GCF), rewrite it in a language worse for that job, move
the network-facing parsing surface into manual memory, and still not fix distribution (the native/JUCE/signing cost
is inherent to self-hosting). The seam is paid down three cheaper ways instead: (1) opaque-measurement forwarding
(Horizon 1) removes most of the recurring mirror work; (2) `go:embed` single-file packaging (Horizon 1) collapses
the two-binary distribution cost; and (3) the bridge-plugin (Horizon 3) is C++-native, a natural point for more
logic to consolidate there while Go shrinks toward the MCP front. Track this so the tax is a deliberate choice, not
an accumulating drag.

## Non-goals

- **Not a DAW.** No arrangement, transport, timeline, or mixing. Sidechain controls plugins; the DAW arranges them.
- **No ML "does this sound good" judgment.** Objective measures only; the agent interprets them. (A learned
  perceptual measure could be added later as an offline analysis, but it is not on this roadmap.)
- **No reverse-engineering or plugin redistribution.** Bring your own licensed plugins.
- **No remote/network control.** Localhost-only bind, by design (a security posture).
