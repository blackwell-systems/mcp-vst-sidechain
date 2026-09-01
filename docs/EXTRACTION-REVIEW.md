# Extraction Fidelity Review (2026-08-31)

Independent review of the extracted `mcp-vst-sidechain` implementation against its origin,
the live-control substrate in Aconite (`~/code/synthesizer`: `Source/McpControlServer.h`,
`mcp/catalog.go`, `mcp/live.go`, `mcp/gcf.go`, `mcp/tools.go`). Purpose: confirm the extraction
is faithful, clean, and generic before Aconite is wired to consume it.

## Verdict

Mostly faithful and clean, with **one real numeric divergence** to fix before integration.
Severity: **medium** (a fidelity bug, not a crash or data loss; cleanly fixable).

> **Resolution (applied after this review).** The divergence below is fixed. The C++ listener and catalog host
> now route continuous native params through `RangedAudioParameter::convertFrom0to1`/`convertTo0to1`
> (skew-aware); `ParamDef` carries a `hasRealRange` flag; the Go layer forwards real values for those params and
> normalized 0..1 for everything else; and a `RangedAudioParameter`-backed regression test covers it. See
> `cpp/ControlListener.h` (`valueForCatalog`, `handleSetParam` value branch), `cpp/Host.cpp` (`enumerateCatalog`),
> and `paramtools.go` (`liveArg`).

## The finding that matters: real-unit conversion (of which skew is the visible part) was dropped

- **Aconite original** converts normalized <-> real values through `RangedAudioParameter::convertFrom0to1()`
  / `convertTo0to1()`, which apply the parameter's `NormalisableRange` (including the **skew** the log/exp
  curves use for frequency, gain, time). Evidence: `Source/McpControlServer.h:223` (`convertFrom0to1`), `:246`
  (`convertTo0to1`).
- **Extracted listener** replaced that with **linear** math: `valueForCatalog()` at `cpp/ControlListener.h:403-409`
  and linear index<->norm in `handleSetParam` (`:258-271`). The catalog host also enumerates ranges as linear
  (`cpp/Host.cpp:103-122`).
- **The divergence is real-unit-vs-normalized, not only skew.** The extracted base-API path reports the
  *normalized* 0..1 as `value` for every continuous param, where the origin reported the *real* value. Skew is
  the most visible symptom, but even a **non-skewed** ranged param with a non-0..1 range diverges: a linear
  `-60..+12 dB` gain reports `value` as 0..1 here vs dB in the origin. Framing the fix as "restore skew" alone
  would miss that; the correct fix restores real-unit conversion, and skew comes along because JUCE's
  `NormalisableRange` owns both.
- **Effect:** `set_param cutoff 500` on a log-skewed 20..20000 Hz range maps to a different normalized value
  under linear math than under the plugin's real curve; and `get_param` reports a normalized number where the
  origin reported 500.
- **Why it matters for the dogfood:** Aconite's own params are `RangedAudioParameter`, so
  Aconite-consuming-Sidechain would report its OWN params differently than Aconite's original. The equivalence
  gate (the point of the extraction) fails on those params until this is fixed. This is the subtle-drift
  pattern the verification discipline exists to catch.

### Scope: the fix only fires for native (ranged) params, and that is correct

The conversion fix helps exactly the case that has a curve to honour: native `RangedAudioParameter`s (Aconite's
own params, or an in-plugin embed). For the project's **headline** use case - hosting a closed-source commercial
VST3/AU - the params arrive as `HostedAudioProcessorParameter`, which is **not** a `RangedAudioParameter`. The
`dynamic_cast` returns null and those params stay on the normalized path *by necessity*: the base hosted API
exposes no real scalar, only normalized 0..1 plus `getText`. That is not a fidelity loss - you drive a hosted
plugin via normalized 0..1 and it applies its own internal curve; the human units live in `text`. So linear
enumeration is a bug **only** for the ranged/own-plugin case, which is precisely what the equivalence gate
covers. `hasRealRange` makes the distinction explicit on both sides.

### Fix (small, clean) - now implemented

- In the C++ listener, prefer `RangedAudioParameter` conversion when the param exposes a `NormalisableRange`
  (`convertFrom0to1`/`convertTo0to1`); fall back to linear/normalized only for hosted params with no range.
- Carry a `hasRealRange` flag in `ParamDef` so the Go layer knows whether `value` is real-unit or normalized,
  and forward real values only for those params. This is not optional polish: it is what makes the `value`
  field honest across both cases and resolves the ambiguous `value` semantics on the `LiveEndpoint` interface.
- Add a regression test built on a **skewed `RangedAudioParameter`** (not a commercial VST3 - see below) to
  prove fidelity.

## Confirmed good

- Catalog math (`clampReal`/`normToReal`/`realToNorm`/`choiceIndex`/`roundHalfUp`) is **logically identical**
  to Aconite's (`catalog.go` vs `synthesizer/mcp/catalog.go`). Not literally byte-for-byte: `clampReal` dropped
  a two-line explanatory comment in extraction; every line of executable math matches.
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

1. ~~Fix the real-unit conversion~~ **(done)**: native ranged params now convert through the plugin's own
   `NormalisableRange`; hosted params keep the normalized path.
2. ~~Clarify `value` semantics~~ **(done)** via the `hasRealRange` flag, plumbed C++ -> catalog -> Go and
   reflected in the `set_param`/`get_param` tool descriptions.
3. **Test the fix with a synthetic skewed `RangedAudioParameter`, not a commercial VST3.** A commercial VST3's
   params are `HostedAudioProcessorParameter` (no `NormalisableRange`), so they never exercise the fixed
   conversion path - they validate the hosted/normalized fallback instead. To prove the *skew* fix, a unit test
   must point the listener at a native param with a log skew and assert real<->norm round-trips through the
   curve. (A commercial VST3 is still worth running, but as a check of the hosted path and catalog quality, not
   of the skew conversion.)
4. Then wire Aconite as a pinned consumer and let the existing equivalence gates (`live_test.go` loopback,
   `mcp_test.go`, the C++ golden with listener off) verify equivalence.
