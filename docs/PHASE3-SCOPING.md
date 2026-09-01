# Phase 3 scoping: the persistent semantic store

Status: design, not yet implemented. This turns the Phase-3 sketch into a buildable spec by resolving each open
decision with a recommended default, defining the data model / tool schemas / lifecycle / tests, and naming the
one prerequisite. See [ARCHITECTURE.md](ARCHITECTURE.md) for the system and the Phase-1 inference this builds on.

## Goal

Persist, per plugin, everything the bridge and the agent learn about a plugin's parameters, so that:

- Probing (the getText sweep behind `describe_param`) is paid once ever, not once per session.
- The agent's own semantics (what a param *means*: role, aliases, polarity) survive restarts and accumulate.
- Phase 4 (intent -> params, e.g. "make it brighter") has a durable map to reason over.

## Non-goals

- No model in the bridge. The bridge caches and serves; the agent (an LLM) supplies the semantics. This is the
  load-bearing principle: it keeps the bridge thin and format-agnostic.
- No closed, bridge-enforced role ontology (see "Roles" below for why).

## Principle: two orthogonal equivalence axes

A parameter is classified on two independent axes. Keeping them separate is deliberate: `LFO 1 Rate` and
`Filter Cutoff` share a *behavior* class (both log-frequency in Hz) without sharing a *role*.

1. **Behavior class (derived, deterministic).** A signature computed from the Phase-1 `ParamInference`:
   `type : curve : unit [: bipolar]`. Examples: `float:log:hz`, `float:linear:db:bipolar`, `float:exp:s`,
   `float:linear:unitless`, `discrete:enum`. It is recomputed from a probe, never hand-authored, and it is what
   lets a manipulation be generic ("nudge any `float:log:hz` param up 15% of its log range"). Stored for query
   and for Phase-4 macros; it is NOT a fixed enum in code, just a normalized string built from the inference.
2. **Role class (agent-authored, soft).** A free-form string the agent assigns (`filter.cutoff`, `amp.attack`).
   Equivalence is string equality: the agent reuses `filter.cutoff` across Serum, Diva, Surge, and cross-plugin
   consistency falls out. The bridge does NOT enforce a vocabulary; a suggested starter set ships as an appendix
   (convention, not a validated enum), so exotic params (`wavetable position`, `FM ratio`) are never forced into
   an ill-fitting bucket.

## Plugin identity and the fingerprint (equivalence on plugins)

The store is keyed by a **fingerprint**: an equivalence relation over plugin instances. Two instances are "the
same" for cache reuse iff their fingerprint matches.

- **Fingerprint =** `sha256(name | manufacturer | format | sortedParamIDs | paramCount)`.
- **Version is recorded but NOT in the key** (default). A minor version bump that leaves the parameter surface
  unchanged reuses the cached semantics; a bump that changes the surface changes `sortedParamIDs`/`paramCount`
  and therefore the fingerprint, yielding a fresh entry. This is the invalidation policy, expressed as an
  equivalence class: surface-identical instances are equivalent.
- Entries are keyed by fingerprint in one store, so a surface change is non-destructive: it creates a new key and
  leaves the old entry intact (the agent may choose to migrate annotations).

### Prerequisite (C++)

The catalog JSON does not currently carry plugin identity (only `stateRootTag`, `stateVersion`, `count`,
`params`). `Host::enumerateCatalog` must emit identity from the loaded `juce::PluginDescription`:
`name`, `manufacturerName`, `version`, `pluginFormatName`, `uniqueId`. `Catalog` (Go) gains matching fields, and
`loadCatalogJSON` parses them. Small, localized change; it is the only prerequisite.

## Data model (stored per param)

```jsonc
{
  "fingerprint": "sha256:...",
  "plugin": { "name": "Surge XT", "manufacturer": "Surge Synth Team",
              "format": "VST3", "version": "1.3.4", "uniqueId": 123456 },
  "params": {
    "<paramID>": {
      // raw (from the catalog, for reference)
      "label": "A Filter 1 Cutoff",
      // derived by Phase 1 (persisted so probing is one-time-ever)
      "inference": { "numeric": true, "unit": "hz", "realMin": 13.75, "realMax": 25087.71,
                     "bipolar": false, "curve": "exponential", "fit": {"model":"exp","maxRelErr":0.0001} },
      "behaviorClass": "float:log:hz",          // derived signature (axis 1)
      // agent-authored (axis 2); all optional, merge-updated
      "role": "filter.cutoff",
      "aliases": ["brightness", "vcf freq"],
      "polarity": "higher = brighter",
      "section": "Filter 1",
      "confidence": 0.9,
      "notes": ""
    }
  }
}
```

`inference` + `behaviorClass` are auto-derived (populated on probe). Everything from `role` down is agent-authored
and merge-updated. A param may have inference without a role (probed but not yet named), or a role without
inference (annotated headless before probing).

## Storage

A **directory of per-fingerprint files**, NOT one shared file. One file per plugin: `<dir>/<fingerprint>.json`.

- **Location:** `--semantic-dir` flag / `SIDECHAIN_SEMANTIC_DIR` env, defaulting to a per-user cache directory
  (`$XDG_CACHE_HOME/sidechain`, or the OS equivalent) so it persists and is reused regardless of working
  directory.
- **Atomic writes:** write a temp file in the directory and rename over the target (atomic on the same
  filesystem), so a file is never torn or half-written, even on a crash mid-write.
- Per-plugin files, keyed by the surface fingerprint, mean a surface change (a new fingerprint) is naturally a
  new file, and unrelated plugins never share a file.

### Concurrency

Multiple `sidechain` processes may run at once (one per hosted plugin, or several agents). The per-fingerprint-
file layout is what makes this safe.

- **Within a process:** writes happen under the session mutex, serialized; a single server touches only its own
  plugin's file.
- **Across processes, different plugins:** they write different files and never contend. (This is the case the
  earlier one-shared-file design would have raced on with a read-modify-write clobber, and the reason it was
  rejected.)
- **Across processes, the same plugin (rare):** handled by (a) the atomic temp+rename, so the file is never
  corrupted, and (b) **merge-on-write**: before writing, re-read the file and merge (union of params, newest-wins
  per field), so two simultaneous writers to the identical plugin at worst lose a same-param/same-field edit,
  never wholesale-clobber the other's work.
- An advisory per-file lock (`flock`) is a deliberate NON-goal up front (cross-platform lock behavior is fiddly);
  add it only if real same-plugin contention is ever observed. There is no global lock because there is no global
  file.

## Lifecycle

- **Load** on `NewServer`/`Run`: read the store, and if an entry's fingerprint matches the loaded catalog,
  populate the in-memory `session.infer` cache and the annotations from it. The `s.infer` map becomes a view
  backed by the store.
- **Write** after each `annotate_params`, and after each `describe_param` probe (so inferences persist and a
  future session skips the sweep). Write-through is simplest at agent speed. Each write is read-merge-write on the
  plugin's fingerprint file, then an atomic rename (see Concurrency).
- **connect_live interaction:** today `connect_live` clears `s.infer` (a new instance may differ). With the store,
  reconnecting to the SAME fingerprint reloads persisted inferences instead of forcing a re-probe; a probe still
  refreshes on demand.
- **Headless:** annotations need no live plugin (the agent can annotate straight from the catalog). Inferences
  need a probe (live). The store loads and saves headless regardless.

## Tool surface

- **`annotate_params`** (new). Input: `params: [{ id, role?, aliases?, polarity?, section?, confidence?, notes? }]`.
  Merge semantics: only provided fields are updated; omitted fields are preserved. Persists. This is how the
  agent teaches the bridge; the bridge never infers these itself.
- **`describe_param`** (extend). If the store already has this param, return the cached inference + behavior class
  + annotations WITHOUT re-probing. Otherwise probe (when live), cache, persist. Output gains `behaviorClass` and
  any agent annotations.
- **`get_semantic_map`** (new, optional). Return the whole current-plugin entry (all params: inference + behavior
  class + annotations) in one call, GCF-encoded like `list_params`. This is the primary read for Phase 4.
- **`forget_semantics`** (new, optional, low priority). Drop the current fingerprint's entry.

## Test plan (all headless, fake host / synthetic catalogs)

- Fingerprint stability and equivalence: same surface -> same key; changed `paramIDs`/`count` -> different key;
  version bump alone -> same key.
- Store round-trip: write, read back, field-for-field; atomic-write leaves no partial file.
- Merge semantics of `annotate_params`: partial update preserves untouched fields.
- `describe_param` recalls from the store without re-probing: assert the fake host receives NO sweep on the second
  describe of a param already in the store (reuse the sweep-counter pattern from `TestRealSetCachesInference`).
- Behavior-class derivation: each `ParamInference` shape maps to the expected signature string.
- Headless annotate (no live endpoint) then persist and reload.
- Non-destructive invalidation: a surface change creates a new entry and leaves the old one intact.

## Open decisions (yours to confirm)

1. **Store directory location:** a per-user cache directory (recommended: `$XDG_CACHE_HOME/sidechain` or OS
   equivalent, persists and is reused everywhere) vs a directory next to the catalog. Either way it is a
   directory of per-fingerprint files, configurable via `--semantic-dir`/`SIDECHAIN_SEMANTIC_DIR`.
2. **Ship `get_semantic_map` now or defer to Phase 4?** Recommended: ship it now; it is small and Phase 4 needs it.
3. **Version in the fingerprint?** Default: no (surface-hash equivalence). Change only if you want every version
   bump to force re-annotation.

## Implementation sequence

1. C++ + Go: emit and parse plugin identity in the catalog (the prerequisite).
2. Go: `semantic.go` with the store type, fingerprint, load/save (atomic), behavior-class derivation.
3. Go: back `session.infer` with the store; persist on probe; reload on connect for a matching fingerprint.
4. Go: `annotate_params`, extend `describe_param`, add `get_semantic_map`.
5. Tests per the plan above.
6. Docs: fold the store + tools into ARCHITECTURE.md and the tool table; note the store in TESTING.md.

## Appendix: suggested role vocabulary (convention, not enforced)

A starting set for cross-plugin consistency. Free-form; the agent may use any string, but reusing these keeps
Phase-4 macros portable.

```
osc.*        : osc.pitch, osc.tune, osc.shape, osc.level, osc.type, osc.pulsewidth, osc.sync, osc.unison
filter.*     : filter.cutoff, filter.resonance, filter.type, filter.keytrack, filter.drive, filter.env_amount
env.*        : env.attack, env.decay, env.sustain, env.release, env.velocity
lfo.*        : lfo.rate, lfo.depth, lfo.shape, lfo.sync
amp.*        : amp.gain, amp.pan, amp.level
fx.*         : fx.mix, fx.feedback, fx.time, fx.drive, fx.width, fx.tone
global.*     : global.volume, global.tune, global.glide, global.voices
mod.*        : mod.source, mod.depth, mod.dest
```
