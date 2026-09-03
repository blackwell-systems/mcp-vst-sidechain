#!/usr/bin/env bash
# build-embedded.sh - build the single-file `sidechain` binary with the host embedded (go:embed).
#
# It builds the C++ host, copies it to internal/hostbin/payload, then `go build -tags embedhost` so the resulting
# Go binary carries the host inside it and self-extracts at startup (see internal/hostbin, managed.go). A normal
# `go build` (no tag) stays two-binary and discovers the host via --host-bin / PATH.
#
#   scripts/build-embedded.sh [output-path]        (default: ./sidechain)
#
# Env: VERSION (stamped into main.Version, default "dev"), SKIP_HOST_BUILD=1 (reuse an already-built host).
set -euo pipefail

cd "$(dirname "$0")/.."
OUT="${1:-./sidechain}"
VERSION="${VERSION:-dev}"
HOST_ART="cpp/build/sidechain-host_artefacts/Release/sidechain-host"
[ "$(uname -s)" = "MINGW"* ] || true   # keep bash happy on odd shells

# 1. Build the host (unless reusing a prior build).
if [ "${SKIP_HOST_BUILD:-}" != "1" ]; then
  echo "==> building sidechain-host"
  cmake -S cpp -B cpp/build -DCMAKE_BUILD_TYPE=Release >/dev/null
  cmake --build cpp/build --config Release --target sidechain-host
fi

# Resolve the built host path (Windows adds .exe; some generators nest a config dir).
HOST=""
for cand in "$HOST_ART" "$HOST_ART.exe" \
            "cpp/build/sidechain-host_artefacts/sidechain-host" \
            "cpp/build/sidechain-host_artefacts/sidechain-host.exe"; do
  if [ -f "$cand" ]; then HOST="$cand"; break; fi
done
if [ -z "$HOST" ]; then
  echo "error: could not find the built host under cpp/build (looked for $HOST_ART[.exe])" >&2
  exit 1
fi

# 2. Copy the host into the embed slot.
echo "==> embedding $HOST ($(wc -c < "$HOST" | tr -d ' ') bytes)"
cp "$HOST" internal/hostbin/payload

# 3. Build the single-file Go binary with the embed tag (strip symbols for a smaller release binary).
echo "==> go build -tags embedhost -o $OUT"
go build -tags embedhost -ldflags "-s -w -X main.Version=$VERSION" -o "$OUT" ./cmd/sidechain

echo "==> done: $OUT (single file; run with --plugin <path>, no --host-bin needed)"
