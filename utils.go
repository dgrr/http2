package http2

import (
	"fmt"
	"reflect"
	"sync"
	"unsafe"
)

// TODO: Needed?
var bytePool = sync.Pool{
	New: func() interface{} {
		return make([]byte, 128)
	},
}

func uint24ToBytes(b []byte, n uint32) {
	_ = b[2] // bound cfrecking
	b[0] = byte(n >> 16)
	b[1] = byte(n >> 8)
	b[2] = byte(n)
}

func bytesToUint24(b []byte) uint32 {
	_ = b[2] // bound checking
	return uint32(b[0])<<16 |
		uint32(b[1])<<8 |
		uint32(b[2])
}

func appendUint32Bytes(dst []byte, n uint32) []byte {
	dst = append(dst, byte(n>>24))
	dst = append(dst, byte(n>>16))
	dst = append(dst, byte(n>>8))
	dst = append(dst, byte(n))
	return dst
}

func uint32ToBytes(b []byte, n uint32) {
	_ = b[3] // bound checking
	b[0] = byte(n >> 24)
	b[1] = byte(n >> 16)
	b[2] = byte(n >> 8)
	b[3] = byte(n)
}

func bytesToUint32(b []byte) uint32 {
	_ = b[3] // bound checking
	n := uint32(b[0])<<24 |
		uint32(b[1])<<16 |
		uint32(b[2])<<8 |
		uint32(b[3])
	return n
}

// resize resizes b if neededLen is granther than cap(b)
func resize(b []byte, neededLen int) []byte {
	b = b[:cap(b)]
	if n := neededLen - len(b); n > 0 {
		b = append(b, make([]byte, n)...)
	}
	return b
}

// cutPadding cuts the padding if the frame has FlagPadded
// from the payload and returns the new payload as byte slice.
func cutPadding(fr *Frame) []byte {
	payload := fr.payload
	if fr.Has(FlagPadded) {
		pad := uint32(payload[0])
		if uint32(len(payload)) < fr.length-pad-1 {
			panic(fmt.Sprintf("out of range: %d < %d", uint32(len(payload)), fr.length-pad-1)) // TODO: Change this panic...
		}
		payload = payload[1 : fr.length-pad]
	}
	return payload
}

// copied from https://github.com/valyala/fasthttp

// b2s converts byte slice to a string without memory allocation.
// See https://groups.google.com/forum/#!msg/Golang-Nuts/ENgbUzYvCuU/90yGx7GUAgAJ .
//
// Note it may break if string and/or slice header will change
// in the future go versions.
func b2s(b []byte) string {
	return *(*string)(unsafe.Pointer(&b))
}

// s2b converts string to a byte slice without memory allocation.
//
// Note it may break if string and/or slice header will change
// in the future go versions.
func s2b(s string) []byte {
	sh := (*reflect.StringHeader)(unsafe.Pointer(&s))
	bh := reflect.SliceHeader{
		Data: sh.Data,
		Len:  sh.Len,
		Cap:  sh.Len,
	}
	return *(*[]byte)(unsafe.Pointer(&bh))
}
