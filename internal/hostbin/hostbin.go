//go:build embedhost

// Package hostbin optionally carries the sidechain-host binary embedded INTO the Go binary, so a release can ship
// as a single file instead of two version-matched artifacts. This file compiles only under `-tags embedhost`, after
// the built host has been copied to internal/hostbin/payload (see scripts/build-embedded.sh). A normal `go build`
// (no tag) uses hostbin_stub.go instead, embeds nothing, and managed mode falls back to --host-bin / PATH discovery.
package hostbin

import _ "embed"

//go:embed payload
var payload []byte

// Bytes returns the embedded host binary. Non-empty only in an -tags embedhost build.
func Bytes() []byte { return payload }
