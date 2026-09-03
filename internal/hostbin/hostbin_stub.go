//go:build !embedhost

package hostbin

// Bytes returns nil: no host binary was embedded (a normal build, without -tags embedhost). Managed mode then
// discovers the host via --host-bin, next to the executable, or PATH.
func Bytes() []byte { return nil }
