# Sidechain

**An agent's control input for any plugin. One MCP endpoint, every VST.**

`mcp-vst-sidechain` is a generic [MCP](https://modelcontextprotocol.io) bridge that hosts any VST3/AU plugin
and exposes its parameters to an AI agent in realtime. Point it at your licensed Serum, Diva, or Kontakt (or a
free effect), and through a single MCP endpoint an agent can enumerate the plugin's full automatable control
surface, read and set any parameter live (by real units like "1000 Hz", even on plugins that only expose a raw
0..1), play notes, save or recall its complete state, and build up a durable map of what each parameter means.
Several agents can drive one instance at once without fighting over the same controls.

Crucially, the agent can also **hear**: it renders the plugin offline, measures the result objectively (brightness,
loudness, dynamics, modulation, even vibrato), and tunes parameters toward an intent on its own. So "make it
brighter" is not a blind guess, it is a closed loop that renders, measures the spectral centroid, and converges the
filter until the sound actually got brighter.

Sidechain talks to plugins only through the standard VST3/AU API, the same way a DAW hosts thousands of
closed-source commercial plugins without ever seeing their source. You bring your own licensed binaries;
nothing is patched and nothing is redistributed.

> Status: early but working, and deep. The Go MCP layer, the C++ control server, and the child-plugin host (the
> JUCE `AudioPluginFormatManager` wrapper) are tested end to end: an integration matrix drives real VST3/AU plugins
> on macOS and Linux. Multiple agents can control one instance at once (with edit leases so they do not collide); a
> semantic layer recovers real-unit control from probe-only plugins and persists the learned meaning across
> sessions; and an offline render + analysis loop lets the agent measure and autonomously tune its own edits
> ("brighter", "louder", "add a 6 Hz vibrato") with the closed loop proven in CI on real plugins. No release is
> tagged yet.

## Why

Producers already automate plugins in a DAW. An LLM should be able to do the same thing conversationally:
"open the filter a little," "make this pad wider," "give me a darker version of this patch." That needs a
generic, realtime control bridge between an agent and an arbitrary plugin. Sidechain is that bridge, and for the
intents that have an objective signal (brighter, louder, punchier, more vibrato) it closes the loop today: it
renders, measures, and tunes until the target is hit, rather than nudging a knob and hoping.

There's no polished, standalone tool for this yet. The existing attempts are prototypes, DAW-specific
controllers, or heavy agent-DAWs; none is a cross-platform generic bridge, and none handles the token cost of
the huge parameter surfaces real plugins expose. Sidechain deliberately does NOT try to be a DAW: it owns deep,
semantic control of a single instrument or effect, the sub-task DAW tools serve worst. See
[docs/POSITIONING.md](docs/POSITIONING.md) for the landscape and where this is differentiated, and
[docs/ROADMAP.md](docs/ROADMAP.md) for what is shipped and what is next.

### Built on GCF for large parameter sets

A plugin with hundreds of parameters is expensive to serialize into an agent's context as JSON. Sidechain
encodes the large `list_params` output as [GCF](https://github.com/blackwell-systems/gcf-go), a wire format
50-92% smaller than JSON that frontier models read with no examples. `set_params` also accepts a GCF table, so
authoring a whole patch is one compact call instead of one per parameter.

## How it works

Two layers, two languages, chosen for what each does best:

```
  agent  <--stdio MCP-->  Sidechain server (Go)  <--localhost TCP-->  host (C++/JUCE)  --hosts-->  VST3/AU
```

- **C++ / JUCE host** wraps one plugin via `AudioPluginFormatManager` and runs a `ControlServer` (the
  VST-agnostic control plane) pointed at the hosted processor through a `PluginBridge`. JUCE is the mature,
  industry-standard plugin-hosting stack with real macOS AU support.
- **Go MCP server** speaks stdio JSON-RPC to the agent and forwards control over a localhost socket. It uses
  the official MCP go-sdk, ships as a single binary, and GCF-encodes large payloads via `gcf-go`.

Audio never crosses the process boundary. Parameter control runs at agent speed (tens of ms), not audio rate,
so the localhost IPC carries only control messages and has zero effect on the sound.

The control server accepts **many controllers at once**: several agents (or an agent alongside the plugin's own
editor) can drive one instance concurrently, each with its own identity, and every change is pushed to the others
as an event. Full detail: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md), [docs/CONCURRENCY.md](docs/CONCURRENCY.md).

## The closed loop: the agent hears and tunes

Controlling parameters is half of sound design; the other half is judging the result. Sidechain gives the agent
both.

- **`render_and_measure` (its ears).** Sidechain renders the hosted plugin OFFLINE (no audio device, no realtime):
  a MIDI note through an instrument, or a test signal through an effect. It then measures the output objectively:
  peak / RMS / crest, spectral centroid (brightness), a three-band split, silence and clipping. Opt into temporal
  analysis and it also reports MODULATION: the rate and depth of an LFO on the filter (centroid), the amplitude
  (tremolo), or the pitch (vibrato). You can drive a whole PHRASE (a chord, an arp, a short sequence), not just one
  note, so patches that only reveal themselves under a pattern are measurable too. Nothing else in the MCP audio
  space can do this, because it needs to HOST the plugin and process its audio, which is exactly what Sidechain
  does and a DAW-side controller cannot.
- **`tune_param` / `tune_params` (autonomy).** Give the tool a parameter and a target ("maximize the spectral
  centroid", "hit -12 dB RMS", "set the LFO rate to 6 Hz") and it renders + measures at each step, converging the
  value with a bounded search. `tune_params` co-optimizes several knobs at once for intents one knob cannot express
  ("punchier" = attack + drive toward more crest; "wobble at 4 Hz, deep" = LFO rate toward a target AND amount
  toward more depth). The agent decides WHAT to tune from the semantic map; the server converges it. This is the
  make-it-brighter loop, run to completion by the machine and proven end to end in CI on real plugins.

The division of labor is deliberate: the meaning of an intent ("brighter" -> the filter cutoff, up) lives in the
agent, and the objective search lives in the server. See [docs/RENDER-SCOPING.md](docs/RENDER-SCOPING.md) and
[docs/PHASE4-SCOPING.md](docs/PHASE4-SCOPING.md).

## Supported formats

Sidechain hosts **VST3** (all platforms) and **AU** (macOS) instruments and effects through JUCE's standard
`AudioPluginFormatManager`, the same hosting path a DAW uses. That is the whole format surface, by design: it
keeps the project buildable from source with no proprietary SDK. In testing it loads a real 774-parameter VST3
synth (Surge XT), enumerates the full surface, and drives it live over the control socket.

**VST2 is not hosted.** The Steinberg VST2 SDK has been unlicensed since 2018 and cannot be redistributed, so
adding it would break the "build from source, standard APIs only" guarantee. To drive a VST2-only instrument
(e.g. Synth1), wrap it into a VST3 with an external adapter and point Sidechain at the resulting `.vst3`; the
bridge itself never sees VST2. Wrapper fidelity varies, so treat that route as best-effort.

A hosted plugin exposes its parameters as normalized 0..1 values plus the plugin's own value text
(`hasRealRange: false`); the real-unit range and skew live inside the plugin. Sidechain's semantic layer recovers
them anyway: `describe_param` probes the value text across the range to infer the unit, real range, and curve, so
an agent can then `set_param real=1000` (Hz) on a plugin whose catalog range is a bare 0..1. Real endpoints are
reported directly (`hasRealRange: true`) only when Sidechain is embedded in a native JUCE plugin that owns the
curve. See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Tools

Roughly grouped: **read** (`list_params`, `get_param`, `describe_param`), **learn** (`annotate_params`,
`get_semantic_map`, `forget_semantics`), **write** (`set_param`, `set_params`), **live/MIDI/state** (`connect_live`,
`play_note`, `save_state`, `load_state`, `reset_init`), **coordinate** (`acquire_lease`, `get_leases`,
`poll_events`), and **hear + tune** (`render_and_measure`, `tune_param`, `tune_params`).

| Tool | What it does |
|---|---|
| `list_params` | The plugin's automatable parameters (id/label/type/range/choices/default). GCF-encoded. |
| `get_param` | One parameter's definition + current value (real units + normalized). |
| `describe_param` | Probe a live param's value text across its range and report the recovered semantics: unit (Hz/dB/ms/%/...), real range, curve shape, whether it is bipolar, or the labels when it is really a discrete control, plus a derived behavior class. Recalls from the semantic store without re-probing once a param has been seen (even headless). |
| `annotate_params` / `get_semantic_map` | Teach the bridge what params MEAN (a stable role like `filter.cutoff`, aliases, polarity) and read the whole plugin's semantic map back. Persisted per plugin (by fingerprint) across sessions. |
| `forget_semantics` | Drop the stored semantics for the current plugin. |
| `set_param` | Set one parameter by real units (`real=`, even on a hosted plugin whose catalog range is a bare 0..1: the value maps through the probed curve), normalized 0..1, or choice name. Validated + clamped. |
| `set_params` | Set many params in one call, from a JSON array or a token-compact GCF table. |
| `connect_live` / `disconnect_live` | Attach to / detach from a running host so the above drive the live instance. |
| `play_note` / `all_notes_off` | Play a MIDI note (optionally auto-released) / panic. |
| `save_state` / `load_state` | Snapshot the whole patch as one opaque blob / recall it. Round-tripped through the plugin's own state, never inspected. |
| `reset_init` | Reset the running plugin to its init/default patch. |
| `acquire_lease` / `release_lease` | Claim (or release) an exclusive edit lease on a param-group section, or the whole instance, so multiple agents driving one plugin don't fight over the same knobs. Returns applied/compensated/rejected. |
| `get_leases` | Show the current edit leases, the leasable sections, the patch generation, and your own controller id. |
| `poll_events` | What changed on the plugin since your last poll: parameter and lease changes made by other controllers. |
| `render_and_measure` | Offline-render the current patch (a MIDI note for anything that accepts MIDI, else a synthesized test signal for an effect) and get back objective measurements: peak/RMS/crest, spectral centroid, a three-band split, silent/clipped, plus a one-line summary. Lets an agent EVALUATE its own edit ("did it get brighter?") instead of tweaking blind. |
| `tune_param` | Drive one param toward a goal (maximize/minimize, or hit a target) on one measurement, rendering and measuring at each step until it converges. You pick the param and measure (e.g. cutoff + centroid); the tool finds the value that gets there and reports the trace. The autonomous "make it brighter" loop. |
| `tune_params` | Co-optimize several params at once (coordinate descent) for intents one knob can't express: "punchier" (attack + drive toward more crest), or "wobble at 4 Hz, deep" (LFO rate toward a target + amount toward more modulation depth). Give a list of {param, measure, goal}; the tool tunes them together and reports where each landed. |

## Quickstart

### 1. Build both binaries

```bash
go build -o sidechain ./cmd/sidechain
cmake -S cpp -B cpp/build -DCMAKE_BUILD_TYPE=Release
cmake --build cpp/build --target sidechain-host
```

(The first CMake configure fetches JUCE via FetchContent, so it is slow. VST3 hosting works everywhere; AU is
macOS only.)

### 2. Run it (managed mode: one command)

Point `sidechain` at a plugin and it spawns and supervises the host for you, waits for the catalog, auto-connects
the live endpoint, and serves MCP over stdio:

```bash
./sidechain \
    --plugin "/Library/Audio/Plug-Ins/VST3/YourSynth.vst3" \
    --host-bin ./cpp/build/sidechain-host_artefacts/Release/sidechain-host
```

Register that command as an MCP server in your client (Claude Code / Desktop). Because it is already live, the
agent can go straight to `list_params` (see the surface), `set_param` / `set_params` (drive it), and
`render_and_measure` / `tune_param` (hear and tune it), no `connect_live` needed. Add `--selftest` to verify the
whole managed startup in one shot without serving MCP.

### External mode (attach to a running host)

For advanced setups (a long-lived host, several servers attaching to one instance), run the host yourself and
attach with `--catalog`:

```bash
# terminal 1: start the host, which writes the catalog and listens on :51703
./cpp/build/sidechain-host_artefacts/Release/sidechain-host \
    --plugin "/Library/Audio/Plug-Ins/VST3/YourSynth.vst3" --catalog plugin-catalog.json --port 51703

# terminal 2: attach the MCP server; the agent then calls connect_live to dial :51703
./sidechain --catalog plugin-catalog.json
```

## Configuration

| Variable / flag | Default | Meaning |
|---|---|---|
| `--catalog` / `SIDECHAIN_CATALOG` | - | Parameter catalog JSON (emitted by the host). Required by the Go server. |
| `--plugin` (host) | - | Path to a `.vst3` or `.component` to load. |
| `--port` (host) / `connect_live` port | `51703` | Loopback control port. |
| `SIDECHAIN_MCP_FORMAT` | `gcf` | `json` forces plain-JSON tool output instead of GCF. |
| `--semantic-dir` / `SIDECHAIN_SEMANTIC_DIR` | per-user cache dir | Where learned param semantics persist (per-plugin files), so a probe/annotation is paid once and reused across sessions. |

## Using the library

The Go layer is importable. Any host that can provide a `ParamCatalog` and a `LiveEndpoint` gets the generic
tool surface for free:

```go
import sidechain "github.com/blackwell-systems/mcp-vst-sidechain"

srv := mcp.NewServer(&mcp.Implementation{Name: "my-bridge"}, nil)
sidechain.RegisterParamTools(srv, myCatalog, func() sidechain.LiveEndpoint { return myEndpoint })
// ...register your own tools on srv too; they compose on one server.
```

The C++ side is split at a seam: `sidechain::ControlServer` is the VST-agnostic control plane (transport,
protocol, identity, change events, command queue), and it drives a plugin only through a `sidechain::PluginBridge`.
`sidechain::JucePluginBridge` is the bundled bridge that hosts a `juce::AudioProcessor` (construct it with a
`juce::AudioProcessor&` + `juce::MidiKeyboardState&`, hand it to a `ControlServer` with a port). Implement your
own `PluginBridge` to expose anything else over the same wire protocol.

## License

MIT. Copyright (c) 2026 Dayna Blackwell. The realtime control substrate and Go MCP forwarder here originated in
the author's own synth work and are published as a standalone, generic OSS bridge.

"VST" is a trademark of Steinberg Media Technologies GmbH, used here descriptively for compatibility, not to
claim affiliation.
