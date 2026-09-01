# Extraction Fidelity Review (2026-08-31)

Independent review of the extracted `mcp-vst-sidechain` implementation against its origin,
the live-control substrate in Aconite (`~/code/synthesizer`: `Source/McpControlServer.h`,
`mcp/catalog.go`, `mcp/live.go`, `mcp/gcf.go`, `mcp/tools.go`). Purpose: confirm the extraction
is faithful, clean, and generic before Aconite is wired to consume it.

## Verdict

Mostly faithful and clean, with **one real numeric divergence** to fix before integration.
Severity: **medium** (a fidelity bug, not a crash or data loss; cleanly fixable).

## The one finding that matters: skew-aware value conversion was dropped

- **Aconite original** converts normalized <-> real values through `RangedAudioParameter::convertFrom0to1()`
  / `convertTo0to1()`, which apply the parameter's `NormalisableRange` **skew** (the log/exp curves audio
  params use for frequency, gain, time). Evidence: `Source/McpControlServer.h:223` (`convertFrom0to1`), `:246`
  (`convertTo0to1`).
- **Extracted listener** replaced that with **linear** math: `valueForCatalog()` at `cpp/ControlListener.h:403-409`
  and linear index<->norm in `handleSetParam` (`:258-271`). The catalog host also enumerates ranges as linear
  (`cpp/Host.cpp:103-122`).
- **Effect:** for any skewed parameter, reported and set values diverge from the original. `set_param cutoff 500`
  on a log-skewed 20..20000 Hz range maps to a different normalized value under linear math than under the
  original skew.
- **Why it matters even for the dogfood:** Aconite's own params are `RangedAudioParameter` with skew, so
  Aconite-consuming-Sidechain would report its OWN params differently than Aconite's original. The equivalence
  gate (the point of the extraction) fails on skewed params until this is fixed. This is the subtle-drift
  pattern the verification discipline exists to catch.

### Fix (small, clean)

- In the C++ listener, prefer `RangedAudioParameter` conversion when the param has a `NormalisableRange`:
  `dynamic_cast<juce::RangedAudioParameter*>` and use `convertFrom0to1`/`convertTo0to1`; fall back to linear
  only when a hosted param genuinely exposes no range. This restores exact Aconite behavior AND handles most
  real plugins.
- Optionally carry a `hasRealRange` / skew flag in `ParamDef` so the Go layer knows whether `value` is real-unit
  or normalized (also resolves the ambiguous `value` semantics on the `LiveEndpoint` interface).
- Add a test with a log-skewed parameter to prove fidelity.

## Confirmed good

- Catalog math (`clampReal`/`normToReal`/`realToNorm`/`choiceIndex`/`roundHalfUp`) is **byte-for-byte identical**
  to Aconite's (`catalog.go` vs `synthesizer/mcp/catalog.go`).
- GCF encoding path identical (`gcf.go`), with the same JSON fallback.
- Error handling adequate: unknown-id checks, clamping, choice validation, closed-endpoint handling. No nil/panic
  gaps found.
- **No Aconite-specific coupling leaked** into the generic library (`server.go`, `paramtools.go`, `live.go` are
  plugin-agnostic; no synth-specific names/semantics).
- The new `AudioPluginFormatManager` child-plugin host (`cpp/Host.h`/`Host.cpp`) is sound, well-designed new code
  (Aconite has no such thing).

## Intentional scope reductions (NOT bugs, leave as-is)

These belong in Aconite, not the generic bridge:
- No `knowledge.go` / `describe_synth` (synth-specific documentation layer).
- No `.synthpreset` typed save/load (Sidechain offers only opaque `get_full_state`/`load_state` blobs).
- No generative composition tools (`set_scale`/`set_progression`/`set_arp`/`set_prob`/`render`/`export_midi`).

## Recommended actions before Aconite integration

1. Fix the skew conversion (above): prefer `RangedAudioParameter` conversion; linear fallback only.
2. Clarify `value` semantics on `LiveEndpoint` (real-unit vs normalized), ideally via a `hasRealRange` flag.
3. Test with a commercial VST3 that has log-skewed frequency/gain params to confirm fidelity.
4. Then wire Aconite as a pinned consumer and let the existing equivalence gates (`live_test.go` loopback,
   `mcp_test.go`, the C++ golden with listener off) verify equivalence.
