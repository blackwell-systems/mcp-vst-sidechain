# Render + analysis scoping: giving the agent ears

Status: IMPLEMENTED (Tier 1 render + Tier 2 static analysis + Tier 2.5 modulation-aware / temporal measurement).
Tier 3 (plugin chain) is designed and deferred. Offline-renders a hosted plugin and returns objective measurements,
so an agent can EVALUATE its own edits ("did it get brighter?", "is it wobbling at the right rate?") instead of
tweaking blind. Peer docs:
[ARCHITECTURE.md](ARCHITECTURE.md) (the system), [POSITIONING.md](POSITIONING.md) (why this is on our side of the
wall a DAW server cannot reach), [PHASE3-SCOPING.md](PHASE3-SCOPING.md) (the store this measures against).

## Goal

Today the agent is deaf: it sets parameters, and a human or a DAW hears the result. The host controls a plugin's
parameter/state surface but never processes audio (no `AudioDeviceManager`, no `processBlock`, no buffers). This
adds the missing half of autonomous sound design:

- Render a note (or a test signal through an effect) through the hosted plugin, OFFLINE, and measure the output.
- Return a compact, objective measurement set the agent can reason over: level, brightness, dynamics, silence.
- Close the perception loop: "make it brighter" becomes verifiable (spectral centroid rose), not a blind guess.

This is also the feedback signal a future Phase 4 (intent -> params) needs: propose params, render, measure,
iterate. It lives entirely on our side of the architectural wall (we host the plugin, so we can render and measure
it; a DAW MCP server cannot hand an agent that).

## Non-goals

- **No realtime audio output.** We do not open an audio device or play to speakers. The human/DAW does that; we
  render offline and MEASURE. Offline render is deterministic and headless, and fast (a few seconds of audio is
  milliseconds of CPU, not realtime seconds).
- **No perceptual/ML "does this sound good" judgment.** We return objective measures; the agent interprets them.
- **No reliance on a meter PLUGIN.** We measure the rendered buffer ourselves. Depending on a third-party meter to
  expose its reading as a parameter is unreliable (many expose it only via their GUI, the "GUI-only is invisible"
  boundary). Sidechain IS the meter.
- **No graph routing (fan-out, sidechains) in v1.** A linear chain is a scoped extension (Tier 3); the MVP is a
  single plugin.

## Threading and safety

Sidechain has no realtime audio thread, and the message thread is the single applier of every mutation. So the
render runs ON the message thread, as a bounded blocking Command (like `get_full_state`): `processBlock` is called
from the one thread that also applies parameter sets, so a render and a `set_param` are naturally serialized and no
new concurrency hazard is introduced. Before each render the processor's DSP state is reset with `reset()` (clears
envelopes, LFO phase, filter memory, reverb tails) so measurements are deterministic and independent of prior
renders, while the current edited patch is exactly what gets measured.

IMPLEMENTATION PITFALL (found during real-host validation, do not regress): reset the DSP with `reset()`, NOT with
a `releaseResources()` + `prepareToPlay()` cycle. That cycle makes the hosted plugin rebuild its DSP from its
patch/default state and DROPS the parameter values pushed in via `set_param`, so every render comes out identical
to the default patch regardless of edits (a filter-cutoff change had ZERO audible effect until this was fixed). The
host already prepared the processor once at load, at the same rate/block, so no re-prepare is needed.

## Design

### Tier 1: render + measure the single hosted plugin (the MVP)

Auto-detect the stimulus from the loaded processor: anything that accepts MIDI is note-driven, otherwise it is a
pure effect. Key on `acceptsMidi()` ALONE, NOT `acceptsMidi() && numInputs == 0`: many synths ALSO expose an
audio-input bus (Surge XT reports 2 inputs for its audio-in oscillator / vocoder), so gating on zero inputs
misclassifies them as effects and renders silence (no note sent). Found during real-host validation.

- **Note-driven:** build a `MidiBuffer` with note-on at sample 0 and note-off at `gateMs`, render `durationMs`
  total (gate + tail), input left silent, accumulate the output.
- **Pure effect (no MIDI):** synthesize an input signal (`silence | sine{freqHz} | noise | impulse`) into the input
  buffer, render `durationMs`, accumulate the output.

Then run the analyzer (Tier 2) on the accumulated output and return the measurement. This alone answers the
"synth -> read the meter" question without any meter plugin.

### Tier 2: the analysis set

A fixed, small, well-defined set computed from the rendered output (channels summed to mono unless noted). The
host emits it over the control socket in snake_case (the wire is snake_case throughout); the Go `render_and_measure`
tool re-encodes it as camelCase in its MCP StructuredContent. The wire shape:

```jsonc
{
  "duration_sec": 2.0, "sample_rate": 48000, "channels": 2,
  "peak_db": -6.2,          // max sample, dBFS
  "rms_db": -18.4,          // RMS level, dBFS
  "crest": 12.2,            // peak/RMS ratio, dB (transient-ness)
  "centroid_hz": 1840.0,    // spectral centroid = "brightness"
  "bands": { "low_db": -20.1, "mid_db": -16.8, "high_db": -28.0 },  // <200 Hz / 200 Hz-2 kHz / >2 kHz energy
  "silent": false,          // below a small threshold (dead patch / no output)
  "clipped": false          // peak >= 0 dBFS
}
```

This covers the common intents: brighter (centroid / high band up), louder (rms/peak up), punchier (crest up),
dead (silent), too hot (clipped). Centroid and bands use `juce::dsp::FFT`; peak/RMS/crest/silence/clip are trivial
and live in the JUCE-free `cpp/RenderAnalysis.h` (unit-tested standalone).

### Tier 2.5 (IMPLEMENTED): modulation-aware (temporal) measurement

**The problem.** Tier 2 collapses the whole render to one number per metric. That is exactly right for a static
patch, and exactly WRONG for anything time-varying: an LFO, a slow filter sweep, tremolo, auto-pan, a moving
envelope. A cutoff wobbling at 2 Hz and a static cutoff at the wobble's mean produce nearly the same
centroid-over-the-whole-buffer, so the Tier-2 feedback signal is blind to modulation by construction. Intents like
"add movement", "make it wobble", "vibrato", "a slow evolving pad" cannot be verified or tuned against a
time-collapsed measurement. This is the single measurement gap that keeps the render loop from reaching modulation.

**The primitive: a per-frame series, not a buffer summary.** Run the SAME offline render (a held note for a synth,
a sustained input for an effect), but analyze it in short frames (default ~25 ms hop) instead of collapsing it.
That yields a short time series for the metrics that can move: `centroid_hz[t]` (a filter LFO / sweep shows here)
and `rms_db[t]` (tremolo / a volume LFO shows here). From each envelope, derive modulation descriptors:

- `rate_hz`: the dominant periodicity of the envelope, from the autocorrelation (or FFT) of the detrended frame
  series. This is the LFO rate.
- `depth`: the excursion of the envelope over the render (centroid range in Hz, rms range in dB) = how deep the
  modulation is.
- `regular`: periodic (a clean LFO) vs irregular (sample-and-hold, noise, drift), from the sharpness of the
  autocorrelation peak.
- `confidence`: how trustworthy the estimate is (see the render-length guardrail below).

**Wire shape (opt-in).** Temporal analysis is off by default (it adds a frame series and only matters for
time-varying patches). A request opts in with `temporal` fields; the reply gains a `modulation` block alongside the
Tier-2 summary:

```jsonc
// request adds:  { "temporal": true, "frame_ms": 25 }   // frame_ms optional, default 25
// reply adds:
"modulation": {
  "frame_ms": 25,
  "centroid": { "rate_hz": 2.03, "depth": 1840.0, "regular": true,  "confidence": 0.91 },  // Hz excursion
  "rms":      { "rate_hz": 0.0,  "depth": 1.2,    "regular": false, "confidence": 0.20 },  // dB excursion
  "dominant": "centroid"   // which signal carries the strongest modulation (or "none")
}
```

Reporting BOTH `centroid` and `rms` means we do not need to know a priori WHAT the LFO modulates: a filter LFO lands
in `centroid`, a tremolo in `rms`, and `dominant` flags the stronger. (A pitch LFO / vibrato needs a pitch-track
envelope, `f0_hz[t]`, a later addition; centroid picks up most timbral movement in the meantime.)

**Render-length guardrail.** Periodicity needs several cycles to estimate: a 0.5 Hz LFO needs multiple seconds. The
render duration must cover at least ~3 cycles of the slowest rate of interest, so `durationMs` should scale with the
target rate (or the host bumps it and notes it). Below that, `confidence` is low and `rate_hz` is reported as a
best-effort estimate, never as a false precision.

**How it closes the loop for modulation.** Once `modulation` exists, LFO RATE and DEPTH become ordinary scalars
that `tune_param` optimizes: "vibrato near 6 Hz" is `tune_param(lfoRate, measure=modulation.centroid.rate_hz,
goal=target, target=6)`; "deeper wobble" is `tune_param(lfoDepth, measure=modulation.centroid.depth,
goal=maximize)`. The parts that are NOT a 1-D continuum stay where they belong: the LFO WAVEFORM (a discrete choice)
and the DESTINATION/ROUTING (a discrete selector or a mod-matrix cell) are set directly by the agent via
`set_param choice=`, driven by the recipe it holds ("vibrato = LFO -> pitch, ~6 Hz, low depth") over the semantic
map. A compositional intent like "add a slow wobble" becomes the agent SEQUENCING 1-D tunes (set destination +
waveform, tune rate, tune depth, render and verify periodicity) rather than any new multi-D optimizer on the server.

**Where it stays hard (honest limits).** This measures modulation; it does not DISCOVER the mod routing. Where a
plugin exposes its LFO wiring only through its GUI (not as automatable params), it stays out of reach, the same
GUI-only wall as everything else. Where routing IS a param, the mapping "param X is LFO1 rate, Y is LFO1->cutoff
depth, Z selects the destination" varies wildly per plugin and is rarely recoverable from value text, so it is
exactly the hard-won, per-plugin structural knowledge the Phase-3 semantic store is there to persist: the agent
works out a synth's modulation topology once, `annotate_params` records those roles, and later sessions recall it.

**Implementation notes.** The frame loop reuses the render buffer; per-frame RMS is trivial and per-frame centroid
reuses the existing FFT path at the frame hop. The envelope statistics (range, autocorrelation, peak sharpness)
are pure and belong in the JUCE-free `cpp/RenderAnalysis.h` next to the Tier-2 core, unit-testable against
synthetic envelopes (a 2 Hz sine envelope -> `rate_hz ~ 2`, `regular`; a ramp -> `rate_hz ~ 0`; noise ->
`!regular`). Non-goals: full modulation-matrix discovery, per-sample automation capture, and pitch tracking (a
later `f0_hz[t]` addition for vibrato).

**As-built refinements (found in real-host validation, do not regress).** Two corrections were needed once this ran
against an actual LFO (TAL-NoiseMaker's Lfo 1 -> filter cutoff), and both matter:

- **Analyze the SUSTAIN window, not the whole render.** The note's own amplitude envelope (the attack transient and,
  for an instrument, the release decay to silence after note-off) is a huge one-shot excursion that otherwise swamps
  the LFO: the first attempt reported centroid depth ~2600 Hz and rms depth ~120 dB with `regular=false` on a
  routed LFO. The bridge now frames only `[attackGuard, noteOff]` for an instrument (a ~200 ms attack guard;
  release excluded) and `[attackGuard, end]` for a pure effect (sustained excitation), falling back to the whole
  buffer when there is too little sustain to window. After this, the same patch reads centroid depth ~1200 Hz,
  `regular=true`, `dominant=centroid`.
- **Smooth the envelope before autocorrelation.** Per-frame centroid jitter autocorrelated at very short lags and
  reported a spurious near-frame-rate "LFO" (~13 Hz, `regular`) on UNMODULATED patches. A light 3-tap moving average
  on the envelope (a real LFO sits far below the frame rate) removes the jitter; unmodulated patches now correctly
  report `dominant=none`, `regular=false`. Depth is measured on the smoothed envelope so frame noise cannot inflate
  it.

The decision signal for a consumer is `dominant` + the per-signal `regular` flag (not the raw `rate_hz`, which is a
best-effort estimate even when there is no real periodicity). Validated end to end: `TestRenderTemporalLive`
(a routed LFO reads `regular` at an LFO-band rate) and `TestTuneModulationRateLive` (tune the LFO rate param toward
a target rate: 3.6 Hz -> 4.0 Hz), both REQUIRED in CI against TAL.

### Tier 3 (deferred): a linear plugin chain

Load N instances and process them in series (output of A -> input of B), measuring the final output: synth ->
effect -> measure. Requires per-node parameter addressing (namespace param ids by chain position) and a small
multi-instance host, so it is a scoped follow-up, not the MVP. Graph routing (fan-out, true sidechain inputs) is a
further step and explicitly out of scope here.

### Optional: WAV export

The render may also write the output to a WAV file and return its path (audio never crosses MCP; the blob is a file
on disk). This is the bridge to the human perception loop and to the DAW: the agent renders a preview, the human
auditions it, or it becomes a starting asset. Off by default.

## Tool surface

- **`render_and_measure`** (shipped). Input (all optional, camelCase in the MCP schema): `{ note, velocity, channel,
  gateMs, durationMs, inputKind, inputFreq, inputLevel }`. Live only (it needs the running plugin). Returns the
  camelCase measurement object plus a one-line summary ("peak -6.2 dBFS, RMS -18.4 dB, centroid 1.84 kHz, not
  clipped"; a distinct "dead patch?" line when silent). Anything that accepts MIDI is note-driven (`note`/`velocity`/
  `gateMs`); a pure effect is fed `inputKind`; auto-detected. WAV export and an analysis-subset selector are not
  implemented (they were optional in this spec).
- (Tier 3, deferred) `render_and_measure` gains an optional chain spec, or a separate `load_chain` tool defines the
  chain once and renders address it.

## C++ prerequisites

- A pure `analyze(const juce::AudioBuffer<float>&, double sampleRate) -> Measurement` function (peak/RMS/crest/
  silence/clip are JUCE-free; centroid/bands use `juce::dsp::FFT`). Kept separate so the JUCE-free parts are
  unit-testable standalone (the `section-derivation-test` pattern).
- A `Render` command on the ControlServer drain + a `PluginBridge::renderAndMeasure(spec)`; the JucePluginBridge
  implementation resets the DSP state (`reset()`, see the pitfall above), builds the MIDI or input buffer, loops
  `processBlock` over the duration, and analyzes. A new `render` wire verb carries the spec and returns the
  measurement; the Go `liveClient` and `LiveEndpoint` gain a `Render` method.

## Test plan

- **C++ analyzer unit test** (JUCE-free parts, standalone like `section_derivation_test.cpp`): feed synthetic
  buffers and assert: a full-scale sine -> rms about -3 dBFS, peak about 0 dBFS, `clipped` at exactly 0; silence ->
  `silent`; a louder signal -> higher rms. (The FFT-based centroid/bands are validated in the gated E2E below.)
- **Gated real-host E2E** (the payoff): `TestRenderBrighter` sets the filter cutoff LOW, renders a note, records
  the centroid; sets the cutoff HIGH, renders again, asserts the centroid INCREASED. This proves "brighter" is
  objectively measurable end to end (the canonical make-it-brighter loop). CI runs it against TAL-NoiseMaker's
  Filter Cutoff (~10x centroid swing), NOT Surge XT: Surge's init-patch filter is Off, so its cutoff is inaudible
  without patch setup. `TestRenderSmoke` (a bare render returns a non-degenerate measurement) runs per plugin via
  `drive_plugin.sh`.
- **Go tool-layer test** via the fake host: the fake returns a canned measurement for the `render` verb (as it does
  for `govern`), so the `render_and_measure` handler's parsing + summary formatting are covered headless. Audio
  correctness lives only in the gated E2E (the fake host has no DSP).

## Implementation sequence (all DONE except Tier 3)

1. [done] C++ `analyzeMono()` (pure, `cpp/RenderAnalysis.h`) + its standalone unit test.
2. [done] C++ render path (the `Render` command: reset -> MIDI/signal -> processBlock loop -> analyze) in the bridge
   and ControlServer, plus the `render` wire verb and the FFT centroid/3-band in the JUCE bridge.
3. [done] Go: `liveClient`/`LiveEndpoint.Render` and the `render_and_measure` MCP tool.
4. [done] Gated E2E: `TestRenderBrighter` (TAL cutoff raises centroid) + `TestRenderSmoke` wired into
   `drive_plugin.sh`.
5. [done] Docs: folded into ARCHITECTURE (tool table + wire protocol + render section); this doc marked implemented.
6. [done] Tier 2.5: modulation-aware measurement (per-frame series -> `modulation` block) so LFO rate/depth are
   measurable and tunable; the primitive that makes time-varying intents legible to the loop. Includes the
   sustain-window and envelope-smoothing refinements above; proven by `TestRenderTemporalLive` +
   `TestTuneModulationRateLive` against TAL.
7. [deferred] Tier 3: multi-instance linear chain + per-node param addressing.

## Decisions (as shipped)

1. **Effect input default:** `sine` at 220 Hz; `noise`/`impulse`/`silence` also available via `inputKind`.
2. **The analysis set:** peak/RMS/crest/centroid/3-band/silent/clipped shipped (all static, whole-buffer). LUFS, a
   finer spectrum, and a transient/attack-time measure are possible later additions. The biggest gap is TEMPORAL:
   the static set is blind to modulation (LFOs, sweeps, tremolo), which Tier 2.5 addresses with a per-frame series
   and a `modulation` block (rate/depth/regular). See Tier 2.5 above.
3. **Reset state per render:** YES, via `reset()` (deterministic; see the pitfall about NOT using release/prepare).
4. **WAV export:** deferred (the measurement is the core; WAV is the human-audition bridge, easy to add later).
5. **Tier 3 chain:** deferred. Single-plugin render + measure is the high-value core and works on today's host.
6. **Mono-sum vs per-channel:** mono-sum for the headline numbers; per-channel is a later option for stereo-field
   intents ("wider").
