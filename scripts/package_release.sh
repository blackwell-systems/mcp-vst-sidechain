#!/usr/bin/env bash
# package_release.sh - assemble ONE ready-to-run bundle per platform for a tagged release.
#
# The release ships a SINGLE self-contained binary: the native JUCE host is embedded into the Go binary via
# go:embed (see scripts/build-embedded.sh, internal/hostbin) and self-extracts to a cache dir on first run, so
# there is no separate sidechain-host to keep beside it. Layout inside the archive:
#
#   sidechain_<version>_<os>_<arch>/
#     sidechain[.exe]
#     NOTICE.txt
#
# The archive is .tar.gz everywhere except Windows, which gets a .zip. Runs on the Linux, macOS, and Windows
# (Git Bash) runners, so it stays POSIX-portable (no GNU-only flags).
#
# Usage:
#   scripts/package_release.sh <version> <os> <arch> <go-bin> <out-dir>
#     version   the tag without the leading v (e.g. 0.1.0)
#     os        linux | macos | windows (goes into the archive name)
#     arch      amd64 | arm64 (goes into the archive name)
#     go-bin    path to the built single-file sidechain (sidechain.exe on Windows)
#     out-dir   directory to write the archive into (created if missing)
#
# Prints the archive filename (basename) on the last line of stdout so the caller can capture it.

set -euo pipefail

version="${1:?version}"
os="${2:?os}"
arch="${3:?arch}"
go_bin="${4:?go-bin}"
out_dir="${5:?out-dir}"

test -f "$go_bin" || { echo "go binary not found: $go_bin" >&2; exit 1; }

mkdir -p "$out_dir"
# Absolutize the output dir so the zip subshell (which cd's elsewhere) still writes to the right place.
out_dir="$(cd "$out_dir" && pwd)"

stem="sidechain_${version}_${os}_${arch}"
stage="$(mktemp -d)/${stem}"
mkdir -p "$stage"

# Keep the on-disk binary name stable (sidechain[.exe]) regardless of the source path.
if [ "$os" = "windows" ]; then
  cp "$go_bin" "$stage/sidechain.exe"
else
  cp "$go_bin" "$stage/sidechain"
  chmod +x "$stage/sidechain"
fi

# Short NOTICE shipped in every bundle: how to clear macOS quarantine and how to register the server in an MCP
# client. Kept plain-text and dependency-free (no em dashes, per repo style).
cat > "$stage/NOTICE.txt" <<EOF
Sidechain ${version} (${os}/${arch})
====================================

This bundle is a SINGLE self-contained binary. The native JUCE plugin host is embedded inside it and extracts
itself to a cache directory on first run, so there is nothing else to install or keep beside it.

  sidechain        the Go MCP server (stdio), with the host embedded. Register this in your MCP client.

Quickstart (managed mode)
-------------------------
Point the server at a plugin and it extracts and spawns the embedded host, waits for its catalog, auto-connects,
and serves MCP on stdio:

  ./sidechain --plugin "/path/to/YourPlugin.vst3"

On macOS, AU plugins can also be addressed by identifier, e.g. --plugin "AudioUnit:Effects/aufx,dcmp,appl".

macOS quarantine
----------------
Downloaded binaries are quarantined by Gatekeeper and will be blocked on first run. Clear the attribute on the
whole unpacked directory once:

  xattr -dr com.apple.quarantine ./${stem}

(Signing and notarization are not yet applied to these bundles.)

Register in an MCP client
-------------------------
Claude Code:

  claude mcp add sidechain -- /absolute/path/to/${stem}/sidechain --plugin "/path/to/YourPlugin.vst3"

Claude Desktop (claude_desktop_config.json):

  {
    "mcpServers": {
      "sidechain": {
        "command": "/absolute/path/to/${stem}/sidechain",
        "args": ["--plugin", "/path/to/YourPlugin.vst3"]
      }
    }
  }

Self-test
---------
Verify the shipped bytes against a plugin without an MCP client:

  ./sidechain --plugin "/path/to/YourPlugin.vst3" --selftest

Exit code 0 means the server extracted and spawned the host, enumerated the catalog, connected, and set a
parameter.
EOF

archive_base="${stem}"
if [ "$os" = "windows" ]; then
  archive="${archive_base}.zip"
  # Zip from the stage's parent so the archive contains the top-level ${stem}/ directory. GitHub's windows-latest
  # Git Bash does not ship `zip`, so prefer it when present (portability) and fall back to 7z, which is
  # preinstalled on the Windows runner. Both produce a standard zip.
  ( cd "$(dirname "$stage")" && \
    if command -v zip >/dev/null 2>&1; then
      zip -qr "$out_dir/$archive" "$stem"
    elif command -v 7z >/dev/null 2>&1; then
      7z a -tzip -bso0 -bsp0 "$out_dir/$archive" "$stem" >/dev/null
    else
      echo "no zip or 7z available to create $archive" >&2; exit 1
    fi )
else
  archive="${archive_base}.tar.gz"
  tar -czf "$out_dir/$archive" -C "$(dirname "$stage")" "$stem"
fi

echo "packaged $out_dir/$archive" >&2
echo "$archive"
