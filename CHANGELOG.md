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
