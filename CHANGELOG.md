# Changelog

All notable changes to this project are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project aims to follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **`describe_param` tool.** Probes a live param's value text across its range and reports the recovered
  real-unit semantics: unit (Hz/dB/ms/%/semitones), real range, curve shape (linear/log/exp), whether it is
  bipolar, or the labels when it is really a discrete control. This is how an agent learns what a hosted param
  actually is before driving it.
- **Real-unit control.** `set_param` accepts `real=<n>` and `set_params` accepts a per-row `real`, letting an
  agent target real values (e.g. "cutoff = 1000 Hz") on a hosted plugin whose catalog range is a bare 0..1. The
  bridge maps the target through the param's probed value text. Closed-form (analytic) inversion is used when a
  linear or exponential model fits the samples well; otherwise a bounded binary search refines against live
  readbacks for exactness. Batch `real` uses the analytic inverse (no per-set search).
- **Full-patch state tools:** `save_state`, `load_state`, and `reset_init`, exposing the plugin's opaque state
  blob round-trip and the host reset hook over MCP (previously present in the wire protocol but unreachable).
- **`hasRealRange` and skew-aware conversion.** Native `RangedAudioParameter`s (an embedded JUCE plugin) now
  report real-unit endpoints and convert normalized↔real through the plugin's own `NormalisableRange` (skew
  included) instead of a linear approximation. Hosted VST3/AU params stay normalized, flagged
  `hasRealRange: false`.
- **Parameter grouping.** The host populates each param's group from the plugin's parameter tree (VST3 units /
  AU clumps) when the plugin exposes one; plugins with a flat tree leave it empty (unchanged).
- **Label-prefix sectioning for ungrouped plugins.** When a plugin reports no parameter groups (every param is
  "other"), `list_params` derives navigable sections from shared label prefixes (e.g. "Filter", "Amp", "Osc")
  so a large flat surface can be paged with `group=`. Real groups always win when present; the derivation is a
  catalog-level view and never mutates `ParamDef.Group` or the wire shape.
- **Set a discrete-hiding-as-float param by label.** `set_param choice=<name>` now works on a live hosted param
  that the catalog types as a plain float but that probes as a discrete control (a filter type, an on/off toggle
  whose value text renders labels like "LP"/"BP"/"HP" or "Off"/"On"). The bridge maps the label to a
  representative normalized position from the probe and reports whether the plugin's readback confirms it; an
  unknown label lists the observed labels. `get_param` surfaces the observed labels once a param has been probed.
- **Power-curve fit for zero-crossing curves.** Curve fitting adds a power model (real = A*norm^P) alongside
  linear and exp. This captures a knob that starts at zero and grows steeply (a time control 0 ms -> 32 s), which
  exp cannot fit (log undefined at the 0 endpoint) and linear fits poorly, so `set_param real=` inverts it in
  closed form instead of falling back to a binary-search refine. Unit-tested only: a survey of the plugins on
  hand found no plugin whose clean power-law params carry a real unit (the ones that fit render bare numbers), so
  there is no real-plugin power-fit E2E - see the note by `fitPower` in `infer.go` and `TestScanPowerFits`.
- **AU (AudioUnit) load by component identifier.** The host accepts an AU identifier
  (`AudioUnit:Effects/aufx,dcmp,appl` = type,subtype,manufacturer) in addition to a `.vst3`/`.component` path.
  On macOS an AU is resolved through the system AudioComponent registry rather than a path, so the identifier is
  the reliable handle (a raw `.component` path loads only when that component is also registered with the OS).

### Changed

- **Control-plane seam: `ControlServer` over a `PluginBridge` (C++).** The single `ControlListener` header is
  split into a VST-agnostic control plane and a plugin-specific bridge behind an abstract interface.
  `cpp/ControlServer.h` owns the transport, wire protocol, controller identity, the MPSC command queue,
  change-event broadcast, and the conflict tier, and depends only on `juce_core` + `juce_events` (no
  `juce_audio_processors`): it drives a plugin solely through `cpp/PluginBridge.h`. `cpp/JucePluginBridge.h` is the
  concrete bridge that hosts a real VST3/AU (the only class that touches `juce::AudioProcessor`: parameter maps,
  real<->normalized curves, opaque state, MIDI, the parameter-change listener). This makes the whole
  transport/protocol/identity/event substrate reusable for anything controllable, not just a JUCE plugin: to
  expose something else over the same protocol, implement `PluginBridge` and reuse `ControlServer` unchanged. Pure
  refactor, behavior-preserving: validated locally against Surge XT across the full gated suite plus the C1
  (`TestMultiClientLive`) and C2 (`TestChangeNotifications`) concurrency tests. See `docs/ARCHITECTURE.md`.
- **Multi-controller host (concurrency C1).** The C++ control server now serves MANY connections at once
  instead of one client at a time. Each connection runs on its own handler thread and gets a `clientID` at the
  ping handshake; commands flow through a mutex-guarded multi-producer queue drained by the single message thread
  (still the only applier of every mutation, so the audio path and correctness are unchanged); and each request
  carries its own completion object, replacing the single shared applied-event and scratch slots that assumed one
  client. Multiple agents can now drive one plugin instance concurrently. Validated by the gated
  `TestMultiClientLive` (distinct identities, concurrent independent control, a state read during a 300-set
  hammer, clean per-connection disconnect). See `docs/CONCURRENCY.md`.
- **Change-notification events + multiplexed async protocol (concurrency C2).** The host now registers as an
  `AudioProcessorParameter::Listener` and pushes a `param_changed` event (`{param, normalized, value, text, by}`)
  to every connected controller whenever a param moves, whatever the source: another controller, the plugin's own
  editor, or host automation. So a change by one controller becomes visible to the others without polling. The
  broadcast runs on the message thread when the change originates there (attributed to the applying controller via
  `by`) and is deferred through an atomic dirty flag + `triggerAsyncUpdate` when it arrives off-thread, so the
  audio path never carries it. The wire protocol becomes multiplexed async: replies correlate to requests by `id`
  (now load-bearing), server-pushed events arrive unsolicited, and each connection has a single-writer outbound
  queue so a slow reader cannot stall the applier. The Go `liveClient` gained a background reader that
  demultiplexes replies (by `id`) from events (exposed on `Events()`), replacing the old one-reply-per-request
  read. Conflict policy remains last-writer-wins in message-thread order; `gsm` is noted as the C3
  governed-convergence engine for the small invariant-bearing coordination state. Validated by the gated
  `TestChangeNotifications` (controller A sets a param, controller B receives the attributed event). See
  `docs/CONCURRENCY.md`.
- **The host emits derived sections in the catalog (`section`), and owns sectioning.** Each catalog row now carries
  a `section` field (alongside the raw `group`): the EFFECTIVE navigable section, which is the param's tree group
  when it has one, else a section DERIVED from shared label prefixes, else `"other"`. A new `cpp/Sectioning.h`
  (`computeSections`) is the single source of truth on the host for this, used by BOTH the catalog emitter and the
  C3 governed layer's leasable sections (collapsing what had been three separate tree walks / derivations into
  one). The Go catalog now PREFERS the host-emitted `section` for its effective-group view (`list_params group=`,
  `Groups`/`Filter`), so sectioning is computed once on the host rather than re-derived in Go; the Go derivation
  (`sections.go`) is retained only as a fallback for catalogs without `section` and as the reference oracle. A new
  gated `TestSectionLockstep`, run by `drive_plugin.sh` for every hosted plugin, asserts the host's emitted
  sections equal the Go reference per param, so the two implementations are provably in lockstep in CI (validated
  on Surge grouped = 774 params and TAL flat/derived = 89 params).
- **Label-prefix sectioning ported to the host for flat plugins.** A plugin with no parameter-tree groups (a flat
  parameter list) exposes leasable governed sections derived from shared label prefixes (a faithful C++ port of the
  Go catalog's `sections.go`: labels tokenized on non-alphanumeric runs with pure-number tokens dropped, a prefix
  promoted to a section when at least three params share it, each param taking its longest qualifying prefix).
  Verified to match the Go derivation exactly on TAL-NoiseMaker's real labels (Amp, Delay, Envelope, Filter, Osc,
  Reverb, ...), so an agent paging a flat plugin by section can lease that same section. (Now folded into
  `computeSections`, above.) A report-only Linux CI leg drives TAL's derived section leases end to end.
- **Section leases bind to the plugin's param groups.** A governed section lease is now taken on a real param
  group by name (`govern{op:acquire_section, group:"Filters"}`) rather than an abstract index. `PluginBridge`
  gained `sectionGroups()` (JucePluginBridge walks the parameter tree for the distinct groups, first-seen order);
  `ControlServer` binds the leasable sections from it at construction and refuses a `govern` on an unknown group,
  and `get_governed` returns the leasable group list plus the held section leases as `{group: holder}`. The C++
  `GovState` now keys section leases by group name (`std::map<string,int>`), so any of a plugin's groups is
  leasable (Surge exposes 14; a flat plugin exposes none, leaving only the instance lease). The enumerated Go
  model keeps its fixed representative scopes as the proof, now joined by `TestGovSectionsIndependent` which makes
  executable the per-scope-independence argument that lets the small enumeration cover any number of named groups.
  Validated by `TestGovernedLive` driving Surge's real groups.
- **C3 governed coordination state wired into the message-thread drain.** The conflict tier (a hand-rolled model
  in `governed.go` with an exhaustive-enumeration proof in `governed_test.go`) is ported to C++
  (`cpp/GovernedState.h`) and wired into `ControlServer`: new `govern{op,...}` and `get_governed` wire commands
  flow through the same MPSC queue as `set_param`, `GovState::apply` runs on the single applier alongside the LWW
  param path (so the check-then-commit is atomic), and a governed change is broadcast to every controller as a
  `governed_changed` event (change-notification parity with C2). The schema models the real multi-controller
  coordination concerns, not the plugin's musical state: **hierarchical edit leases** (a whole-instance lease and
  per-section leases, where taking the whole instance revokes others' section leases (compensate) and taking a
  section of, or the whole of, an instance held by another is refused (reject) - so concurrent editors do not
  fight over the same knobs); a **patch generation** counter bumped on a whole-patch change (`load_state` /
  `reset_init`) so an agent can detect the base moved under it; and **disconnect cleanup** (a departing or crashed
  controller's leases are released automatically, the invariant that makes leases safe). Each command resolves as
  **applied / compensated / rejected**. The continuous plugin params are untouched and stay last-writer-wins.
  `governed_test.go` stays the executable invariant proof, kept in lockstep with the C++ port. Validated by the
  gated `TestGovernedLive` (hierarchical acquire/reject/compensate + generation bump, observed cross-controller)
  and `TestGovernedDisconnectFreesLease` (a holder disconnects, its lease frees, another acquires). See
  `docs/CONCURRENCY.md`.
- **`LiveEndpoint` interface:** `SetParam(id, v, isReal)` distinguishes a real-unit value from a normalized one;
  added `SampleText` (the value-text probe primitive).
- **Denser default probe:** the value-text sweep uses 21 uniform points for a better curve read and seed.
- Tool and type documentation corrected so `set_param`/`get_param`/`ParamDef` describe hosted-plugin values as
  normalized 0..1 (real units only when `hasRealRange`), rather than always "real units."
- README gained a **Supported formats** section (VST3/AU scope; VST2 reachable only via an external VST3
  wrapper); `docs/ARCHITECTURE.md` updated to match the `LiveEndpoint` signature.

### Fixed

- **Live-control socket race:** the request/response round-trip is now serialized by a mutex on the client, so
  concurrent tool calls (for example a held `play_note` and a `set_param`) can no longer interleave bytes on the
  one control socket. The note verbs previously released the session lock before their socket call.
- **Plugin paths with spaces:** the host CLI now resolves paths like `.../Surge XT.vst3` correctly (JUCE wraps
  space-containing args in quotes when it reconstructs the command line; those quotes are now stripped).
- **Opaque state is now format-agnostic and byte-exact.** `save_state`/`load_state` carry base64 of the exact
  bytes the plugin's `getStateInformation` produced, instead of assuming XML-wrapped state (`getXmlFromBinary` /
  `copyXmlToBinary`). A plugin with raw-binary state (common in commercial plugins) previously failed
  `save_state` outright; and the XML re-serialization round-trip was lossy. The bridge now round-trips the raw
  bytes verbatim, so state save/recall works for XML-wrapped and raw-binary plugins alike. Wire field renamed
  `xml` -> `state`.

### Tests

- **C3 governed-state model stub + exhaustive-enumeration verification (`governed.go`, `governed_test.go`).** A
  first, illustrative cut of the small discrete coordination state a future C3 conflict tier would govern (an
  exclusive-edit lease, a voice-mode gate, a panic/playback latch), kept separate from the continuous plugin
  params (which stay last-writer-wins). It is the `reduce`/`ok`/`repair` shape from `docs/CONCURRENCY.md` plus the
  conflict tier `apply` (resolving each command as applied / compensated / rejected, with lease acquisition as a
  guarded reject and the value/mode/gate commands compensating). Verified the way the sequencing note prescribes:
  a DFS over (reachable states x command alphabet) asserts the invariant after every `apply` (reproducing gsm's
  build-time "no reachable state violates an invariant" guarantee as an ordinary test), plus repair totality and
  idempotence over a widened, deliberately malformed product space, plus targeted reject/compensate checks. Not
  wired into the live control path: scaffolding so the model and its proof exist before the first real
  multi-controller conflict. A gated `TestAULive` and a report-only integration step load an Apple built-in AU by
  identifier, enumerate its catalog, and drive it over the control socket (ping + get_param + set_param +
  read-back), closing the AU gap (everything prior exercised VST3 only). Report-only for now; promotable to a
  required CI step once confirmed green headlessly.
- **Power-fit survey.** A gated `TestScanPowerFits` probes every param on a running host and flags any clean
  analytic power fit, used to hunt for a real plugin that exercises the power model. Finding recorded (none of
  the surveyed plugins expose a power-law param the real-unit set path can drive).
- **Generic full-surface live suite.** Four gated, catalog-driven E2E tests that work against ANY hosted plugin
  (no plugin-specific ids): `TestFullSurfaceSweep` sets every param to normalized {0, 0.5, 1} and reads each
  back, proving the whole control surface is drivable without a crash; `TestStateRoundTrip` snapshots, mutates,
  saves, loads, and asserts restoration, proving the opaque save/load path; `TestBatchSetParams` applies N rows
  in one `set_params` and asserts applied N / skipped 0; `TestMidiSmoke` plays a held middle-C then panics.
  Gated on `SIDECHAIN_SWEEP_PORT` + `SIDECHAIN_SWEEP_CATALOG` (MIDI additionally on `SIDECHAIN_SWEEP_MIDI=1`, so
  it runs only for instruments). Validated locally against Surge XT (774 params) and TAL-NoiseMaker (89 params).

### Changed

- **Integration workflow drives multiple plugins on macOS + Linux.** The single-plugin drive is refactored into
  a shared `drive_plugin` helper (`.github/scripts/drive_plugin.sh`) that starts the host, waits for the
  catalog, and runs the generic gated suite. macOS keeps Surge XT VST3 as the hard-required leg (plus the
  Surge-only capability tests) and adds a report-only Surge XT Effects VST3 leg (a NON-synth, run without MIDI).
  A new report-only Linux job builds the host and drives Surge XT, Surge XT Effects, TAL-NoiseMaker, and Dexed
  VST3 under `xvfb-run`. Everything new is `continue-on-error` until proven green on CI, mirroring the existing
  AU/staticcheck report-only pattern.

### Docs

- **`docs/CONCURRENCY.md`** (new, design/contract): elevates concurrency to a first-class product property. States
  the per-layer invariants (audio thread lock-free and sacred; the message thread is the single applier of every
  mutation; control-plane locks are fine; the semantic store is per-fingerprint files with no global lock), and
  the target model of multiple concurrent controllers of one plugin instance (many connections feeding a
  multi-producer queue drained by the one message thread; change-notification events for cross-controller
  visibility; client identity; last-writer-wins). Includes the contract invariants, a phased path (C0..C3), and a
  concurrency test category. Design only; C0 (single controller) is today's reality.
- **`docs/PHASE3-SCOPING.md`** (new, design only): a buildable spec for the planned persistent semantic store.
  Resolves the open decisions (fingerprint-as-equivalence for cache reuse, storage, lifecycle, invalidation),
  folds in the two-axis equivalence model (a derived behavior-class signature plus soft agent-authored role
  strings with a suggested vocabulary rather than an enforced ontology), names the one C++ prerequisite (emit
  plugin identity in the catalog), and defines the tool schemas and test plan. Not yet implemented.
- **`docs/TESTING.md`** (new): documents the testing approach: the four layers (pure unit, fake-host loopback,
  in-memory MCP transport, gated real-plugin E2E), how to run each locally (including the gated env vars), the
  three CI workflows and the required-vs-report-only plugin/OS matrix, the conventions learned the hard way
  (disconnect discipline, state-first round-trip, promote-on-actual-result), and how to add a plugin.
- **`docs/ARCHITECTURE.md`** expanded into a comprehensive component/wire-protocol/tool-surface/data-flow
  document (base64 opaque state, the full tool set, the Phase-1 semantic layer).
- `docs/EXTRACTION-REVIEW.md` corrected: reframed the finding as real-unit-vs-normalized (skew being the visible
  part), scoped the fix to native ranged params (hosted commercial plugins stay normalized by necessity), fixed
  the self-contradicting "test with a commercial VST3" recommendation, and noted the fix has been applied.

## [0.1.0] - 2026-08-31

### Added

- Initial release: generic MCP bridge that hosts any VST3/AU plugin (C++/JUCE host + Go MCP server), exposing a
  plugin's parameter catalog and realtime control to an agent, with GCF-encoded large payloads (`6b29aec`).

### Docs

- Extraction fidelity review documenting the skew-conversion divergence and its fix (`e264462`).
- README: dropped the Prior art section (`df3cf15`); kept the VST trademark disclaimer under License
  (`f8291d9`); smoothed the prose, de-jargoned and tightened (`cd6a337`); removed the roadmap section
  (`e20dfed`).
