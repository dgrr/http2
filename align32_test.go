//go:build 386 || arm || mips || mipsle || armbe || mips64p32 || mips64p32le

package http2

import "unsafe"

// On 32-bit platforms the first word of a value passed to a 64-bit atomic must
// be 8-byte aligned, or the operation panics at run time. The compiler only
// guarantees that for the first word of an allocated struct, so every field
// used with a 64-bit atomic has to sit at an offset that is a multiple of 8.
//
// These are array lengths, so a misaligned field is a compile error on the
// platforms that care. Building this package for 386 in CI is what enforces it.
var (
	_ [0 - unsafe.Offsetof(Conn{}.closed)%8]byte
	_ [0 - unsafe.Offsetof(Stream{}.window)%8]byte
)
