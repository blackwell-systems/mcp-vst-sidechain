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

## The seam: two interfaces

### C++ side - `sidechain::ControlListener`

Point it at any `juce::AudioProcessor` (your own plugin, or a hosted child plugin's processor) plus a
`juce::MidiKeyboardState` and a port:

```cpp
sidechain::ControlListener listener (targetProcessor, keyboardState, /*port*/ 51703);
listener.onResetInit = []{ /* optional host-supplied init/default recall */ };
```

The listener walks `getParameters()` for the id catalog, snapshots atomic values for reads, and routes every
mutation through the message thread (`setValueNotifyingHost`) via a lock-free SPSC queue. No concrete processor
type crosses this boundary. `onResetInit` is the single host-specific hook, and it is a nullable
`std::function`, so it is a clean extension point, not a dependency.

### Go side - `ParamCatalog` + `LiveEndpoint`

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
    PlayNote(note, chn int, vel float64) error
    // ...NoteOff / AllNotesOff / ResetInit / GetFullState / LoadState / Close
}

func RegisterParamTools(srv *mcp.Server, cat ParamCatalog, live func() LiveEndpoint)
```

`RegisterParamTools` wires `list_params` / `get_param` / `set_param` / `set_params` onto any MCP server. A host
supplies a catalog and a live-endpoint accessor; the tools do the rest. Because these compose on a plain
`*mcp.Server`, a host can register its own tools alongside them on the same server.

## The wire protocol

Localhost TCP, line-delimited JSON, request/response, bound to `127.0.0.1` only. One JSON object per line, one
JSON reply per line, each reply carrying `{ok: bool}`. Commands mirror the MCP tools 1:1, so the Go server is a
thin forwarder:

| Command | Payload | Reply |
|---|---|---|
| `ping` | - | `{ok, pong}` |
| `get_param` | `{param}` | `{ok, param, value, normalized, text}` |
| `set_param` | `{param, value\|normalized\|choice}` | `{ok, param, value, normalized, text}` |
| `note_on` / `note_off` | `{chan, note, vel}` | `{ok, note, on}` |
| `all_notes_off` | - | `{ok}` |
| `reset_init` | - | `{ok, reset}` |
| `load_state` | `{xml}` | `{ok, loaded}` |
| `get_full_state` | - | `{ok, xml}` |
| `get_state` | - | `{ok, count, params}` |

Full state (`load_state` / `get_full_state`) is treated as an **opaque** blob: the bridge round-trips the
plugin's own `getStateInformation` / `setStateInformation`, with no knowledge of any plugin's schema.

## GCF (token efficiency)

A plugin with hundreds of parameters is a token nightmare to serialize into an agent's context as JSON. The
big read tool (`list_params`) encodes its model-facing payload as [GCF](https://github.com/blackwell-systems/gcf-go),
50-92% smaller than JSON and comprehended zero-shot. Set `SIDECHAIN_MCP_FORMAT=json` to force plain JSON.

GCF is orthogonal to the control socket: that channel stays plain JSON (no model reads it, and the C++ side has
no GCF decoder). `set_params` additionally accepts a GCF table as input, so authoring a whole patch is one
compact call rather than one-per-parameter.

## Boundaries (honest limits)

- You get the **automatable-parameter surface**, not more. GUI-only controls, or state a plugin does not
  expose as parameters, are invisible to any host. That is the same surface a human automating in a DAW gets.
- **Parameter-metadata quality varies wildly.** Well-behaved plugins name params cleanly; many expose garbage
  ("Param 47") or hundreds of undifferentiated entries. An LLM layer that infers semantic meaning from generic
  param names is the natural next layer, and is not built yet.
- **No reverse-engineering, no redistribution.** Hosting a plugin is what DAWs do; the user supplies their own
  licensed binaries. No source is touched and nothing is shipped.
