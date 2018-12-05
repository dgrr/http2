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

// cutPadding cuts the padding if the frame has FlagPadded
// from the payload and returns the new payload as byte slice.
func cutPadding(fr *Frame) []byte {
	payload := fr.payload
	if fr.Has(FlagPadded) {
		pad := uint32(payload[0])
		if uint32(len(payload)) < fr.length-pad-1 {
			panic(fmt.Sprintf("out of range: %s < %s", uint32(len(payload)), fr.length-pad-1)) // TODO: Change this panic...
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
