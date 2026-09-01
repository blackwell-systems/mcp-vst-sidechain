# Concurrency

Concurrency is a core property of Sidechain, not an afterthought. The product is a control substrate that
multiple controllers drive at once: several agents, an agent alongside a plugin's own GUI, multiple tools. This
document is the contract. Every change is checked against the invariants here, and the design assumes the full
target (multiple concurrent controllers of one plugin instance) even where it is implemented in stages.

Peer documents: [ARCHITECTURE.md](ARCHITECTURE.md) (the system), [TESTING.md](TESTING.md) (how it is verified),
[PHASE3-SCOPING.md](PHASE3-SCOPING.md) (the semantic store, whose per-fingerprint storage is part of this model).

## The layers and their concurrency invariants

The system is a stack of threads and processes. Each layer has one rule that must hold.

| Layer | Concurrency rule (invariant) |
|---|---|
| Audio thread (`processBlock`) | Lock-free, no allocation, no blocking, ever. Reads parameter atomics only. Sacred. |
| Message thread (JUCE) | The SINGLE point where mutations apply (`setValueNotifyingHost`, state, MIDI). All writes serialize here. |
| Control plane (host sockets) | May use locks freely: it runs at control rate (tens of ms), never on the audio thread. |
| Go server (per process) | Reads may run concurrently; mutations serialize. The socket round-trip is atomic per request. |
| Semantic store (Phase 3) | A directory of per-fingerprint files; atomic temp+rename; merge-on-write. No global file, no global lock. |

Two of these are already true and load-bearing: the **audio thread is lock-free**, and the **message thread is
the single applier** of every mutation. The target model builds on both without weakening either.

## The target model: multiple concurrent controllers of one instance

Any number of controllers connect to one host hosting one plugin. All of them can read and drive it at once, and
each sees the others' changes. Correctness comes from one idea: **who may enqueue a command becomes many; where a
command applies stays one (the message thread).** Multi-controller does not add write concurrency to the DSP or
parameter state; it adds producers to a queue drained by a single consumer.

### Host: many connections, one applier

- The accept loop spawns a handler per connection instead of serving one client at a time. A thread-safe client
  registry tracks connected controllers (add on accept, remove on disconnect).
- The command queue goes single-producer/single-consumer to **multi-producer/single-consumer**: many socket
  handlers enqueue, the message thread drains. Recommended: one command queue guarded by a short control-plane
  lock (a lock is fine here, it is not the audio thread). Alternative: a per-client lock-free SPSC queue plus a
  registry the drain iterates. The queue carries the originating client id.
- **Reads need no queue.** `get_param` snapshots the parameter atomic from any thread, so concurrent readers are
  already safe. State reads (`get_full_state`) still route through the message thread, because
  `getStateInformation` is not safe to call concurrently with processing.
- **Conflict policy: last-writer-wins per parameter,** in message-thread drain order. Deterministic and benign at
  control rate. No per-parameter locks or leases in v1 (that is a later, optional refinement).

### Visibility: change notifications (bidirectional protocol)

Multi-controller requires that a change by controller A (or by the plugin's own GUI or host automation) becomes
visible to controller B. The host listens for parameter changes (`AudioProcessorParameter::Listener` /
`audioProcessorParameterChanged`) and pushes an event to every connected client:
`{ event: "param_changed", param, normalized, value, text, by: <clientID> }`.

This evolves the wire protocol from strict request/response to a **multiplexed async protocol**: replies are
correlated to requests by `id` (the id, currently decorative, becomes load-bearing), and server-pushed events
arrive unsolicited. A controller may ignore events attributed to its own `clientID` (echo suppression).

### Go client: async reader and finer locking

- The `liveClient` gains a single background reader goroutine that demultiplexes the socket: replies route to the
  waiting caller by `id`, events route to a subscriber channel. This replaces the current one-reply-per-request
  read, which cannot receive unsolicited events.
- The session lock becomes finer-grained: concurrent reads (`get_param`, `describe_param`) proceed without
  blocking each other or writes; mutations serialize. (Within one Go server there is one agent; the multi-
  controller concurrency lives at the host across connections.)

### Identity

Each connection gets a `clientID`, assigned by the host at the `connect_live` handshake and returned to the
client. It attributes change events, scopes logging, and enables echo suppression. It is the "who is controlling"
concept the current single-endpoint design lacks.

## Invariants (the contract)

Every change must preserve all of these:

1. The audio thread never takes a lock, allocates, or blocks.
2. Every parameter/state/MIDI mutation applies on the message thread, one at a time.
3. Control-plane locks are allowed; they must never be held across an audio-thread boundary.
4. Reads (parameter atomics) are safe from any thread and never block a writer.
5. Concurrent writers resolve last-writer-wins in message-thread order; no lost-update corruption, no deadlock.
6. The semantic store never uses a global file or global lock: per-fingerprint files, atomic writes,
   merge-on-write.
7. A slow or dead controller cannot stall another: per-connection handlers, bounded queues, no shared blocking.

## Phased implementation path

The invariants hold at every step; capability grows.

- **C0:** single controller (the original design). Done.
- **C1: many connections. DONE.** The host accepts many connections, each on its own handler thread; commands go
  through a mutex-guarded MPSC queue drained by the single message thread; each request carries its own
  Completion (replacing the one shared applied-event and single scratch slots that assumed one client); and each
  connection gets a `clientID` at the ping handshake. Multiple controllers drive one instance concurrently;
  visibility is still poll-based (re-read to observe others). Covered by the gated `TestMultiClientLive`.
- **C2: change notifications. DONE.** The host registers as an `AudioProcessorParameter::Listener` and pushes a
  `{ event: "param_changed", param, normalized, value, text, by }` to every connected controller when a param
  moves, whatever the source (another controller, the plugin's own editor, host automation). The change is
  broadcast on the message thread when it originates there (attributed to the applying controller via `by`), and
  deferred through an atomic dirty flag + `triggerAsyncUpdate` when it arrives off-thread, so the audio path
  never takes the broadcast (invariant 1 preserved). The wire protocol is now multiplexed async: a reply carries
  the request `id` (the id, formerly decorative, is now load-bearing), an event carries none. The Go `liveClient`
  gained a background reader that demultiplexes replies (routed to the waiting caller by `id`) from events
  (routed to `Events()`), with per-connection single-writer outbound queues on the host so a slow reader cannot
  stall the applier (invariant 7). Covered by the gated `TestChangeNotifications` (A sets, B receives the event,
  attributed to A). Conflict policy stays **last-writer-wins in message-thread drain order** (see below).
- **C3: governed convergence + refinements.** Two strands. (a) **Conflict engine.** LWW is correct for
  independent continuous params but says nothing about *coordination* state that carries invariants (a
  mono/poly/legato mode gate, an active-voice budget, a routing enum whose combinations are constrained). That
  small, discrete, bounded slice wants convergence-with-invariants rather than raw last-writer-wins: the planned
  engine is [gsm](https://github.com/blackwell-systems/gsm) (Governed State Machines: compensation-based
  convergence over discrete/bounded state, verified by exhaustive enumeration at build time, an alternative to
  CRDT/consensus). gsm governs only that invariant-bearing coordination state, NOT the hundreds of continuous
  params (it cannot model float), which remain LWW. The conflict tier is pluggable behind the message-thread
  applier precisely so this can drop in without touching C1/C2. (b) **Refinements:** echo-suppression tuning,
  optional per-parameter ownership/leases, richer events (state loaded, notes), backpressure policy.

### C3 sequencing (why the conflict engine lands later, not now)

C3 is deliberately unbuilt. The governed coordination state it protects (leases, held-note ownership, a
scene/patch generation counter, mutually exclusive global gates) does not exist yet, and building a conflict
engine before the real conflicts appear tends to model the wrong invariants. The order of operations:

1. **Govern nothing until a real conflict forces it.** C2's last-writer-wins is correct for the continuous
   params and stays that way. Ship multi-controller, watch what two controllers actually collide on.
2. **When the first genuine invariant appears, model it in ~20 lines first:** a pure `reduce(state, cmd)`, an
   `ok(state)` predicate, and a deterministic `repair(state)`, over a small discrete/bounded state (enums, bools,
   bounded ints only). The conflict tier is then guarded transitions on the single applier: compute the next
   state, and either **reject** (return a typed error to the originating controller, state unchanged) or
   **compensate** (commit `repair(reduce(...))` so the write lands but drags dependent state with it). Pick per
   field: reject where a controller must know it lost (lease acquisition), compensate where silent convergence is
   fine (a mode gate). This needs no external engine.
3. **Verify by exhaustive enumeration in a test.** Because the governed state is finite and small, a DFS over
   (reachable states x command alphabet) that asserts `ok` after every `repair(reduce(...))` reproduces gsm's
   build-time guarantee ("no reachable state violates an invariant") as an ordinary test, and catches a partial
   `repair` immediately. Run it in CI.
4. **Adopt gsm when that model outgrows hand-rolling,** i.e. when there are enough invariants and compensations
   that its declarative ergonomics and reusable verified engine pay for the dependency. At that point the move is
   porting a proven model into gsm, not guessing at one.

The reason this works without reaching for CRDTs or consensus: there is ONE host, ONE applier thread, and one
copy of the state (invariant 2). C3 is not a distributed-convergence problem; it is keeping a small discrete
invariant-bearing state consistent under a serial applier where per-variable LWW is too coarse. If the scope ever
became multi-host (replicated governed state, partitions), that reframe collapses and the honest tools become
CRDTs (merge-friendly bits) and consensus/Raft (strict exclusivity). That is explicitly a non-goal (see below),
and gsm does not target that case either.

## Testing (a first-class category)

Concurrency gets its own test level, run under the race detector:

- N concurrent controllers driving one host: no torn state, no deadlock, last-writer-wins is deterministic.
- Connect/disconnect churn while others drive: clean handoff, no stall (the failure mode that produced the
  one-client reconnect bug).
- Change-notification delivery: a set by one controller is observed by another (C2+).
- A wedged controller (stops reading) does not block others (invariant 7).
- The `fakeHost` and Go client gain multi-client support so these run headless.

## Non-goals

- No changes to the realtime audio path. It is correct; the work is entirely in the control plane, the protocol,
  and identity.
- No distributed / multi-host orchestration (one host still hosts one plugin; "concurrency" here is many
  controllers of that one instance, plus safe multi-process use of the shared store).
- No per-parameter locking in v1. Last-writer-wins is the model; leases are a C3 option only if a real use case
  demands exclusivity.
