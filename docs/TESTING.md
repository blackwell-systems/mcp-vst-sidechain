# Testing

Sidechain is tested in layers, fastest first. The default `go test ./...` needs no plugin, no socket to a real
host, and no network: it runs the pure logic, a fake-host loopback, and an in-memory MCP transport. A second,
opt-in layer drives real plugins end to end and runs only when you point it at a running host (locally) or in
the integration workflow (CI). This split keeps the everyday gate fast and deterministic while still proving the
whole stack against actual VST3/AU plugins.

Related: [ARCHITECTURE.md](ARCHITECTURE.md) describes the system these tests cover.

## The layers

| Layer | What it proves | Needs a plugin? | How it runs |
|---|---|---|---|
| 1. Pure unit | Catalog math, inference/curve-fit, sectioning, GCF, tool handlers, server wiring | No | `go test ./...` |
| 2. Fake-host loopback | The Go forwarder + tool handlers over a real socket, against an in-process fake speaking the wire protocol | No | `go test ./...` |
| 3. In-memory MCP transport | Tool registration, JSON arg unmarshaling, dispatch, GCF result encoding, through the real `mcp.Server` | No | `go test ./...` |
| 4. Real-plugin E2E (gated) | The C++ host + wire protocol + a real plugin: load, enumerate, drive, state, MIDI | Yes | gated by env vars; CI `integration.yml` |

Layers 1 to 3 are ordinary Go tests and always run. Layer 4 tests `t.Skip` unless their environment variables are
set, so the normal suite stays green with no plugin present.

### Layer 1: pure unit

No I/O. Covers `catalog.go` (clamp/normalize/choice, JSON load, filtering, `NewCatalog`, sections), `infer.go`
(value-text parsing, unit normalization, curve classification, `CurveFit` linear/exp/power, `NormForReal`,
inversion), `sections.go` (label-prefix derivation), `gcf.go` (env gating, structured-result fallback),
`paramtools.go` (`resolveReal`, `liveArg`, headless get/set, `set_params` skip reporting, `summary`), and
`server.go` (`loadCatalogFile`, `NewServer`). Files: `catalog_test.go`, `catalog_more_test.go`,
`infer_test.go`, `infer_more_test.go`, `sections_test.go`, `gcf_more_test.go`, `paramtools_more_test.go`,
`server_test.go`, `discrete_choice_test.go`.

### Layer 2: fake-host loopback (`live_test.go`)

`fakeHost` is a goroutine that accepts one client and answers the exact line-delimited JSON the C++
`ControlServer` speaks, backed by an in-memory value map. The session's live path (`connect_live`, `set_param`,
`get_param`, `describe_param`, `set_params`, note verbs, `save_state`/`load_state`, real-unit and discrete-by-label
control) is driven against it over a real TCP socket. This is "same bytes on the wire" as the C++ host without
needing a plugin. It renders different value-text shapes per fake param id (linear Hz, exponential, power,
toggle, unitless) so the inference and control paths get real inputs.

### Layer 3: in-memory MCP transport (`transport_test.go`)

Stands up the real `mcp.Server` over `mcp.NewInMemoryTransports()` and calls tools as a client would. This covers
the layer the handler-level tests skip: tool registration (`ListTools`), unmarshaling JSON arguments into the
input structs, dispatch, and result encoding including the GCF `list_params` path.

### Layer 4: real-plugin E2E (gated)

These drive a running `sidechain-host` with a real plugin loaded. Each test skips cleanly unless its env vars are
set. See "Running the gated tests locally" below.

| Test(s) | File | Env vars | Drives |
|---|---|---|---|
| `TestFullSurfaceSweep`, `TestStateRoundTrip`, `TestBatchSetParams`, `TestMidiSmoke` | `sweep_live_test.go` | `SIDECHAIN_SWEEP_PORT`, `SIDECHAIN_SWEEP_CATALOG`, `SIDECHAIN_SWEEP_MIDI` | Every param round-trips (no crash); opaque state save/load; batch set; MIDI ack |
| `TestInferLive`, `TestWiredLive` | `infer_test.go` | `SIDECHAIN_LIVE_PORT`, `SIDECHAIN_LIVE_CATALOG`, `SIDECHAIN_LIVE_PARAM` | Probe -> infer -> invert -> set real -> read back; the wired `describe_param`/`set_param real=` handlers |
| `TestE2ESurgeExtras` | `e2e_live_test.go` | as above plus `SIDECHAIN_LIVE_DISCRETE`, `SIDECHAIN_LIVE_TIME` | Discrete-hiding-as-float set-by-label; real-unit control on a steep curve (refine fallback) |
| `TestAULive` | `au_live_test.go` | `SIDECHAIN_AU_PORT`, `SIDECHAIN_AU_CATALOG`, `SIDECHAIN_AU_PARAM` | Load an AudioUnit by component identifier and drive it |
| `TestScanPowerFits` | `scan_test.go` | `SIDECHAIN_SCAN_PORT`, `SIDECHAIN_SCAN_CATALOG` | Survey every param for a clean analytic power fit (a diagnostic, not an assertion) |

## Running everything locally

### The fast gate (no plugin)

```bash
go test ./...                                   # all non-gated tests; gated ones skip
go test -race -coverprofile=cover.out ./...     # what CI runs
go tool cover -func=cover.out | tail -1         # coverage total
gofmt -l .                                       # must be empty
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@v0.8.1 ./...   # pinned; required in CI
go run golang.org/x/vuln/cmd/govulncheck@latest ./...    # report-only in CI
```

### The gated real-plugin tests

Build the host, start it on a plugin, point the tests at it:

```bash
cmake -S cpp -B cpp/build -DCMAKE_BUILD_TYPE=Release
cmake --build cpp/build --target sidechain-host --config Release
HOST=cpp/build/sidechain-host_artefacts/Release/sidechain-host

# generic suite against any plugin
"$HOST" --plugin ~/Library/Audio/Plug-Ins/VST3/"Surge XT.vst3" --catalog cat.json --port 51999 &
SIDECHAIN_SWEEP_PORT=51999 SIDECHAIN_SWEEP_CATALOG=cat.json SIDECHAIN_SWEEP_MIDI=1 \
  go test -run 'TestStateRoundTrip|TestFullSurfaceSweep|TestBatchSetParams|TestMidiSmoke' -v .

# an AudioUnit by identifier (macOS)
"$HOST" --plugin "AudioUnit:Effects/aufx,dcmp,appl" --catalog aucat.json --port 52000 &
AU_PARAM=$(python3 -c "import json;print(json.load(open('aucat.json'))['params'][0]['id'])")
SIDECHAIN_AU_PORT=52000 SIDECHAIN_AU_CATALOG=aucat.json SIDECHAIN_AU_PARAM="$AU_PARAM" \
  go test -run TestAULive -v .
```

The `SIDECHAIN_LIVE_*` tests need specific param ids (a cutoff, a discrete param, a time param); resolve them
from the catalog JSON by label, as `integration.yml` does.

## CI

Three workflows, split by cost and blast radius.

- **`go.yml`** (required, fast): `gofmt` gate, `go mod tidy` drift gate, build, vet, `-race` + coverage, and a
  pinned **staticcheck** gate. A **govulncheck** job runs report-only (its findings are dominated by Go stdlib
  CVEs fixed in newer patch releases, so gating on it would red the build on toolchain drift, not our code).
- **`cpp.yml`** (best-effort): builds `sidechain-host` on ubuntu, macOS, and Windows (VST3 hosting everywhere; AU
  is macOS only). `fail-fast: false`. A report-only clang-tidy step lints our two translation units; warnings-as-
  errors is deferred until a clean baseline is confirmed.
- **`integration.yml`** (the real-plugin E2E): builds the host, downloads pinned free plugins, and drives each
  through the Layer-4 suite, on **macOS** and **Linux** (headless, under `xvfb-run`). The shared harness is
  `.github/scripts/drive_plugin.sh`.

### Required vs report-only

The integration matrix mixes hard assertions with report-only legs (`continue-on-error`), mirroring the
staticcheck/govulncheck pattern. A new or environment-fragile leg lands report-only so a wrinkle cannot red the
pipeline, and is promoted to required only after it is green across several runs.

| Plugin / format | macOS | Linux |
|---|---|---|
| Surge XT (synth) | required | required |
| TAL-NoiseMaker (synth) | not run (driven on Linux) | required |
| Dexed (FM synth) | not run (macOS release is AU-only) | required |
| Surge XT Effects (effect) | required | report-only (state does not restore headless there) |
| AudioUnit (Apple built-in) | required | not applicable |

The one report-only exception is honest: Surge Effects loads and drives under `xvfb` on Linux, but its state
round-trip does not restore headless there, unlike macOS. It is surfaced, not hidden.

A separate **concurrency leg (macOS, required)** drives one Surge host with several controllers at once and runs
`TestMultiClientLive` (C1: distinct identities, independent concurrent control, a state read during a set hammer,
clean disconnect) and `TestChangeNotifications` (C2: one controller's set is pushed to another as an attributed
`param_changed` event). See [CONCURRENCY.md](CONCURRENCY.md).

## Conventions (learned the hard way)

- **Gated tests must skip cleanly with no env set.** Never let a live test fail the default `go test`.
- **Every test that connects must disconnect.** The host now serves many connections at once (concurrency C1),
  so a lingering connection no longer blocks the next handshake, but a leaked connection still holds a handler
  thread and a socket and muddies the multi-controller identity/attribution assertions. Connect, assert
  `Connected LIVE`, then `defer s.handleDisconnectLive(...)` (or `defer lc.Close()`).
- **Run the state round-trip on a freshly loaded plugin.** `save_state`/`load_state` is meant to round-trip a
  natural patch. The full-surface sweep drives every param to extremes (a no-crash stress test) and leaves some
  plugins in an unnatural/modal state their own save/load will not reproduce. The harness runs the state test
  first, against the clean load, then lets the sweep churn.
- **`TestStateRoundTrip` asserts restore only on params that actually moved.** An immovable or plugin-computed
  pseudo-parameter reads back non-deterministically; asserting on it is a flake source.
- **When promoting a report-only leg, read the actual test result, not the step conclusion.** A
  `continue-on-error` step reports `success` even when the test inside failed. Promote only after confirming the
  test genuinely passed across runs.
- **The bridge is byte-exact; some plugins are not.** State is round-tripped as base64 of the plugin's own bytes,
  proven across the required legs. A plugin that does not faithfully restore its own state (async recall, modal
  routing) is a plugin property, kept report-only, not a bridge defect.
- **No em dashes** in test comments or strings (repo style).

## Adding a plugin to the E2E matrix

1. Add a download step in `integration.yml` (pin the version; verify the URL) that fetches the plugin's VST3 for
   the target OS into the plugins directory.
2. Add a `drive_plugin "<name>" "<path>" <port> <midi:0|1>` step. Start it **report-only** (`continue-on-error:
   true`, and `if: ${{ !cancelled() }}` on the Linux legs so every plugin still reports).
3. Watch a few runs. Promote to required by dropping `continue-on-error` once it is genuinely green.
