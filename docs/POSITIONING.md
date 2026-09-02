# Positioning

Where Sidechain sits in the 2025-2026 landscape of AI-agent control of audio software, what it should and should
not try to be, and why. This is a strategy note, kept honest: it names where the project is genuinely
differentiated and where it merely duplicates work that already exists. Peer docs: [ARCHITECTURE.md](ARCHITECTURE.md)
(the system), [CONCURRENCY.md](CONCURRENCY.md) (the multi-controller layer this note leans on).

## The landscape (what already exists)

"Agents controlling audio via MCP" is an active, crowded trend, not a green field. The density is at the **DAW
layer**:

- **DAW MCP servers** drive a DAW the user already owns: AbletonMCP (several implementations), reaper-mcp (58
  tools), and servers for Logic / Pro Tools / FL / Bitwig. They are strong at arrangement: tracks, clips, MIDI,
  transport, mixing, rendering, device chains. Anthropic has also shipped Ableton/Splice connectors. This layer is
  well served.
- **Generic plugin-host bridges** are nearly empty. The one direct analog is **carla-mcp-server**: an MCP server
  for the Carla plugin host (loads VST2/3/AU/LV2), with realtime param get/set, MIDI, and state save/load. It is
  early (low-teens stars, small commit count), Python, depends on an external Carla install, is single-client, and
  has no parameter-semantics layer.
- **Text-to-patch / AI-to-synth** is mostly research and one-off ML tools (CTAG mapping text to synth params,
  SynthScribe, SerumRNN, MicroMusic audio-to-Vital), not reusable realtime control bridges.
- **Multi-agent control of one instance** is unclaimed in audio. Every multi-agent-MCP result is general
  orchestration (planner/coordinator, status-sharing, file-locking); nobody applies it to concurrent control of a
  single plugin or DAW instance.

## The architectural wall (the fact that decides the strategy)

One structural fact separates Sidechain from the DAW servers and dictates everything below:

> **Sidechain controls plugins IT hosts. DAW servers control plugins the DAW hosts. Neither can easily have the
> other.**

Sidechain's headless JUCE process cannot reach the synth loaded on a user's Ableton track, and a VST3 plugin
cannot introspect its neighbors inside a DAW. So the two are on opposite sides of a wall defined by "who hosts the
plugin." This is why the naive "just add a timeline and compete with the DAW servers" move is a trap (see
Non-goals): it would abandon our side of the wall for a war we cannot win.

But the wall cuts both ways, and the DAW servers are **weak exactly where we are strong**: they are arrangement-
first and thin on deep per-plugin parameter control (their scripting APIs surface plugins shallowly). Deep,
semantic, generic control of a single plugin's full parameter surface is our entire core and their worst sub-task.

## What Sidechain is (and is not)

Sidechain is **agent-native audio infrastructure**: the best possible agent control of a single instrument or
effect, and a building block other agent-audio pipelines depend on. It is **not** a DAW, and not an
end-user "producer in a chat window" app competing with Ableton.

The aggressive move is not to fight on arrangement. It is to own the thing arrangement tools are bad at, and make
the seam across the wall clean. Three plays, in order of how unclaimed they are:

1. **Own "AI sound designer for one instrument."** Be so good at deep semantic control of a single synth/effect
   (real-unit control, param roles, persistence, GCF token efficiency, multi-controller coordination) that when
   someone wants to DESIGN a sound (not arrange a song), they reach for Sidechain rather than fumble through a
   DAW's plugin API. This is a sub-turf adjacent to theirs where we have a structural advantage.
2. **Bridge with a preset/state handoff.** Design the patch in Sidechain's headless sandbox, export the plugin's
   opaque state (already round-tripped byte-exact via save_state/load_state), and drop it into the DAW instance.
   "Shape it here, place it there." This is the one integration the wall actually permits: it moves state, not live
   control, across the boundary.
3. **Be the plugin-depth layer in a multi-server agent stack.** An agent can run both Sidechain and a DAW server,
   using the DAW server for arrangement and Sidechain for deep plugin work. We do not beat AbletonMCP; we make the
   agent better than AbletonMCP-alone on the axis we own.

## Differentiated vs commodity (honest)

- **Commodity (others already do this):** load a plugin, set a parameter, play a note, save/recall state over MCP.
  carla-mcp does it for hosted plugins; every DAW server does it inside its DAW. Do not treat this as the pitch.
- **Differentiated (the moat, because it requires hosting the plugin ourselves):**
  - the **semantic layer** (probe value text to recover real units/range/curve; agent-authored roles; persistent
    per-fingerprint store) and **GCF** token efficiency, which address the huge-param-surface and
    no-semantics problems the DAW servers ignore;
  - **multi-controller coordination** (C1/C2/C3 edit leases + change events), unaddressed anywhere in audio;
  - a **self-contained, cross-platform, headless host** (no DAW, no external host install) with prebuilt bundles.

The moat items share a property: they only work because we host the plugin. That is the same wall that separates
us from the DAW servers, used in our favor.

## Non-goals (the guardrail)

- **Do not build a DAW.** No timeline, clips, multi-track arrangement, or mixing. The moment Sidechain grows a
  timeline it has crossed to the DAW servers' turf and abandoned its structural advantage. Arrangement is their
  strength; per-plugin depth is ours. Stay on our side of the wall and be untouchable there.
- **Do not chase feature parity with DAW MCP servers.** Win on depth, not breadth.
- **Multi-agent coordination is built but speculative in demand.** Keep it; do not lead the pitch with it until a
  real "two agents on one instrument" use case appears. Lead with the semantic/token layer, which is a universal
  pain today.

## Sources

Landscape read (lean pass, September 2026; directional, not exhaustive):
[Carla MCP](https://github.com/agrathwohl/carla-mcp-server),
[AbletonMCP](https://github.com/FabianTinkl/AbletonMCP),
[reaper-mcp](https://lobehub.com/mcp/bonfire-audio-reaper-mcp),
[The MCP Turn in Audio (Sonic Field)](https://sonicfield.org/ai-agents-enter-the-studio-the-mcp-turn-in-audio),
[Audio-production MCP servers (ChatForest)](https://chatforest.com/reviews/music-audio-production-mcp-servers/),
[CTAG / synth-patch AI notes](https://gist.github.com/0xdevalias/5a06349b376d01b2a76ad27a86b08c1b).
