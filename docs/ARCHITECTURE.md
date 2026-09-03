# Architecture

Sidechain is a two-layer, two-language system. The split is deliberate.

```
  ┌──────────────┐   MCP (stdio / JSON-RPC)     ┌─────────────────────────────┐
  │  LLM agent   │ <──────────────────────────> │  Sidechain server (Go)      │
  │ (Claude etc.)│   GCF-encoded payloads       │  • MCP server (go-sdk)      │
  └──────────────┘                              │  • GCF encode/decode (gcf-go)│
                                                └─────────────┬───────────────┘
                                                              │ localhost TCP
                                                              │  line-delimited JSON
                                                              │  (CONTROL messages only)
                                                              ▼
                                                ┌─────────────────────────────┐
                                                │  host (C++ / JUCE)          │
                                                │  message thread:            │
                                                │     setParameter / state    │
                                                │  ─────────────────────────  │
                                                │  audio thread (realtime):   │
                                                │     process()  no GC/alloc  │  ── audio out ──▶
                                                └─────────────┬───────────────┘
                                                              │ hosts the binary (no source)
                                                              ▼
                                                ┌─────────────────────────────┐
                                                │ target VST3 / AU plugin     │
                                                └─────────────────────────────┘
```

## Why two languages

- **Plugin host / realtime wrapper: C++ (JUCE).** The audio thread must be realtime-safe (no GC pauses, no
  allocation on the hot path), and the VST3/AU SDKs are C++. JUCE's `AudioPluginFormatManager` hosts VST3/AU
  natively and cross-platform, with proper macOS AU support and decades of compatibility with real commercial
  plugins. This is the mature, industry-standard plugin-hosting stack.
- **MCP / control layer: Go.** It is not realtime-critical (stdio JSON-RPC plus tool dispatch), the official
  MCP go-sdk is a good fit, it ships as a single static binary, and `gcf-go` handles GCF encoding natively.

**Audio never crosses the process boundary.** Parameter control runs at control rate (agent speed, tens of ms),
not audio rate, so the localhost IPC carries only control/state messages and has zero effect on audio quality.
The decision is "two binaries vs one," not "clean audio vs compromised."

## Component map

Four components sit between the agent and the plugin. The C++ host is a separate process; the Go server is a
separate process; they meet on one localhost socket. Three seam interfaces keep the design generic: on the C++
side `PluginBridge` (what the control plane can drive) with `ControlServer` as the plane above it; on the Go side
`ParamCatalog` + `LiveEndpoint`.

```
  ┌──────────────────────────── C++ host process (sidechain-host) ────────────────────────────┐
  │                                                                                            │
  │  main.cpp ──▶ sidechain::Host                                                              │
  │                 • load()            AudioPluginFormatManager: VST3 path / AU by identifier │
  │                 • enumerateCatalog() walks getParameters() -> catalog JSON (to a file)     │
  │                 • startControl()     JucePluginBridge + ControlServer on 127.0.0.1:<port>  │
  │                                                                                            │
  │               sidechain::ControlServer  (a juce::Thread + juce::AsyncUpdater)              │
  │                 many socket threads   parse a line, snapshot reads, enqueue mutations      │
  │                 MPSC queue ──▶ message thread  bridge.applyParam / state / MIDI            │
  │                 sidechain::JucePluginBridge   the plugin-specific half (params/state/MIDI) │
  └────────────────────────────────────────────┬───────────────────────────────────────────┘
                                                │  localhost TCP, line-delimited JSON
  ┌─────────────────────────────────────────────┴──────────────── Go server process (sidechain) ┐
  │                                                                                              │
  │  cmd/sidechain/main.go ──▶ Run() ──▶ NewServer()                                             │
  │                                                                                              │
  │     Catalog  (ParamCatalog)      loaded from the catalog JSON; identity + validate + sections│
  │     session                      catalog + live endpoint + infer cache + semantic store     │
  │     liveClient  (LiveEndpoint)   the async wire client (reply demux + change-event channel)  │
  │     registerParam/Live/Governed/Semantic Tools   the MCP tool surface on one *mcp.Server     │
  │     SemanticStore (semantic.go)  per-fingerprint persistent semantics; GCF for big reads     │
  └──────────────────────────────────────────────────────────────────────────────────────────┘
```

### The C++ host (`sidechain::Host`, `cpp/Host.h` / `cpp/Host.cpp`)

A headless child-plugin host. It loads one plugin, enumerates its catalog, and points a `ControlServer` (through
a `JucePluginBridge`) at the hosted processor. `main.cpp` is a thin CLI that drives it and pumps a JUCE message loop.

- **Loader.** `Host::load` registers the platform formats (`addDefaultFormats`: VST3 everywhere, AudioUnit on
  macOS), asks each format to describe the binary via a `KnownPluginList` scan, and instantiates the first
  described type through `createPluginInstance`. A shell file that hosts several sub-plugins yields the first.
  The instance is prepared at a fixed rate/block (48 kHz, 512) and all buses are enabled.
- **AU identifier vs filesystem path.** `load` accepts either a `.vst3`/`.component` path OR an AudioUnit
  component identifier of the form `AudioUnit:Synths/aumu,dls ,appl` (type,subtype,manufacturer). The identifier
  is the reliable AU handle on macOS: an AU is resolved through the system AudioComponent registry
  (`AudioComponentFindNext`), not by scanning a path. A raw `.component` path only loads when that component is
  also registered with the OS. The path form is validated with a file-exists check; the identifier form skips it
  (there is no file to stat).
- **Catalog enumeration (`enumerateCatalog`).** Walks the loaded plugin's `getParameters()` on the **base**
  `AudioProcessorParameter` API (not `RangedAudioParameter`) and emits the catalog JSON the Go server reads. The
  base API is used deliberately: a hosted VST3/AU exposes its parameters as `HostedAudioProcessorParameter`,
  which is not a `RangedAudioParameter`, so a ranged cast would drop every hosted param. Each row carries
  `id`, `label`, `group`, `section`, `type`, `min`, `max`, `step`, `default`, `hasRealRange`, and (for choices)
  `choices`. The root object also stamps `stateRootTag`, `stateVersion`, `count`, and a `plugin` identity object
  (`name`/`manufacturer`/`format`/`version`/`uniqueId`, from the loaded `juce::PluginDescription`) that keys the
  Phase-3 semantic store's fingerprint. Only automatable params are emitted.
- **Sectioning (`group` vs `section`, `cpp/Sectioning.h`).** `group` is the raw parameter-tree group: VST3 units
  and AU clumps surface as nested `AudioProcessorParameterGroup`s, and enumeration records each param's immediate
  parent group name (empty for a flat tree). `section` is the EFFECTIVE navigable section: that group when present,
  else one DERIVED from shared label prefixes ("Filter Cutoff"/"Filter Resonance" -> "Filter"), else `"other"`.
  `computeSections` is the single source of truth for this on the host - the same computation the C3 governed layer
  uses for its leasable sections - so the host, not the Go side, derives sections; the Go catalog prefers the
  emitted `section` and keeps its own derivation (`sections.go`) only as a fallback and as the reference oracle the
  gated `TestSectionLockstep` cross-checks the host against on every plugin.
- **Type inference from the base API.** A param that `isDiscrete()` with value-strings becomes a `choice` (with
  the strings as `choices`); a boolean or a 2-step param becomes `bool`; any other discrete param becomes `int`;
  everything else is `float`.
- **Native vs hosted parameter handling (`hasRealRange`).** A native `RangedAudioParameter` (an embedded JUCE
  plugin, or Sidechain's own params) owns a real-to-normalized curve, including the skew that frequency, gain,
  and time controls use. When a param is both ranged and continuous (a `float`), `enumerateCatalog` reports its
  **real** endpoints and default through the plugin's own `NormalisableRange` and sets `hasRealRange: true`. A
  hosted continuous param has no such scalar, so it reports `0..1` and `hasRealRange: false`. A discrete param
  (choice/int/bool) reports its index range `0..(steps-1)` on either path (skew is a continuous-value concern).

### The control-plane seam: `ControlServer` + `PluginBridge` (`cpp/ControlServer.h`, `cpp/PluginBridge.h`, `cpp/JucePluginBridge.h`)

The live-control substrate is split at an interface so the transport/protocol/identity/event machinery is
reusable beyond a JUCE plugin. `ControlServer` is the **VST-agnostic control plane**: it owns the socket
listener, the wire protocol, controller identity, the command queue, change-event broadcast, and the conflict
tier, and it depends only on `juce_core` + `juce_events` (no `juce_audio_processors`). It drives a plugin only
through a `PluginBridge`. `JucePluginBridge` is the **plugin-specific half**: the one class that touches
`juce::AudioProcessor` (parameter maps, real<->normalized curves, opaque state, MIDI, the parameter-change
listener). To expose anything else over the same protocol, implement another `PluginBridge`.

- **Thread model (the load-bearing safety rule).** The accept loop runs on a `juce::Thread`; each connected
  controller gets its own handler thread. A handler never touches parameter or DSP state directly (that would
  race `processBlock`). It only: parses a line of JSON; for **reads**, asks the bridge (safe from any thread,
  parameter atomics) and replies directly; for **mutations**, resolves the target through the bridge, pushes a
  `Command` into a mutex-guarded MPSC queue, fires an `AsyncUpdate`, and waits (bounded) on its own per-request
  `Completion` for the message thread to apply it before acking. The message thread drains the queue
  (`handleAsyncUpdate`) and applies each command via `bridge.applyParam` / note verbs / state calls: exactly the
  on-screen-keyboard / GUI-edit path, so the editor and the host see the change and it serializes normally.
- **The MPSC command queue.** Many socket handlers produce; the single message thread consumes. A short
  control-plane mutex guards a `std::deque<Command>` (bounded at 4096); the message thread is the sole applier,
  so every mutation serializes there. This is the conflict tier: **last-writer-wins in message-thread order**, and
  where a governed-convergence engine (gsm) can slot in at C3. See [CONCURRENCY.md](CONCURRENCY.md).
- **Per-request completion, bounded apply-then-ack.** Each blocking command carries a heap-allocated `Completion`
  (a `WaitableEvent` plus result slots), shared between the requesting handler and the message thread so a bounded
  timeout can never free it under a late apply. `set_param` waits up to 250 ms, `reset_init` 500 ms, state verbs
  2 s; the ack therefore means "applied," giving the agent clean request/response semantics.
- **Many controllers, identity, EOF handling.** The server binds `127.0.0.1` only and serves many connections at
  once; each is assigned a `clientID` at the ping handshake. A handler's read loop waits for readability first,
  then reads: a ready socket that returns 0 bytes is EOF, so the handler returns and the connection is removed
  cleanly without stalling others (invariant 7). Each connection has a single-writer `Outbound` queue so replies
  and broadcast events never race on one socket.
- **Change events (C2).** `JucePluginBridge` registers as a `juce::AudioProcessorParameter::Listener` and calls
  `ControlServer::onParamChanged(index)` on any change (a controller, the plugin's editor, host automation). The
  server broadcasts a `param_changed` event to every connection: directly when the change is on the message thread
  (attributed to the applying controller via `by`), or deferred through an atomic dirty flag + `triggerAsyncUpdate`
  when it fires off-thread, keeping the audio path lock-free.
- **Value semantics (in the bridge).** `JucePluginBridge` maps a normalized 0..1 to the catalog `value`: for a
  continuous native (ranged) param, the real value via the plugin's own skew-aware curve; for a discrete param,
  the index `0..(steps-1)`; for a hosted continuous param, the normalized value itself. The set forms
  (`normalized` / `choice` / `value`) invert through the same curve in `resolveSet`.
- **The single host hook.** `JucePluginBridge::onResetInit` is a nullable `std::function<void()>` run on the
  message-thread drain for `reset_init`. Null means a no-op ack. It is the one host-specific extension point.

### The Go server (`server.go`, `paramtools.go`, `live.go`, `catalog.go`)

- **`NewServer` / `Run`.** `Run` loads the catalog JSON (failing fast if it is empty or corrupt: a server with
  no catalog cannot validate or clamp anything) and serves stdio JSON-RPC. `NewServer` builds one `*mcp.Server`,
  registers the generic param tools and the live verbs against a single `session`, and returns both so a caller
  can drive it in-process (tests, or embedding in another Go host).
- **The `session`.** One per process. It holds the `ParamCatalog`, a headless param map (real-unit values keyed
  by id, used when not connected live), the current `LiveEndpoint` (nil means headless), and the per-session
  inference cache (`infer`, keyed by param id). A single mutex serializes tool calls, which is also what keeps the
  line-delimited control protocol from interleaving.
- **The semantic store (`semantic.go`, Phase 3).** When attached, the session's `infer` is backed by a persistent
  store: a directory of per-fingerprint JSON files (`sha256(name|manufacturer|format|sortedParamIDs|count)`), keyed
  by the catalog's `plugin` identity. A probe is written through (atomic temp+rename, field-level merge) so a
  future session recalls it instead of re-sweeping; `annotate_params` accumulates the agent's own role/aliases/
  polarity per param. Each param carries a derived behavior class (`float:log:hz`) and a free-form role
  (`filter.cutoff`) on two orthogonal axes. Store dir via `--semantic-dir` / `SIDECHAIN_SEMANTIC_DIR`. See
  [PHASE3-SCOPING.md](PHASE3-SCOPING.md).
- **The `Catalog`.** The read side: indexed by id for O(1) lookup, with the pure param math (`clampReal`,
  `normToReal`, `realToNorm`, `choiceIndex`, `roundHalfUp`). It also computes an **effective-group** view once at
  construction (see the semantic layer, sectioning). `ParamDef.Group` and the wire shape are never mutated by the
  view.
- **GCF.** The big read tool (`list_params`) encodes its model-facing payload as GCF (see below).
- **Render + analysis (`render_tools.go`).** Registers `render_and_measure`; the `LiveEndpoint.Render` client
  method carries the `RenderSpec` over the wire and decodes the snake_case `Measurement` reply. The Phase-4
  feedback signal (see the render section under the wire protocol).
- **Closed-loop tuning (`tune_tools.go` + `tune_params.go`, Phase 4).** Registers `tune_param` (a bounded coarse-seed
  + golden-section search on one param, `tuneAxis`) and `tune_params` (coordinate descent over several knobs, reusing
  `tuneAxis` per axis), using `set_param` + `Render` as the objective. Pure mechanism (no intent ontology); the agent
  picks what to tune from the semantic map. See `docs/PHASE4-SCOPING.md`.

### The seam interfaces

**C++ side, `sidechain::ControlServer` over a `sidechain::PluginBridge`.** The server is the VST-agnostic plane;
the bridge is what it drives. For a JUCE plugin, hand a `JucePluginBridge` (built from any `juce::AudioProcessor`
plus a `juce::MidiKeyboardState`) to a `ControlServer` with a port:

```cpp
sidechain::JucePluginBridge bridge (targetProcessor, keyboardState);
bridge.onResetInit = []{ /* optional host-supplied init/default recall */ };
sidechain::ControlServer server (bridge, /*port*/ 51703);   // bridge must outlive server
```

The bridge walks `getParameters()` for the id catalog, snapshots atomic values for reads, resolves set targets
through the plugin's own curve, and applies every mutation on the message thread; the server owns the socket,
protocol, identity, queue, and events. No concrete processor type crosses into `ControlServer`. To control
something other than a JUCE plugin, implement `PluginBridge` and reuse the whole plane unchanged.

**Go side, `ParamCatalog` + `LiveEndpoint`.**

```go
type ParamCatalog interface {
    Get(id string) *ParamDef            // range/type/choices for validate + clamp
    Filter(group, substr string) []ParamDef
    Groups() []string
    All() []ParamDef
}

type LiveEndpoint interface {           // one impl = liveClient over the C++ listener socket
    SetParam(id string, v float64, isReal bool) (value, applied float64, text string, err error) // isReal => v is real units (hasRealRange param); else normalized 0..1
    GetParam(id string) (value, normalized float64, text string, err error)
    SampleText(id string, points []float64) ([]ValueSample, error) // Phase-1 probe: sweep value text, restore
    PlayNote(note, chn int, vel float64) error
    // ...NoteOff / AllNotesOff / ResetInit / GetFullState / LoadState / Close
}

func RegisterParamTools(srv *mcp.Server, cat ParamCatalog, live func() LiveEndpoint)
```

`RegisterParamTools` wires `list_params` / `get_param` / `set_param` / `set_params` / `describe_param` onto any
MCP server. A host supplies a catalog and a live-endpoint accessor; the tools do the rest. Because these compose
on a plain `*mcp.Server`, a host can register its own tools alongside them on the same server. The accessor is a
`func() LiveEndpoint` so a host can swap the endpoint at runtime (the headless server's own `connect_live` /
`disconnect_live` mutate the session's `live` field, which the accessor just returns).

## The wire protocol

Localhost TCP, line-delimited JSON, request/response, bound to `127.0.0.1` only. One JSON object per line, one
JSON reply per line, each reply carrying `{ok: bool}` and echoing an optional request `id` for correlation. The
Go client (`liveClient`) adds a per-request id and a 5 s deadline, and serializes the whole write-then-read under
a mutex so two concurrent tool calls (for example a held `play_note` and a `set_param`) can never interleave
bytes on the one socket. Commands mirror the MCP tools closely, so the Go server is a thin forwarder:

| Command | Payload | Reply |
|---|---|---|
| `ping` | - | `{ok, pong}` |
| `get_param` | `{param}` | `{ok, param, value, normalized, text}` |
| `set_param` | `{param, value\|normalized\|choice}` | `{ok, param, value, normalized, text}` |
| `note_on` / `note_off` | `{chan, note, vel}` | `{ok, note, on}` |
| `all_notes_off` | - | `{ok}` |
| `reset_init` | - | `{ok, reset}` |
| `load_state` | `{state}` | `{ok, loaded}` |
| `get_full_state` | - | `{ok, state}` |
| `get_state` | - | `{ok, count, params}` |
| `govern` | `{op, group?}` | `{ok, resolution, governed}` |
| `get_governed` | - | `{ok, governed, sections}` |
| `render` | `{note?, velocity?, channel?, gate_ms?, duration_ms?, input_kind?, input_freq?, input_level?, temporal?, frame_ms?, notes?, mpe?}` (all optional; `notes` is a phrase of `{note,start_ms,gate_ms,velocity,bend?,pressure?}`) | `{ok, measurement}` (measurement gains a `modulation` block when `temporal`) |

The server also PUSHES unsolicited events (no reply id): `{event: "param_changed", param, normalized, value, text, by}` on any parameter change, and `{event: "governed_changed", governed, resolution, by}` on a lease/generation change. `ping` additionally returns the connection's `client` id.

The listener's `set_param` accepts `value`, `normalized`, or `choice` (a discrete index). In practice the Go
client only ever sends `value` (real units, for a `hasRealRange` param) or `normalized`: everything else,
including choice-by-name and discrete-by-label, is resolved to a normalized position in Go before it hits the
wire. `get_state` is a session snapshot (every resolved param's current value) used for diagnostics; it has no
MCP tool of its own.

### Opaque state

Full state (`load_state` / `get_full_state`) is treated as an **opaque** blob: `state` is base64 of the exact
bytes the plugin's own `getStateInformation` produced, round-tripped verbatim to `setStateInformation` with no
knowledge of any plugin's schema. This was recently changed from an XML re-serialization (`getXmlFromBinary` /
`copyXmlToBinary`) to raw base64 bytes, so it is format-agnostic and byte-exact: a plugin with raw-binary state
(common in commercial plugins) previously failed `save_state` outright, and the XML round-trip was lossy. The
wire field was renamed `xml` -> `state` at the same time. On `get_full_state`, an empty blob (a plugin with no
persistent state) is a valid result: a separate `fetchedStateOk` flag distinguishes "no state" from
"getStateInformation failed to run."

### Render + analysis

`render` runs an OFFLINE render on the applier thread (no audio device, no realtime): the bridge resets the
processor's DSP state (via `reset()`, NOT a `releaseResources()`/`prepareToPlay()` cycle: that cycle rebuilds the
plugin's DSP from its patch/default state and drops the parameter values pushed in via `set_param`, so every
render would come out as the default patch regardless of edits), drives it with a MIDI note (any plugin that
accepts MIDI, input left silent) or a synthesized input signal (a pure effect, `input_kind` = sine/noise/impulse/
silence), loops `processBlock` over `duration_ms` summing the output to mono, and analyzes it. Because it runs on
the single applier it serializes with `set_param`, so a measured patch is exactly the current edited state, and it
introduces no new concurrency hazard. The reply's `measurement` is `{duration_sec, sample_rate, channels, peak_db,
rms_db, crest, centroid_hz, bands:{low_db, mid_db, high_db}, silent, clipped}` (all snake_case on the wire); the
pure peak/rms/crest/silent/clipped core lives in the JUCE-free `cpp/RenderAnalysis.h`, and the FFT-derived
`centroid_hz` + three-band split are computed in the JUCE bridge. This is the feedback signal for Phase 4: an
agent sets a param, renders, and reads back an objective measurement ("brighter" = the spectral centroid rose).

**Tier 2.5 (modulation-aware / temporal).** A request may add `temporal: true` (and optional `frame_ms`, default
25); the reply then carries a `modulation` block: `{frame_ms, centroid:{rate_hz, depth, regular, confidence},
rms:{...}, pitch:{...}, dominant}`. The bridge re-analyzes the SAME render in short frames over the SUSTAIN window
only (skip the attack guard; for an instrument stop at the LAST note-off so the release tail is excluded), builds the
per-frame centroid, rms, and f0 (pitch, via `estimateF0`) envelopes, and estimates each one's dominant periodicity
(LFO rate) via a smoothed-envelope autocorrelation (`analyzeEnvelope` in `RenderAnalysis.h`, pure and unit-tested).
This makes time-varying patches legible: a filter LFO shows on `centroid`, a tremolo on `rms`, a vibrato on `pitch`
(depth in semitones), `dominant` names the strongest. The decision signal is `dominant` + `regular`; `rate_hz` is
best-effort (and frame-based f0 smears for wide/fast vibrato). `tune_param`/`tune_params` read nested modulation
measures (`modulation.centroid.rate_hz`, `modulation.pitch.depth`, etc.) and auto-enable `temporal`, so "tune the
LFO to 6 Hz" and "add vibrato" are the same loop as "make it brighter." A render can also be driven by a `notes`
phrase (chords/arps, with per-note `bend`/`pressure` and an `mpe` channel-per-note mode) instead of one held note.
See
`docs/RENDER-SCOPING.md` Tier 2.5.

## The MCP tool surface

All tools are registered on one `*mcp.Server`. The read tools reflect the live instance when connected and the
headless session otherwise.

| Tool | What it does |
|---|---|
| `list_params` | The plugin's automatable parameters (id/label/type/range/choices/default/group), with `group=` / `filter=` paging. GCF-encoded. |
| `get_param` | One parameter's definition + current value (value + normalized). Reads the live instance when connected; surfaces cached discrete labels if the param was probed. |
| `set_param` | Set one param by `value` (real units when `hasRealRange`, else normalized 0..1 or a discrete index), `normalized` (always 0..1), `choice` (a choice NAME, or a live discrete-as-float param matched by label), or `real` (a real-unit target on a live hosted param, mapped via its value text). Validated + clamped. |
| `set_params` | Set many params in one call, from a JSON array or a token-compact GCF table. Supports per-row `value`/`normalized`/`choice`/`real`. Unknown/invalid rows are skipped and reported, never fatal. |
| `describe_param` | Probe a live param's value text and report the recovered semantics (unit, real range, curve, bipolar, or discrete labels) + a derived behavior class + any annotations. Recalls from the semantic store WITHOUT re-probing once seen (even headless); a fresh probe is cached and persisted. |
| `annotate_params` | Merge-update agent-authored semantics (role, aliases, polarity, section, notes) per param and persist them. Headless; only provided fields change. |
| `get_semantic_map` | The whole current-plugin semantic map (per-param behavior class, unit/range/curve when probed, annotations). The primary read for Phase 4. |
| `forget_semantics` | Drop the current plugin's stored entry and clear the inference cache. |
| `connect_live` / `disconnect_live` | Dial / drop the control socket (default `127.0.0.1:51703`); a new connection clears any stale inference cache. |
| `play_note` / `all_notes_off` | Play a MIDI note (optional `holdMs` to auto-release) / panic all notes. Live only. |
| `save_state` / `load_state` | Snapshot the whole patch as one opaque blob / recall it. Live only. |
| `reset_init` | Recall the host-supplied init/default patch (a no-op ack if the host wired no hook). Live only. |
| `acquire_lease` / `release_lease` | Claim / release an exclusive edit lease on a param-group section (`section=`) or the whole instance (the C3 governed layer): applied / compensated / rejected. Live only. |
| `get_leases` | The current instance/section lease holders, the leasable sections, the patch generation, and your controller id. Live only. |
| `poll_events` | Drain the server-pushed `param_changed` / `governed_changed` events since the last poll (deduped to latest-per-param, other controllers only by default). Live only. |
| `render_and_measure` | Offline-render the current patch (a MIDI note for anything that accepts MIDI, else a synthesized `inputKind` signal for a pure effect) and return an objective `measurement` (peak/RMS/crest dB, spectral centroid, three-band split, silent/clipped) plus a one-line human summary. The Phase-4 feedback signal: set a param, render, read back whether it got brighter/louder. Live only. |
| `tune_param` | Drive ONE param toward a goal (`maximize`/`minimize`/`target`) on ONE `measure` (centroid_hz/peak_db/rms_db/crest/band, or a nested modulation measure like `modulation.centroid.rate_hz`/`.depth`/`modulation.pitch.*`), rendering + measuring at each step (a bounded coarse-seed + golden-section search; temporal auto-enabled for modulation measures). The agent picks the param/measure/direction from the semantic map; the tool converges it and returns the value it settled on plus the search trace. The autonomous make-it-brighter (and set-the-LFO-rate) loop (Phase 4). Live only. |
| `tune_params` | Co-optimize SEVERAL params at once by coordinate descent: a list of `knobs`, each `{id, measure, goal, target?}`, tuned in turn each round (holding the others at their best) until a round moves nothing. For compositional intents one param cannot express ("punchier" = attack + drive toward higher crest; "wobble" = LFO rate toward a target AND amount toward more depth). Temporal auto-enabled if any knob is a modulation measure. Live only. |

### The four ways to set one value

`set_param` is the interesting surface because a hosted plugin exposes so little metadata. The handler routes on
which field is present:

1. **`real=`** goes to `setParamReal`: a real-unit target (e.g. cutoff 1000 Hz) mapped through the param's probed
   value text. Live only.
2. **`choice=` on a non-choice catalog param** goes to `setParamDiscreteChoice`: a discrete control (filter type,
   on/off) that the catalog types as a plain float but that probes as discrete, matched by label. Live only.
3. **`choice=` on a real choice param** is resolved by `resolveReal` to the choice index.
4. **`value=` / `normalized=`** are resolved by `resolveReal`. For a `hasRealRange` param a real `value` is
   forwarded as real units (the plugin applies its own curve, `liveArg` sets `isReal=true`); everything else is
   forwarded as a normalized 0..1 the Go layer computed exactly.

## The semantic layer (`infer.go`)

Phase 1 recovers real-unit meaning from a hosted plugin's own value text. A hosted param exposes only a
normalized 0..1 scalar through the base API, so the catalog reports it as "0..1, mystery." But the plugin ships
its own `getText` formatter, and sweeping it across the range often reveals the real unit, range, curve shape,
bipolarity, and even params that are really discrete toggles or enums hiding as floats. This is deterministic:
no model involved. It is also plugin-dependent: a param that does not implement `getText` yields unitless numbers
and the layer simply learns nothing, degrading to "normalized only" rather than guessing.

- **The probe (`SampleText`).** Sets the param across 21 uniform normalized points, collecting the rendered value
  text at each, then restores the original value (best-effort, via the normal set path, so a DAW would see the
  transient sweep). Dense enough for a good curve read and a close piecewise-linear seed. One-time cost per param;
  the result is cached on the session and cleared on a new `connect_live`.
- **Parse and normalize (`parseValueText` / `normalizeUnit`).** A regex pulls a leading signed number and an
  optional unit token (letters or `%`) out of each rendered string, dropping a trailing annotation like
  `(Left)`. A non-numeric string (e.g. `Off`, `Ladder`) marks the param discrete. Numeric samples are folded into
  a base unit per family (`ms` -> `s`, `kHz` -> `Hz`) so a sweep that switches units still builds a monotonic
  real table.
- **Inference (`inferParam`).** Any non-numeric sample means the param is discrete: it records the distinct
  labels and, per label, a representative (median) norm that renders it (for the discrete-by-label set path).
  Otherwise it builds a sorted (norm, real) table, picks the dominant base unit, records `RealMin`/`RealMax`,
  flags `Bipolar` (min < 0 < max), classifies the curve (`classifyCurve` reads where the midpoint lands relative
  to a straight line: exponential, logarithmic, or linear), and fits a closed-form model.
- **Analytic curve fit (`CurveFit`).** `fitCurve` fits three models by least squares and keeps the lowest-error
  one: **linear** (`real = A + B*norm`), **exp** (`real = A*exp(B*norm)`, fit in log space, only when all reals
  are positive), and **power** (`real = A*norm^B`, fit in log-log space). The power model captures a zero-crossing
  curve, a time knob that grows from 0 ms to 32 s, which exp cannot fit (`log(0)` is undefined) and linear fits
  poorly. A fit is trusted for closed-form inversion only when its worst sample error is within 1% of the range
  (`analyticReliable`).
- **Inversion and refine (`NormForReal` / `refineToReal`).** `NormForReal` inverts to the norm to send: the
  analytic inverse when the fit is reliable, else piecewise-linear interpolation over the sampled table. For a
  single `set_param real=`, when no clean model fits, `refineToReal` seeds with the sampled estimate and then
  binary-searches the norm within the sample bracket against live `getText` readbacks (bounded to 16 iterations),
  landing the value as exactly as the plugin allows on an awkward log/exp region. Batch `real` rows use the
  analytic inverse only (no per-set search).
- **Discrete-hiding-as-float by label.** When a param probes as discrete, `set_param choice=<name>` maps the
  label (case-insensitive) to its representative norm and forwards that, then reports whether the plugin's
  readback confirms the label. `get_param` surfaces the observed labels once a param has been probed;
  `list_params` deliberately does not probe (it would probe every param).

### Sectioning (`sections.go`, Phase 2)

Many plugins expose a flat parameter list (every param lands in `"other"`) whose labels nonetheless carry clear
structure: "Filter Cutoff", "Amp Attack", "Osc 1 Tune". `deriveSections` derives navigable sections from shared
label prefixes so a flat surface can be paged with `group=`. A leading token-prefix (numbers dropped, so
"Osc 1 Tune" and "Osc 2 Tune" share "Osc") becomes a section when at least 3 params share it; each param takes
the longest qualifying prefix, and the rest fall to `"other"` (sorted last). This is a **catalog-level view**:
the `Catalog` computes an `effGroup` slice once at construction, used only by `Groups()` and `Filter()`. Real
groups always win when present (`derived` is false); the derivation never mutates `ParamDef.Group` or the wire
shape. This is what "Phase 2 is done" means. Phase 3 (the persistent semantic store, `semantic.go`) now backs the
inference cache and adds agent-authored semantics. Phase 4 (intent -> params) has begun: `tune_param` (its first
increment) closes the loop mechanically, with the agent supplying the intent -> (param, measure, direction) mapping
over the semantic map and the server converging it against the render loop (see below, and `docs/PHASE4-SCOPING.md`).

## GCF (token efficiency)

A plugin with hundreds of parameters is a token nightmare to serialize into an agent's context as JSON. The big
read tool (`list_params`) encodes its model-facing payload as [GCF](https://github.com/blackwell-systems/gcf-go),
50-92% smaller than JSON and comprehended zero-shot. `structuredResult` returns the human summary plus a GCF
block as the tool's TEXT content and no `StructuredContent` (so there is no parallel JSON copy inflating the
token bill). It degrades safely: if GCF is disabled (`SIDECHAIN_MCP_FORMAT=json`) or an encode hits the
numeric-domain guard (`EncodeGenericChecked` returns an error), it falls back to the JSON `StructuredContent`
path, so a bad value never panics the long-running server.

GCF is orthogonal to the control socket: that channel stays plain JSON (no model reads it, and the C++ side has
no GCF decoder). `set_params` additionally accepts a GCF table as input, so authoring a whole patch is one
compact call rather than one-per-parameter. The batch handler is lenient: a bare table without the `GCF ...`
header gets a `GCF profile=generic` header prepended before decoding.

## Data-flow walkthroughs

### A `set_param real=1000` on a hosted param

```
agent --MCP set_param{id, real:1000}--> Go handleSetParam ──▶ setParamReal
   session.inference(id): cached? else probe -> SampleText (21 norms over the socket) -> inferParam
   fit reliable?  yes -> NormForReal analytic inverse -> one SetParam(norm) over the socket
                  no  -> refineToReal: seed + bounded binary search, each step a SetParam(norm) + getText readback
   C++ listener: enqueue SetParam(norm) -> message thread setValueNotifyingHost -> plugin applies its own curve
   reply {ok, value, normalized, text} per set -> Go reports "Set LIVE ... plugin reports \"1000.0 Hz\""
```

### A `describe_param`

```
agent --MCP describe_param{id}--> Go handleDescribeParam
   if the semantic store already has this param's inference -> recall it (no probe, works headless)
   else (live): session.probe(id) sweeps 21 norms over the socket, restores, infers unit/range/curve/labels,
                caches on the session AND writes through to the store (so a future session skips the sweep)
   reply: a one-line summary + behavior class + any role annotation + inference (JSON StructuredContent)
```

### A `save_state` / `load_state` round-trip

```
save_state --> liveClient GetFullState -> {cmd:get_full_state}
   C++ enqueue GetFullState -> message thread getStateInformation(MemoryBlock) -> base64 -> reply {state}
   Go returns the opaque base64 string to the agent verbatim
load_state{state} --> liveClient LoadState -> {cmd:load_state, state}
   C++ enqueue LoadState -> message thread base64-decode -> setStateInformation(exact bytes) -> {ok, loaded}
```

### A `render_and_measure` "make it brighter" loop

```
set_param{cutoff, normalized:0.9} --> applied on the message thread (param now reflects the edit)
render_and_measure{note:60, gate_ms:800, duration_ms:1500}
   --> liveClient Render -> {cmd:render, ...}
   C++ enqueue Render -> message thread: reset() DSP state -> note_on -> processBlock loop -> sum to mono
                         -> RenderAnalysis (peak/rms/crest) + FFT (centroid + 3 bands) -> {ok, measurement}
   Go decodes the snake_case measurement, returns {measurement, summary} (e.g. "centroid 2.02 kHz")
   agent compares the centroid before/after: higher == brighter (objective, no listening required)
```

## Testing and CI architecture

The Go layer is tested at several levels; the C++ host is exercised through the real-plugin integration jobs plus
two small C++ unit targets that need no JUCE, both run by the `cpp.yml` `unit` job: the pure section-derivation
algorithm (`cpp/tests/section_derivation_test.cpp`) and the pure render-analysis core
(`cpp/tests/render_analysis_test.cpp`: peak/RMS/crest/silent/clipped from a known buffer).

- **Unit tests (always green, no plugin).** Catalog math, sectioning, inference, GCF, the concurrency conflict
  tier (`governed_test.go`, an exhaustive enumeration of the governed state), the semantic store (`semantic_test.go`:
  fingerprint equivalence, atomic round-trip + merge, behavior-class, headless annotate + reload, non-destructive
  invalidation), the concurrency/semantic MCP tools (`govtools_test.go`, `semantic_test.go`), and the handler logic
  are covered by table tests. The power fit is unit-tested only (`TestSetRealPowerAnalytic` with a synthetic
  `32*norm^6.5` s curve): a survey of the plugins on hand found none whose clean power-law params carry a real
  unit, so there is no real-plugin power-fit E2E.
- **The in-process fake host (`live_test.go`).** A goroutine speaks the `ControlServer` line-JSON protocol
  against an in-memory param map, with rendered value text that mimics real plugins (linear Hz, log Hz, a
  zero-crossing power time knob, a sigmoid that fits no model, an on/off toggle, a discrete-as-float filter type,
  a bare-number unitless param). It drives the whole session live path (connect, set/get, real-unit, discrete,
  MIDI, state, disconnect) without a running C++ host. Because the real listener speaks the identical protocol,
  this is "same bytes on the socket."
- **The in-memory MCP transport (`transport_test.go`).** Stands up the actual `mcp.Server` over
  `NewInMemoryTransports` and calls tools as a client would, exercising tool registration, JSON argument
  unmarshaling, dispatch, and result encoding (including the GCF path). Headless.
- **The gated real-plugin E2E suite.** Skips cleanly unless env-gated, so the normal `go test` stays green:
  - **Generic, catalog-driven (`sweep_live_test.go`, gated on `SIDECHAIN_SWEEP_PORT` + `SIDECHAIN_SWEEP_CATALOG`,
    MIDI additionally on `SIDECHAIN_SWEEP_MIDI=1`).** `TestFullSurfaceSweep` sets every param to {0, 0.5, 1} and
    reads each back; `TestStateRoundTrip` snapshots, mutates, saves, loads, and asserts restoration;
    `TestBatchSetParams` applies N rows in one call and asserts applied N / skipped 0; `TestMidiSmoke` plays a
    held note then panics. No plugin-specific ids, so these run against any hosted plugin.
  - **Surge-specific capability tests (`infer_test.go`, `e2e_live_test.go`, gated on `SIDECHAIN_LIVE_*`).** Prove
    the Phase-1 path against real Surge XT: cutoff exp-fit inference/inversion, discrete set-by-label, and a
    steep zero-based time curve that drives the binary-search refinement.
  - **AU load+drive smoke (`au_live_test.go`, gated on `SIDECHAIN_AU_*`).** Loads an Apple built-in AU by
    component identifier, enumerates its catalog, and drives it over the socket, closing the AU gap (everything
    else exercises VST3).
  - **Full tool surface (`full_tool_surface_test.go`, gated, run per plugin).** `TestFullToolSurfaceLive` drives
    EVERY MCP tool handler against the real host in one run - reads, writes, state (save/load/reset), MIDI, the
    governed leases, the semantic tools, and `poll_events` surfacing a real change pushed by a second controller -
    so no tool is only unit- or fake-host-tested. All 19 handlers are exercised end to end.
  - **Sectioning + semantic store (gated on the sweep env, run per plugin by `drive_plugin.sh`).**
    `TestSectionLockstep` asserts the host's emitted per-param `section` equals the Go reference derivation (so the
    two implementations cannot drift); `TestSemanticStoreLive` probes a real param once, then a second HEADLESS
    session recalls it from the store (proving probe-once-ever).
  - **Concurrency (gated on the sweep env, the C1+C2+C3 leg).** `TestMultiClientLive` (independent concurrent
    control), `TestChangeNotifications` (a set by one controller is pushed to another), `TestGovernedLive` +
    `TestGovernedToolsLive` (hierarchical edit leases via the wire and via the MCP tools), and
    `TestGovernedDisconnectFreesLease` (crash-safe lease cleanup). See [CONCURRENCY.md](CONCURRENCY.md).
  - **Render + analysis (`render_live_test.go`).** `TestRenderSmoke` (gated on the sweep env, run per plugin by
    `drive_plugin.sh`) renders a note and asserts a non-degenerate measurement came back; values are plugin-specific
    and deliberately not asserted. `TestRenderBrighter` (gated on `SIDECHAIN_LIVE_*`, run against TAL-NoiseMaker's
    Filter Cutoff, whose init patch has an active lowpass) is the canonical make-it-brighter proof: render low
    cutoff, render high cutoff, assert the spectral centroid rose (~10x in practice).
  - **Closed-loop tuning (`tune_tools_test.go` + `tune_params_test.go` in-memory, `tune_live_test.go` +
    `tune_params_live_test.go` gated).** The in-memory tests drive `tune_param` and `tune_params` against the fake
    host (centroid responds to `cutoff`, rms to `gain`, so there are two independent axes) and assert
    maximize/minimize/target converge and the set/restore landing is correct. `TestTuneBrighterLive` (gated, TAL
    cutoff) is the AUTONOMOUS make-it-brighter loop; `TestTuneParamsWobbleLive` (gated, TAL Lfo 1) co-tunes the LFO
    rate toward a target AND the amount toward more modulation depth.
  - **Modulation-aware measurement (Tier 2.5, `render_analysis_test.cpp` for the pure envelope core;
    `modulation_live_test.go` gated).** The C++ unit test asserts `analyzeEnvelope` on synthetic envelopes (2 Hz
    sine -> rate ~2 + regular, ramp -> not periodic, noise -> irregular). `TestRenderTemporalLive` and
    `TestTuneModulationRateLive` (gated, run against TAL with Lfo 1 routed to the cutoff by the CI step) prove a
    real LFO is measured (`regular` at an LFO-band rate) and tunable (`tune_param` on the LFO rate -> a target rate).
  - **Power-fit survey (`scan_test.go`, gated on `SIDECHAIN_SCAN_*`).** Probes every param on a running host and
    flags any clean analytic power fit; a survey aid, not a CI assertion.
- **The CI matrix.**

  | Workflow | Runs on | Gate | What |
  |---|---|---|---|
  | `go.yml` | ubuntu | required | gofmt, `go mod tidy`, build, vet, `go test -race -coverprofile`; a required pinned `staticcheck`; a report-only `govulncheck` (stdlib-CVE noise, so it would red on toolchain drift). |
  | `cpp.yml` (`unit`) | ubuntu | required | Compile + run the JUCE-free section-derivation and render-analysis unit tests with `-Wall -Wextra -Werror`. Fast, deterministic. |
  | `cpp.yml` (`build`) | ubuntu + macOS-14 + windows | best-effort | Configure + build the JUCE host (VST3 everywhere, AU on macOS), `fail-fast:false`. Our two TUs compile with `-Werror` / `/WX` (scoped so JUCE's own module sources are exempt), so a warning in our code fails the build. clang-tidy is report-only by design (runner linter-version drift). |
  | `integration.yml` (`smoke`) | macOS-14 | required + report | Build the host; required legs: Surge XT (synth, generic suite + capability), Surge XT Effects VST3, an Apple-AU load+drive, and the C1+C2+C3 concurrency leg. Report-only: none currently. |
  | `integration.yml` (`smoke-linux`) | ubuntu | required + report | Build once, then drive Surge XT / TAL-NoiseMaker / Dexed (required) and Surge XT Effects (report-only: its state round-trip does not restore headless on Linux) under `xvfb-run`, plus a required flat-plugin section-lease leg (TAL derived sections). |

  The plugin-drive steps share `.github/scripts/drive_plugin.sh`, which starts the host, waits for the catalog,
  asserts enumeration > 0, and runs the generic gated suite (sweep / state / batch / MIDI / section-lockstep /
  semantic-store) pointed at that host. A new leg lands `continue-on-error` and is promoted to required once green
  across runs (the report-only staticcheck / govulncheck pattern). Validated locally against Surge XT (774 params)
  and TAL-NoiseMaker (89 params).

## Configuration

Environment variables read by the running Go server: `SIDECHAIN_CATALOG` (the catalog JSON path, equivalent to
`--catalog`), `SIDECHAIN_MCP_FORMAT` (`json` forces plain JSON output instead of GCF), and `SIDECHAIN_SEMANTIC_DIR`
(the persistent semantic-store directory, equivalent to `--semantic-dir`; empty defaults to a per-user cache dir).
The control port is a `--port` flag on the C++ host and a `port` argument to `connect_live` (both default 51703);
there is no port environment variable in the running path (the `SIDECHAIN_PORT` name appears only in tool-schema
prose). The remaining `SIDECHAIN_*` variables are test gates, not runtime configuration.

## Boundaries (honest limits)

- You get the **automatable-parameter surface**, not more. GUI-only controls, or state a plugin does not expose
  as parameters, are invisible to any host. That is the same surface a human automating in a DAW gets.
- **Parameter-metadata quality varies wildly.** Well-behaved plugins name params cleanly; many expose garbage
  ("Param 47") or hundreds of undifferentiated entries. The Phase-1 semantic layer recovers real units from value
  text where the plugin renders them (a param that renders bare numbers stays "normalized only"); the Phase-3
  store lets the agent attach a durable name-to-meaning layer (roles) on top, but the roles are agent-supplied, not
  inferred by the bridge. Turning intent into param moves over that map ("make it brighter") is Phase 4: `tune_param`
  provides the objective search; the agent still supplies the intent -> (param, measure, direction) mapping.
- **Real units only on native ranged params by default.** A hosted VST3/AU param arrives as
  `HostedAudioProcessorParameter` with no `NormalisableRange`, so its catalog range is a bare 0..1
  (`hasRealRange: false`); real-unit targeting on it goes through the value-text probe (`set_param real=`), not a
  known curve. `hasRealRange: true` (real endpoints and skew) fires only for a native `RangedAudioParameter`, the
  case an embedded/own-plugin host exercises.
- **VST3/AU only.** VST2 is not hosted (the Steinberg VST2 SDK has been unlicensed since 2018 and cannot be
  redistributed, which would break the "build from source, standard APIs only" guarantee). A VST2-only instrument
  is reachable by wrapping it into a VST3 with an external adapter; the bridge never sees VST2, and wrapper
  fidelity varies.
- **Multiple controllers, one instance.** The `ControlServer` serves many connections at once (concurrency
  C1/C2/C3): each gets a `clientID`, drives the plugin independently, receives `param_changed` events for others'
  changes, and can claim hierarchical edit leases (governed by a conflict tier on the single applier). The gated
  E2E tests still disconnect explicitly (a leaked connection holds a handler thread and muddies identity/attribution
  assertions). "Concurrency" here means many controllers of one instance, not many hosts; see
  [CONCURRENCY.md](CONCURRENCY.md).
- **The power fit is unit-tested only.** No surveyed real plugin exposes a clean power-law param the real-unit set
  path can drive (see the note by `fitPower` and `TestScanPowerFits`).
- **No reverse-engineering, no redistribution.** Hosting a plugin is what DAWs do; the user supplies their own
  licensed binaries. No source is touched and nothing is shipped.
</content>
</invoke>
