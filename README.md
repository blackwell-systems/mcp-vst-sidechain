# Sidechain

**An agent's control input for any plugin. One MCP endpoint, every VST.**

`mcp-vst-sidechain` is a generic [MCP](https://modelcontextprotocol.io) bridge that hosts any VST3/AU plugin
and exposes its parameters to an AI agent in realtime. Point it at your licensed Serum, Diva, Kontakt, or a
free effect, and an agent can enumerate the plugin's entire automatable control surface, read and set any
parameter live, play notes, and save or recall its full state, all over one MCP endpoint.

Sidechain talks to plugins only through the standardized VST3/AU API, the same way every DAW hosts thousands of
closed-source commercial plugins it has no source for. You bring your own licensed binaries; no source is
touched and nothing is redistributed.

> Status: early. The Go MCP layer and the C++ control listener are working and tested; the child-plugin host
> (the JUCE `AudioPluginFormatManager` wrapper) is an MVP. See the [roadmap](#roadmap).

## Why

Producers already automate plugins in a DAW. An LLM should be able to do the same thing conversationally:
"open the filter a little," "make this pad wider," "give me a darker version of this patch." That needs a
generic, realtime control bridge between an agent and an arbitrary plugin. Sidechain is that bridge.

The niche is contested but unowned. Existing attempts are prototypes, host-controllers, or heavy agent-DAWs;
none is a polished, standalone, cross-platform generic bridge, and none has a token-efficiency story for the
huge parameter surfaces real plugins expose.

### The unfair advantage: GCF

A plugin with hundreds of parameters is a token nightmare to serialize into an agent's context as JSON.
Sidechain encodes the big read tool (`list_params`) as [GCF](https://github.com/blackwell-systems/gcf-go), a
token-compact wire format that is 50-92% smaller than JSON and comprehended zero-shot. `set_params` also takes
a GCF table as input, so authoring a whole patch is one compact call rather than one-per-parameter. Nothing
else in this space has this.

## How it works

Two layers, two languages, chosen for what each does best:

```
  agent  <--stdio MCP-->  Sidechain server (Go)  <--localhost TCP-->  host (C++/JUCE)  --hosts-->  VST3/AU
```

- **C++ / JUCE host** wraps one plugin via `AudioPluginFormatManager` and runs a `ControlListener` pointed at
  the hosted processor. JUCE is the mature, industry-standard plugin-hosting stack with real macOS AU support.
- **Go MCP server** speaks stdio JSON-RPC to the agent and forwards control over a localhost socket. It uses
  the official MCP go-sdk, ships as a single binary, and GCF-encodes large payloads via `gcf-go`.

Audio never crosses the process boundary. Parameter control runs at agent speed (tens of ms), not audio rate,
so the localhost IPC carries only control messages and has zero effect on the sound. Full detail:
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Tools

| Tool | What it does |
|---|---|
| `list_params` | The plugin's automatable parameters (id/label/type/range/choices/default). GCF-encoded. |
| `get_param` | One parameter's definition + current value (real units + normalized). |
| `set_param` | Set one parameter by real value, normalized 0..1, or choice name. Validated + clamped. |
| `set_params` | Set many params in one call, from a JSON array or a token-compact GCF table. |
| `connect_live` / `disconnect_live` | Attach to / detach from a running host so the above drive the live instance. |
| `play_note` / `all_notes_off` | Play a MIDI note (optionally auto-released) / panic. |

## Quickstart

### 1. Build the Go server

```bash
go build -o sidechain ./cmd/sidechain
```

### 2. Build the C++ host

```bash
cmake -S cpp -B cpp/build -DCMAKE_BUILD_TYPE=Release
cmake --build cpp/build --target sidechain-host
```

(The first configure fetches JUCE via FetchContent, so it is slow. VST3 hosting works everywhere; AU is macOS
only.)

### 3. Load a plugin and start the host

```bash
./cpp/build/sidechain-host_artefacts/sidechain-host \
    --plugin "/Library/Audio/Plug-Ins/VST3/YourSynth.vst3" \
    --catalog plugin-catalog.json \
    --port 51703
```

This loads the plugin, writes its parameter catalog to `plugin-catalog.json`, and starts the control listener
on `127.0.0.1:51703`.

### 4. Run the MCP server and point an agent at it

```bash
./sidechain --catalog plugin-catalog.json
```

Register `./sidechain` as an MCP server in your client (Claude Code / Desktop). Then, from the agent:
`connect_live` (dials the host on `51703`), `list_params` to see the surface, and `set_param` / `set_params`
to drive it live.

## Configuration

| Variable / flag | Default | Meaning |
|---|---|---|
| `--catalog` / `SIDECHAIN_CATALOG` | - | Parameter catalog JSON (emitted by the host). Required by the Go server. |
| `--plugin` (host) | - | Path to a `.vst3` or `.component` to load. |
| `--port` (host) / `connect_live` port | `51703` | Loopback control port. |
| `SIDECHAIN_MCP_FORMAT` | `gcf` | `json` forces plain-JSON tool output instead of GCF. |

## Using the library

The Go layer is importable. Any host that can provide a `ParamCatalog` and a `LiveEndpoint` gets the generic
tool surface for free:

```go
import sidechain "github.com/blackwell-systems/mcp-vst-sidechain"

srv := mcp.NewServer(&mcp.Implementation{Name: "my-bridge"}, nil)
sidechain.RegisterParamTools(srv, myCatalog, func() sidechain.LiveEndpoint { return myEndpoint })
// ...register your own tools on srv too; they compose on one server.
```

The C++ `sidechain::ControlListener` is a single header: drop it into any JUCE plugin or app, construct it with
a `juce::AudioProcessor&` + `juce::MidiKeyboardState&` + a port, and it exposes that processor over the same
wire protocol.

## Roadmap

- [x] Generic MCP param tools (list/get/set, batch set) with GCF encoding + JSON fallback.
- [x] C++ `ControlListener` (loopback control, message-thread apply, opaque full-state round-trip).
- [x] Headless child-plugin host MVP (load a VST3/AU, enumerate its catalog, serve control).
- [ ] Plugin scanning / discovery (list installed plugins, not just load-by-path).
- [ ] Crash sandboxing / isolation for misbehaving plugins.
- [ ] Parameter-name normalization + an LLM semantics layer for plugins with poor metadata.
- [ ] Preset/program access (enumerate + recall a plugin's own presets by index).
- [ ] An in-DAW wrapper-plugin shape (single-binary, embedded control) alongside the headless host.

## License

MIT. Copyright (c) 2026 Dayna Blackwell. The realtime control substrate and Go MCP forwarder here originated in
the author's own synth work and are published as a standalone, generic OSS bridge.
