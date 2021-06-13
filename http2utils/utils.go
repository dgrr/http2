package http2utils

import (
	"crypto/rand"
	"fmt"
	"reflect"
	"unsafe"

	"github.com/valyala/fastrand"
)

func Uint24ToBytes(b []byte, n uint32) {
	_ = b[2] // bound checking
	b[0] = byte(n >> 16)
	b[1] = byte(n >> 8)
	b[2] = byte(n)
}

func BytesToUint24(b []byte) uint32 {
	_ = b[2] // bound checking
	return uint32(b[0])<<16 |
		uint32(b[1])<<8 |
		uint32(b[2])
}

func AppendUint32Bytes(dst []byte, n uint32) []byte {
	dst = append(dst, byte(n>>24))
	dst = append(dst, byte(n>>16))
	dst = append(dst, byte(n>>8))
	dst = append(dst, byte(n))
	return dst
}

func Uint32ToBytes(b []byte, n uint32) {
	_ = b[3] // bound checking
	b[0] = byte(n >> 24)
	b[1] = byte(n >> 16)
	b[2] = byte(n >> 8)
	b[3] = byte(n)
}

func BytesToUint32(b []byte) uint32 {
	_ = b[3] // bound checking
	n := uint32(b[0])<<24 |
		uint32(b[1])<<16 |
		uint32(b[2])<<8 |
		uint32(b[3])
	return n
}

func EqualsFold(a, b []byte) bool {
	n := len(a)
	if n != len(b) {
		return false
	}
	for i := 0; i < n; i++ {
		if a[i]|0x20 != b[i]|0x20 {
			return false
		}
	}
	return true
}

func Resize(b []byte, neededLen int) []byte {
	b = b[:cap(b)]

	if n := neededLen - len(b); n > 0 {
		b = append(b, make([]byte, n)...)
	}

	return b[:neededLen]
}

// CutPadding cuts the padding if the frame has FlagPadded
// from the payload and returns the new payload as byte slice.
func CutPadding(payload []byte, length int) []byte {
	pad := int(payload[0])

	if len(payload) < length-pad-1 {
		panic(fmt.Sprintf("out of range: %d < %d", uint32(len(payload)), length-pad-1)) // TODO: Change this panic...
	}

	payload = payload[1 : length-pad]

	return payload
}

func AddPadding(b []byte) []byte {
	n := int(fastrand.Uint32n(256-9)) + 9
	nn := len(b)

	b = Resize(b, nn+n)
	b = append(b[:1], b...)

	b[0] = uint8(n)

	rand.Read(b[nn+1 : nn+n])

	return b
}

func FastBytesToString(b []byte) string {
	return *(*string)(unsafe.Pointer(&b))
}

func FastStringToBytes(s string) []byte {
	sh := (*reflect.StringHeader)(unsafe.Pointer(&s))
	bh := reflect.SliceHeader{
		Data: sh.Data,
		Len:  sh.Len,
		Cap:  sh.Len,
	}

	return *(*[]byte)(unsafe.Pointer(&bh))
}
