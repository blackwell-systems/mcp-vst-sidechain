# Render + analysis scoping: giving the agent ears

Status: design, not yet implemented. A buildable spec for offline rendering a hosted plugin and returning objective
measurements, so an agent can EVALUATE its own edits ("did it get brighter?") instead of tweaking blind. Peer docs:
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
new concurrency hazard is introduced. Before each render the processor is re-prepared (`prepareToPlay` resets DSP
state: envelopes, LFO phase, filter memory) so measurements are deterministic and independent of prior renders;
parameters are NOT reset by prepareToPlay, so the current patch is what gets measured.

## Design

### Tier 1: render + measure the single hosted plugin (the MVP)

Auto-detect instrument vs effect from the loaded processor (accepts MIDI and no audio input => instrument; has an
audio input => effect); allow an explicit override.

- **Instrument:** build a `MidiBuffer` with note-on at sample 0 and note-off at `gateMs`, render `durationMs` total
  (gate + tail), accumulate the output.
- **Effect:** synthesize an input signal (`silence | sine{freqHz} | noise | impulse`) into the input buffer, render
  `durationMs`, accumulate the output.

Then run the analyzer (Tier 2) on the accumulated output and return the measurement. This alone answers the
"synth -> read the meter" question without any meter plugin.

### Tier 2: the analysis set

A fixed, small, well-defined set computed from the rendered output (channels summed to mono unless noted):

```jsonc
{
  "durationSec": 2.0, "sampleRate": 48000, "channels": 2,
  "peakDb": -6.2,          // max sample, dBFS
  "rmsDb": -18.4,          // RMS level, dBFS
  "crest": 12.2,           // peak/RMS ratio, dB (transient-ness)
  "centroidHz": 1840.0,    // spectral centroid = "brightness"
  "bands": { "lowDb": -20.1, "midDb": -16.8, "highDb": -28.0 },  // <200 Hz / 200 Hz-2 kHz / >2 kHz energy
  "silent": false,         // below a small threshold (dead patch / no output)
  "clipped": false         // peak >= 0 dBFS
}
```

This covers the common intents: brighter (centroidHz / highDb up), louder (rms/peakDb up), punchier (crest up),
dead (silent), too hot (clipped). Centroid and bands use `juce::dsp::FFT`; peak/RMS/crest/silence/clip are trivial.

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

- **`render_and_measure`** (new). Input: `{ note?, velocity?, channel?, gateMs?, durationMs?, input?{kind,freqHz,level},
  analysis?[...], wav?bool|path }`. Live only (it needs the running plugin). Returns the measurement object plus a
  one-line summary ("peak -6.2 dBFS, RMS -18.4 dB, centroid 1.84 kHz, not clipped"). For an instrument `note`
  drives it; for an effect `input` provides the signal; auto-detected, override allowed.
- (Tier 3) `render_and_measure` gains an optional chain spec, or a separate `load_chain` tool defines the chain
  once and renders address it.

## C++ prerequisites

- A pure `analyze(const juce::AudioBuffer<float>&, double sampleRate) -> Measurement` function (peak/RMS/crest/
  silence/clip are JUCE-free; centroid/bands use `juce::dsp::FFT`). Kept separate so the JUCE-free parts are
  unit-testable standalone (the `section-derivation-test` pattern).
- A `Render` command on the ControlServer drain + a `PluginBridge::renderAndMeasure(spec)`; the JucePluginBridge
  implementation re-prepares, builds the MIDI or input buffer, loops `processBlock` over the duration, and analyzes.
  A new `render` wire verb carries the spec and returns the measurement; the Go `liveClient` and `LiveEndpoint`
  gain a `Render` method.

## Test plan

- **C++ analyzer unit test** (JUCE-free parts, standalone like `section_derivation_test.cpp`): feed synthetic
  buffers and assert: a full-scale sine -> rms about -3 dBFS, peak about 0 dBFS, `clipped` at exactly 0; silence ->
  `silent`; a louder signal -> higher rms. (The FFT-based centroid/bands are validated in the gated E2E below.)
- **Gated real-host E2E** (the payoff, per instrument): load Surge, set the filter cutoff LOW, render a note,
  record centroid; set the cutoff HIGH, render again, assert centroid INCREASED. This proves "brighter" is
  objectively measurable end to end, and is the canonical make-it-brighter loop. Add to `drive_plugin.sh` for
  MIDI-capable plugins.
- **Go tool-layer test** via the fake host: the fake returns a canned measurement for the `render` verb (as it does
  for `govern`), so the `render_and_measure` handler's parsing + summary formatting are covered headless. Audio
  correctness lives only in the gated E2E (the fake host has no DSP).

## Implementation sequence

1. C++ `analyze()` (pure) + its standalone unit test.
2. C++ render path (the `Render` command: prepare -> MIDI/signal -> processBlock loop -> analyze) in the bridge and
   ControlServer, plus the `render` wire verb.
3. Go: `liveClient.Render`, the `LiveEndpoint` method, and the `render_and_measure` MCP tool.
4. Gated E2E: cutoff-up-raises-centroid on Surge; wire into `drive_plugin.sh`.
5. Docs: fold the tool into ARCHITECTURE (tool table + wire protocol); mark this doc implemented.
6. (Later) Tier 3: multi-instance linear chain + per-node param addressing.

## Open decisions (yours to confirm)

1. **Effect input default:** `sine` (a fixed test tone, easy to reason about) vs `noise` (flat-ish excitation for
   spectral shaping) vs `silence`. Recommended default: `sine` at 220 Hz, with `noise` available for filter/EQ work.
2. **The analysis set:** is peak/RMS/crest/centroid/3-band/silent/clipped the right starting set, or do you want
   LUFS, a finer spectrum, or a transient/attack-time measure?
3. **Reset state per render:** recommended YES (deterministic, reproducible). Confirm you do not want stateful
   sequential renders.
4. **WAV export in v1 or defer:** recommended defer (the measurement is the core; WAV is the human-audition bridge,
   easy to add once the render engine exists).
5. **Tier 3 chain now or later:** recommended defer. Single-plugin render + measure is the high-value core and works
   on today's single-plugin host; the chain is a separate, larger change.
6. **Mono-sum vs per-channel measurement:** recommended mono-sum for the headline numbers, with per-channel a later
   option if stereo-field intents ("wider") appear.
