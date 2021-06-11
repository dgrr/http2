package http2

import (
	"sync"

	"github.com/dgrr/http2/http2utils"
)

const FrameGoAway FrameType = 0x7

// GoAway ...
//
// https://tools.ietf.org/html/rfc7540#section-6.8
type GoAway struct {
	stream uint32
	code   uint32
	data   []byte // additional data
}

var goawayPool = sync.Pool{
	New: func() interface{} {
		return &GoAway{}
	},
}

// AcquireGoAway ...
func AcquireGoAway() *GoAway {
	return goawayPool.Get().(*GoAway)
}

// ReleaseGoAway ...
func ReleaseGoAway(ga *GoAway) {
	ga.Reset()
	goawayPool.Put(ga)
}

// Reset ...
func (ga *GoAway) Reset() {
	ga.stream = 0
	ga.code = 0
	ga.data = ga.data[:0]
}

// CopyTo ...
func (ga *GoAway) CopyTo(ga2 *GoAway) {
	ga2.stream = ga.stream
	ga2.code = ga.code
	ga2.data = append(ga2.data[:0], ga.data...)
}

// Code ...
func (ga *GoAway) Code() ErrorCode {
	return ErrorCode(ga.code)
}

// SetCode ...
func (ga *GoAway) SetCode(code ErrorCode) {
	ga.code = uint32(code & (1<<31 - 1))
	// TODO: Set error description as a debug data?
}

// Stream ...
func (ga *GoAway) Stream() uint32 {
	return ga.stream
}

// SetStream ...
func (ga *GoAway) SetStream(stream uint32) {
	ga.stream = stream & (1<<31 - 1)
}

// Data ...
func (ga *GoAway) Data() []byte {
	return ga.data
}

// SetData ...
func (ga *GoAway) SetData(b []byte) {
	ga.data = append(ga.data[:0], b...)
}

// ReadFrame ...
func (ga *GoAway) ReadFrame(fr *Frame) (err error) {
	if len(fr.payload) < 8 { // 8 is the min number of bytes
		err = ErrMissingBytes
	} else {
		ga.code = http2utils.BytesToUint32(fr.payload)
		ga.code = http2utils.BytesToUint32(fr.payload[4:])
		if len(fr.payload[8:]) > 0 {
			ga.data = append(ga.data[:0], fr.payload[8:]...)
		}
	}
	return
}

// WriteFrame ...
func (ga *GoAway) WriteFrame(fr *Frame) (err error) {
	fr.payload = http2utils.AppendUint32Bytes(fr.payload[:0], ga.stream)
	fr.payload = http2utils.AppendUint32Bytes(fr.payload[:4], ga.code)
	_, err = fr.AppendPayload(ga.data)
	return
}
