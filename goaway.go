package http2

import (
	"github.com/dgrr/http2/http2utils"
)

const FrameGoAway FrameType = 0x7

var _ Frame = &GoAway{}

// GoAway ...
//
// https://tools.ietf.org/html/rfc7540#section-6.8
type GoAway struct {
	stream uint32
	code   uint32
	data   []byte // additional data
}

func (ga *GoAway) Type() FrameType {
	return FrameGoAway
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
func (ga *GoAway) Deserialize(fr *FrameHeader) (err error) {
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

func (ga *GoAway) Serialize(fr *FrameHeader) {
	fr.payload = http2utils.AppendUint32Bytes(fr.payload[:0], ga.stream)
	fr.payload = http2utils.AppendUint32Bytes(fr.payload[:4], ga.code)

	fr.payload = append(fr.payload, ga.data...)
}
