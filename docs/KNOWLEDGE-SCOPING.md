# Knowledge scoping: how the agent knows synthesis

Status: design. How sound-design expertise reaches the agent, so it can drive an arbitrary plugin like a producer
rather than a knob-randomizer. The answer is NOT a fine-tuned model: it is a LAYERED, GROUNDED, WRITE-BACK retrieval
architecture (a layered RAG) whose knowledge only ever PROPOSES, while the render/measure loop VERIFIES and the
semantic store PERSISTS. Peer docs: [PHASE3-SCOPING.md](PHASE3-SCOPING.md) (the store, the bottom layer, already
built), [RENDER-SCOPING.md](RENDER-SCOPING.md) (the verifier), [PHASE4-SCOPING.md](PHASE4-SCOPING.md) (the tune
loop), [ARCHITECTURE.md](ARCHITECTURE.md), [ROADMAP.md](ROADMAP.md).

## The problem

A frontier model brings real synthesis priors (FM theory, ADSR, filters, the "warm/bright/punchy" vocabulary), but
they are generic (not THIS plugin), qualitative (not numeric), and fallible (a plugin may not behave as expected).
To act like a producer the agent needs that knowledge (1) grounded to the specific plugin, (2) turned into concrete
param moves against objective measures, and (3) checked against what the plugin actually did. Fine-tuning bakes
knowledge into opaque, un-editable, model-locked weights that cannot be verified against the plugin in front of it,
and freezes at training time. A retrieval architecture keeps knowledge as inspectable data, rides frontier models,
is corrected by the loop, and ACCUMULATES across sessions. That is the bet.

## The layers

Top layers are the most general and reusable and let the agent reason about a plugin it has never seen; lower layers
are more specific; the bottom layer is the agent's own grounded memory. Each layer has its own retrieval key and
mechanism (this is not one vector index):

| Layer | Scope | Retrieval key | Mechanism | Static / live | Status |
|---|---|---|---|---|---|
| Paradigm theory | universal | paradigm class (from param-surface classification) | select-by-class lookup (no embeddings) | static, curated | design |
| Type recipes / intent -> mechanism | per paradigm | (paradigm, natural-language intent) | semantic search | static, curated | design |
| Interpretation heuristics | universal | measure name | small lookup | static, curated | design |
| Reference signatures | per target sound | a numeric measurement target | nearest-neighbor in MEASUREMENT space | static + accretive | design |
| Empirical per-plugin store | per plugin | plugin fingerprint + param id | deterministic exact-key KV | live, agent-written | IMPLEMENTED (Phase 3) |

### Paradigm theory (the reasoning frame)

A taxonomy of synthesis paradigms (subtractive, FM/PM, additive, wavetable, granular, physical modeling,
sample/rompler, hybrids) with, for each: how it makes sound, its signal-flow model, and a RECOGNITION FINGERPRINT
from the param surface (operators + ratios -> FM; a wavetable-position param -> wavetable; grain size/density ->
granular; osc + resonant filter + ADSR -> subtractive). This is what lets a cold, unseen plugin be reasoned about:
classify it, then apply building-block knowledge. Small and stable; a select-by-class lookup, not a vector search.

### Building-block glossary + intent -> mechanism map

Paradigm-agnostic primitives wired to the measures that reflect them: filter cutoff -> brightness
(`centroid_hz` / `high_db`); amp attack -> punch (`crest`); LFO -> destination -> movement (`modulation`); drive ->
harmonics + loudness. On top, a CROSS-PARADIGM intent -> mechanism map, because a perceptual intent has different
realizations per paradigm and the agent must pick the one this synth affords: "brighter" = cutoff up OR FM index up
OR more/higher partials OR wavetable position OR drive; "movement" = any LFO/env to any destination; "punch" =
faster amp attack. The general knowledge lists the mechanisms; the semantic map says which one exists here.

### Interpretation heuristics

How to read the objective numbers musically: centroid > 5 kHz on a bass = probably harsh; crest < 6 dB = squashed
or dull; low-band dominant + centroid ~200 Hz = a sub. Turns measurements into judgments. Small, high-leverage.

### Reference signatures (a bridge to taste without ML)

Target sounds encoded as objective MEASUREMENT fingerprints ("warm analog bass ~ low-band dominant, centroid
~300 Hz, crest ~10 dB"). "Make it warmer" becomes "tune toward this signature", and `tune_params` co-optimizes to
match it. Retrieval here is nearest-neighbor in measurement space, not text. This encodes aesthetic targets as
something measurable, the honest partial answer to the taste gap that stops short of a perceptual ML model.

### Empirical per-plugin store (the grounded memory, already built)

Phase 3. Per-plugin, per-param roles/aliases/polarity/behavior-class, keyed deterministically by a surface
fingerprint, derived by probing/rendering (grounded, not asserted), written back by the agent (`annotate_params`),
recalled without re-probing (`describe_param` / `get_semantic_map`). This is the least RAG-like and most valuable
layer: exact-key (determinism beats similarity for "which param is the cutoff"), and a LIVING memory that grows,
not a frozen corpus. It is what makes the whole thing accumulate: a plugin figured out once is remembered forever.

## The contract: propose, then verify, then persist

Every layer above the store only PROPOSES. Nothing retrieved is trusted until the render/measure loop checks it,
and only what worked is persisted. The first-contact flow on an unknown plugin:

```
classify  (paradigm theory: recognize the synth from its param surface)
  -> retrieve  (type recipe + intent->mechanism for the intent; any reference signature)
  -> ground    (map the mechanism to real param ids via the store / describe_param)
  -> propose   (set_param structural choices; choose a param + measure + direction to tune)
  -> tune      (tune_param / tune_params: the bounded search converges the measure)
  -> verify    (render + measure: did the sound move the intended way?)
  -> persist   (annotate_params writes the confirmed role/mapping back to the store)
```

The theory is the frame, the store is the memory, the loop is the check. This is why retrieval errors are safe: a
wrong recipe is caught by the measurement before it ships, and the store remembers the correction so the mistake is
not repeated. It is grounded RAG, retrieval with a verifier, which is the safety net text-only RAG lacks.

## Where it lives (the seam it must not cross)

All of this is AGENT-SIDE: knowledge is retrievable data (files, or small MCP resources/tools the agent queries),
never `if plugin == X` or `if filter` logic in the server. The bridge stays generic: it sets params and measures
buffers and knows nothing about "filters" or "FM". `get_semantic_map` / `describe_param` are already retrieval tools
over the bottom layer; the upper layers add sources (a paradigm/recipe resource, a signature matcher), composed by
the agent. Mechanism in the server, all synthesis meaning in agent-retrievable data.

## Design cautions

- **Do not jump to a vector DB.** The paradigm taxonomy is tiny and curated: a select-by-class lookup or prompt
  injection, no embeddings. Semantic search earns its keep only at the intent -> recipe layer once that corpus is
  large. Over-indexing early is the classic RAG over-build.
- **Keep the store exact-key.** Determinism beats similarity for param identity; do not fuzzy-match roles.
- **The loop and write-back matter as much as the corpora.** Imperfect retrieval is fine because the loop corrects
  it and the store remembers the correction. Invest there, not only in bigger knowledge files.
- **Curated per-plugin knowledge does not scale.** Hand-authored seeds for a few flagship plugins are a jump-start,
  not the strategy; the empirical store (self-grounding) is what covers the long tail.

## Relationship to fine-tuning

This layered, grounded, write-back RAG is the explicit alternative to fine-tuning a model. Same knowledge, but
inspectable, editable, model-portable, self-correcting via the loop, and accumulating across sessions instead of
frozen at training time. A fine-tune remains a possible LATER cost/latency optimization (distill a small local model
for the inner loop once dogfooding yields trajectory data), not a capability prerequisite, and not the way synthesis
knowledge enters the system.

## Non-goals / honest limits

- **Aesthetic taste.** Objective measures and reference signatures capture brightness, punch, movement, loudness,
  not "pleasing". A learned perceptual measure could close this later; it is deferred and out of scope here.
- **No ontology in the bridge.** Knowledge never becomes server branching logic; it stays data the agent reasons
  over.

## Implementation sketch

1. Author the paradigm theory + building-block/intent-mechanism map + interpretation heuristics as structured,
   retrievable data (start as a curated file / MCP resource; add semantic search only if the recipe corpus grows).
2. A lightweight paradigm CLASSIFIER (param-surface fingerprints) so the agent selects the right sub-knowledge.
3. Reference signatures as measurement templates + a nearest-signature matcher over render output.
4. The bottom layer (empirical store) and the verifier (render/tune loop) already exist; wire the upper layers to
   the same propose -> verify -> persist contract.
