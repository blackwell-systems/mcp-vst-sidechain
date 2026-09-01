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

### Tests

- **AU load+drive smoke.** A gated `TestAULive` and a report-only integration step load an Apple built-in AU by
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
