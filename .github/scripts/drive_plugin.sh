# shellcheck shell=bash
# drive_plugin.sh - shared helper sourced by the plugin-drive steps of integration.yml (both macOS and Linux).
# Drives ONE hosted plugin through the generic, catalog-driven gated suite. Sourced fresh in each `run:` step
# because GitHub Actions steps do not share shell state.
#
#   drive_plugin <name> <plugin-path-or-id> <port> <midi:0|1>
#     starts the host on that plugin/port, waits for the catalog, asserts enumeration > 0, runs
#     TestFullSurfaceSweep/TestStateRoundTrip/TestBatchSetParams (+ TestMidiSmoke when midi=1) with the
#     SIDECHAIN_SWEEP_* gate pointed at that host, then kills the host. Returns non-zero on any failure.
#
# Environment:
#   SIDECHAIN_HOST_BIN  path to the built sidechain-host (required; differs per platform artefact layout).
#   SIDECHAIN_XVFB=1    wrap the host in `xvfb-run -a` (Linux: headless JUCE plugin hosting still needs an X
#                       server at plugin construction even though we never open an editor).

drive_plugin() {
  local name="$1" plugin="$2" port="$3" midi="$4"
  local host="${SIDECHAIN_HOST_BIN:?set SIDECHAIN_HOST_BIN to the sidechain-host path}"
  echo "::group::drive $name ($plugin) on :$port"
  local cat="cat-$port.json"

  local -a launch=("$host")
  if [ "${SIDECHAIN_XVFB:-}" = "1" ]; then launch=(xvfb-run -a "$host"); fi
  "${launch[@]}" --plugin "$plugin" --catalog "$cat" --port "$port" >"host-$port.log" 2>&1 &
  local pid=$!
  local rc=0

  for i in $(seq 1 40); do [ -s "$cat" ] && break; sleep 1; done
  if [ ! -s "$cat" ]; then
    echo "host for $name did not write a catalog:"; cat "host-$port.log" || true
    kill "$pid" 2>/dev/null || true; echo "::endgroup::"; return 1
  fi

  local count
  count=$(python3 -c "import json; print(json.load(open('$cat'))['count'])")
  echo "$name enumerated $count params"
  if [ "$count" -le 0 ]; then
    echo "$name enumerated zero params"; kill "$pid" 2>/dev/null || true; echo "::endgroup::"; return 1
  fi

  # State round-trip runs FIRST, against the freshly-loaded plugin. save_state/load_state is meant to round-trip
  # a NATURAL patch; the full-surface sweep below deliberately drives every param to extremes (a no-crash stress
  # test), which leaves some plugins in an unnatural/modal state their OWN save/load does not faithfully
  # reproduce. Testing state on the clean load keeps this a test of the bridge (byte-exact) rather than of a
  # plugin's tolerance for a maxed-out state. The sweep then churns freely since nothing after it reads state.
  SIDECHAIN_SWEEP_PORT="$port" SIDECHAIN_SWEEP_CATALOG="$cat" \
    go test -run 'TestStateRoundTrip' -v . || rc=$?

  local runexpr='TestFullSurfaceSweep|TestBatchSetParams|TestSectionLockstep'
  local midienv=""
  if [ "$midi" = "1" ]; then runexpr="$runexpr|TestMidiSmoke"; midienv="1"; fi
  SIDECHAIN_SWEEP_PORT="$port" \
  SIDECHAIN_SWEEP_CATALOG="$cat" \
  SIDECHAIN_SWEEP_MIDI="$midienv" \
    go test -run "$runexpr" -v . || rc=$?

  kill "$pid" 2>/dev/null || true
  echo "::endgroup::"
  return $rc
}
