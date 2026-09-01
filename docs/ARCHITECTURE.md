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
separate process; they meet on one localhost socket. The two seam interfaces (`ControlListener` on the C++ side,
`ParamCatalog` + `LiveEndpoint` on the Go side) are what keep the design generic.

```
  ┌──────────────────────────── C++ host process (sidechain-host) ────────────────────────────┐
  │                                                                                            │
  │  main.cpp ──▶ sidechain::Host                                                              │
  │                 • load()            AudioPluginFormatManager: VST3 path / AU by identifier │
  │                 • enumerateCatalog() walks getParameters() -> catalog JSON (to a file)     │
  │                 • startControl()     constructs the listener on 127.0.0.1:<port>           │
  │                                                                                            │
  │               sidechain::ControlListener  (a juce::Thread + juce::AsyncUpdater)            │
  │                 socket thread     parse a line, snapshot reads, enqueue mutations          │
  │                 SPSC ring (256) ──▶ message thread  setValueNotifyingHost / state / MIDI   │
  └────────────────────────────────────────────┬───────────────────────────────────────────┘
                                                │  localhost TCP, line-delimited JSON
  ┌─────────────────────────────────────────────┴──────────────── Go server process (sidechain) ┐
  │                                                                                              │
  │  cmd/sidechain/main.go ──▶ Run() ──▶ NewServer()                                             │
  │                                                                                              │
  │     Catalog  (ParamCatalog)      loaded from the catalog JSON; validate + clamp + sections  │
  │     session                      one per process; holds catalog + live endpoint + infer cache│
  │     liveClient  (LiveEndpoint)   the wire client over the control socket                    │
  │     RegisterParamTools / registerLiveTools   the MCP tool surface on one *mcp.Server        │
  │     GCF (structuredResult)       token-compact encoding of the big read tool                │
  └──────────────────────────────────────────────────────────────────────────────────────────┘
```

### The C++ host (`sidechain::Host`, `cpp/Host.h` / `cpp/Host.cpp`)

A headless child-plugin host. It loads one plugin, enumerates its catalog, and points a `ControlListener` at
the hosted processor. `main.cpp` is a thin CLI that drives it and pumps a JUCE message loop.

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
  `id`, `label`, `group`, `type`, `min`, `max`, `step`, `default`, `hasRealRange`, and (for choices) `choices`.
  The root object also stamps `stateRootTag`, `stateVersion`, and `count`. Only automatable params are emitted.
- **Parameter-tree grouping.** VST3 units and AU clumps surface as nested `AudioProcessorParameterGroup`s in the
  plugin's parameter tree. Enumeration walks that tree once and records each param's immediate parent group name,
  giving an agent a handle to page a large surface. A plugin with a flat tree leaves the group empty, which the
  Go side maps to `"other"`.
- **Type inference from the base API.** A param that `isDiscrete()` with value-strings becomes a `choice` (with
  the strings as `choices`); a boolean or a 2-step param becomes `bool`; any other discrete param becomes `int`;
  everything else is `float`.
- **Native vs hosted parameter handling (`hasRealRange`).** A native `RangedAudioParameter` (an embedded JUCE
  plugin, or Sidechain's own params) owns a real-to-normalized curve, including the skew that frequency, gain,
  and time controls use. When a param is both ranged and continuous (a `float`), `enumerateCatalog` reports its
  **real** endpoints and default through the plugin's own `NormalisableRange` and sets `hasRealRange: true`. A
  hosted continuous param has no such scalar, so it reports `0..1` and `hasRealRange: false`. A discrete param
  (choice/int/bool) reports its index range `0..(steps-1)` on either path (skew is a continuous-value concern).

### The `ControlListener` (`cpp/ControlListener.h`)

A single-header, in-process live-control listener for any `juce::AudioProcessor`. It has no dependency on any
concrete processor type: it takes a `juce::AudioProcessor&`, a `juce::MidiKeyboardState&`, and a port, so it is
a drop-in for a hosted child plugin or for an embedded native plugin alike.

- **Thread model (the load-bearing safety rule).** The socket runs on its own background thread (a
  `juce::Thread`). That thread never touches parameter or DSP state directly (that would race `processBlock`).
  It only: parses a line of JSON; for **reads**, snapshots the parameter's atomic value and replies directly
  (reads are safe from any thread); for **mutations**, resolves the target, pushes a `Command` into a lock-free
  SPSC queue, fires an `AsyncUpdate`, and waits (bounded) for the message thread to apply it before acking. The
  message thread drains the queue (`handleAsyncUpdate`) and applies each command via `setValueNotifyingHost`,
  `keyboardState`, or the state calls: exactly the on-screen-keyboard / GUI-edit path, so the editor and the host
  see the change and it serializes normally.
- **The SPSC command queue.** A `juce::AbstractFifo` over a fixed 256-deep ring of `Command` structs. The socket
  thread is the sole producer, the message thread the sole consumer, so no lock is needed. If the ring is full
  the command is dropped (a human or agent issues commands far slower than a ~20 ms drain, so a drop only means
  one command did not land, never a crash or a race). A parallel `wantAck` bit array carries whether each command
  should signal the `appliedEvent` the socket thread is blocked on.
- **Bounded apply-then-ack.** For a `set_param`, the socket thread resets `appliedEvent`, enqueues, then waits
  up to 250 ms; the ack therefore means "applied," giving the agent clean request/response semantics (the value
  is live before the reply lands). `reset_init` waits 500 ms and state verbs wait 2 s (a full-state restore is
  heavier).
- **One client at a time, EOF handling.** The listener binds `127.0.0.1` only and serves one connection at a
  time. The read loop waits for readability first, then reads: when the socket is ready but `read` returns 0 the
  peer has closed (EOF), so it returns and lets the accept loop take the next client. (An earlier version treated
  a 0-byte read as "keep waiting," which spun on EOF and stalled the next client's handshake.)
- **Value semantics.** `valueForCatalog` maps a normalized 0..1 back to the catalog `value`: for a continuous
  native (ranged) param, the real value via the plugin's own skew-aware curve; for a discrete param, the index
  `0..(steps-1)`; for a hosted continuous param, the normalized value itself.
- **The single host hook.** `onResetInit` is a nullable `std::function<void()>` run on the message-thread drain
  for the `reset_init` command. Null means the command is a no-op ack. It is the one host-specific extension
  point, kept as a callback so the header stays free of any concrete processor dependency.

### The Go server (`server.go`, `paramtools.go`, `live.go`, `catalog.go`)

- **`NewServer` / `Run`.** `Run` loads the catalog JSON (failing fast if it is empty or corrupt: a server with
  no catalog cannot validate or clamp anything) and serves stdio JSON-RPC. `NewServer` builds one `*mcp.Server`,
  registers the generic param tools and the live verbs against a single `session`, and returns both so a caller
  can drive it in-process (tests, or embedding in another Go host).
- **The `session`.** One per process. It holds the `ParamCatalog`, a headless param map (real-unit values keyed
  by id, used when not connected live), the current `LiveEndpoint` (nil means headless), and the per-session
  inference cache (`infer`, keyed by param id). A single mutex serializes tool calls, which is also what keeps the
  line-delimited control protocol from interleaving.
- **The `Catalog`.** The read side: indexed by id for O(1) lookup, with the pure param math (`clampReal`,
  `normToReal`, `realToNorm`, `choiceIndex`, `roundHalfUp`). It also computes an **effective-group** view once at
  construction (see the semantic layer, sectioning). `ParamDef.Group` and the wire shape are never mutated by the
  view.
- **GCF.** The big read tool (`list_params`) encodes its model-facing payload as GCF (see below).

### The two seam interfaces

**C++ side, `sidechain::ControlListener`.** Point it at any `juce::AudioProcessor` (your own plugin, or a hosted
child plugin's processor) plus a `juce::MidiKeyboardState` and a port:

```cpp
sidechain::ControlListener listener (targetProcessor, keyboardState, /*port*/ 51703);
listener.onResetInit = []{ /* optional host-supplied init/default recall */ };
```

The listener walks `getParameters()` for the id catalog, snapshots atomic values for reads, and routes every
mutation through the message thread via the SPSC queue. No concrete processor type crosses this boundary.

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

## The MCP tool surface

All tools are registered on one `*mcp.Server`. The read tools reflect the live instance when connected and the
headless session otherwise.

| Tool | What it does |
|---|---|
| `list_params` | The plugin's automatable parameters (id/label/type/range/choices/default/group), with `group=` / `filter=` paging. GCF-encoded. |
| `get_param` | One parameter's definition + current value (value + normalized). Reads the live instance when connected; surfaces cached discrete labels if the param was probed. |
| `set_param` | Set one param by `value` (real units when `hasRealRange`, else normalized 0..1 or a discrete index), `normalized` (always 0..1), `choice` (a choice NAME, or a live discrete-as-float param matched by label), or `real` (a real-unit target on a live hosted param, mapped via its value text). Validated + clamped. |
| `set_params` | Set many params in one call, from a JSON array or a token-compact GCF table. Supports per-row `value`/`normalized`/`choice`/`real`. Unknown/invalid rows are skipped and reported, never fatal. |
| `describe_param` | Probe a live param's value text across its range and report the recovered semantics (unit, real range, curve, bipolar, or discrete labels). Live only; the result is cached for `set_param real=`. |
| `connect_live` / `disconnect_live` | Dial / drop the control socket (default `127.0.0.1:51703`); a new connection clears any stale inference cache. |
| `play_note` / `all_notes_off` | Play a MIDI note (optional `holdMs` to auto-release) / panic all notes. Live only. |
| `save_state` / `load_state` | Snapshot the whole patch as one opaque blob / recall it. Live only. |
| `reset_init` | Recall the host-supplied init/default patch (a no-op ack if the host wired no hook). Live only. |

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
shape. This is what "Phase 2 is done" means. The per-session inference cache (Phase 1) is not persisted; a
persistent semantic store (Phase 3) and intent mapping (Phase 4) are future work.

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
agent --MCP describe_param{id}--> Go handleDescribeParam (live only)
   session.probe(id): SampleText sweeps 21 norms over the socket, restores the original, returns samples
   inferParam(samples) -> unit/range/curve/bipolar or discrete labels; cached on the session
   reply: a one-line summary + the samples + inference (JSON StructuredContent)
```

### A `save_state` / `load_state` round-trip

```
save_state --> liveClient GetFullState -> {cmd:get_full_state}
   C++ enqueue GetFullState -> message thread getStateInformation(MemoryBlock) -> base64 -> reply {state}
   Go returns the opaque base64 string to the agent verbatim
load_state{state} --> liveClient LoadState -> {cmd:load_state, state}
   C++ enqueue LoadState -> message thread base64-decode -> setStateInformation(exact bytes) -> {ok, loaded}
```

## Testing and CI architecture

The Go layer is tested at four levels; the C++ host is exercised only through the real-plugin integration jobs
(there is no C++ unit test target).

- **Unit tests (always green, no plugin).** Catalog math, sectioning, inference, GCF, and the handler logic are
  covered by table tests (`catalog_test.go`, `catalog_more_test.go`, `sections_test.go`, `infer_test.go`,
  `infer_more_test.go`, `gcf_more_test.go`, `paramtools_more_test.go`, `discrete_choice_test.go`,
  `server_test.go`). The power fit is unit-tested only (`TestSetRealPowerAnalytic` with a synthetic
  `32*norm^6.5` s curve): a survey of the plugins on hand found none whose clean power-law params carry a real
  unit, so there is no real-plugin power-fit E2E.
- **The in-process fake host (`live_test.go`).** A goroutine speaks the `ControlListener` line-JSON protocol
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
  - **Power-fit survey (`scan_test.go`, gated on `SIDECHAIN_SCAN_*`).** Probes every param on a running host and
    flags any clean analytic power fit; a survey aid, not a CI assertion.
- **The CI matrix.**

  | Workflow | Runs on | Gate | What |
  |---|---|---|---|
  | `go.yml` | ubuntu | required | gofmt, `go mod tidy`, build, vet, `go test -race -coverprofile`; a required pinned `staticcheck`; a report-only `govulncheck` (dominated by stdlib CVEs, so it would red on toolchain drift). |
  | `cpp.yml` | ubuntu + macOS-14 + windows | best-effort | Configure + build the JUCE host (VST3 everywhere, AU on macOS). `fail-fast:false` so one leg does not red the others. Report-only clang-tidy on Linux; warnings-as-errors is a deferred TODO. |
  | `integration.yml` (`smoke`) | macOS-14 | mixed | Build the host; Surge XT VST3 (synth) is the hard-required generic-suite leg plus the Surge-only capability tests; a report-only Surge XT Effects VST3 leg (a non-synth, no MIDI); a report-only Apple-AU load+drive. |
  | `integration.yml` (`smoke-linux`) | ubuntu | report-only | Build the host once, then drive Surge XT, TAL-NoiseMaker, Dexed, and Surge XT Effects VST3 under `xvfb-run` (headless JUCE plugin hosting still touches X at construction). |

  The plugin-drive steps share `.github/scripts/drive_plugin.sh`, which starts the host, waits for the catalog,
  asserts enumeration > 0, and runs the generic gated suite pointed at that host. Everything unproven is
  `continue-on-error` until it has been green on CI for a few runs, mirroring the report-only staticcheck /
  govulncheck pattern. Validated locally against Surge XT (774 params) and TAL-NoiseMaker (89 params).

## Configuration

Two environment variables are read by the running Go server: `SIDECHAIN_CATALOG` (the catalog JSON path,
equivalent to `--catalog`) and `SIDECHAIN_MCP_FORMAT` (`json` forces plain JSON output instead of GCF). The
control port is a `--port` flag on the C++ host and a `port` argument to `connect_live` (both default 51703);
there is no port environment variable in the running path (the `SIDECHAIN_PORT` name appears only in tool-schema
prose). The remaining `SIDECHAIN_*` variables are test gates, not runtime configuration.

## Boundaries (honest limits)

- You get the **automatable-parameter surface**, not more. GUI-only controls, or state a plugin does not expose
  as parameters, are invisible to any host. That is the same surface a human automating in a DAW gets.
- **Parameter-metadata quality varies wildly.** Well-behaved plugins name params cleanly; many expose garbage
  ("Param 47") or hundreds of undifferentiated entries. The Phase-1 semantic layer recovers real units from value
  text where the plugin renders them, but a param that renders bare numbers stays "normalized only," and a
  name-to-meaning layer over generic names is still future work.
- **Real units only on native ranged params by default.** A hosted VST3/AU param arrives as
  `HostedAudioProcessorParameter` with no `NormalisableRange`, so its catalog range is a bare 0..1
  (`hasRealRange: false`); real-unit targeting on it goes through the value-text probe (`set_param real=`), not a
  known curve. `hasRealRange: true` (real endpoints and skew) fires only for a native `RangedAudioParameter`, the
  case an embedded/own-plugin host exercises.
- **VST3/AU only.** VST2 is not hosted (the Steinberg VST2 SDK has been unlicensed since 2018 and cannot be
  redistributed, which would break the "build from source, standard APIs only" guarantee). A VST2-only instrument
  is reachable by wrapping it into a VST3 with an external adapter; the bridge never sees VST2, and wrapper
  fidelity varies.
- **One client at a time.** The `ControlListener` serves a single connection; a second connect blocks until the
  first disconnects. The gated E2E tests disconnect explicitly for this reason.
- **The power fit is unit-tested only.** No surveyed real plugin exposes a clean power-law param the real-unit set
  path can drive (see the note by `fitPower` and `TestScanPowerFits`).
- **No reverse-engineering, no redistribution.** Hosting a plugin is what DAWs do; the user supplies their own
  licensed binaries. No source is touched and nothing is shipped.
</content>
</invoke>
