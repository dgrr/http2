package http2

import (
	"sync"
)

const FrameResetStream FrameType = 0x3

// RstStream ...
//
// https://tools.ietf.org/html/rfc7540#section-6.4
type RstStream struct {
	code uint32
}

var rstStreamPool = sync.Pool{
	New: func() interface{} {
		return &RstStream{}
	},
}

// AcquireRstStream ...
func AcquireRstStream() *RstStream {
	return rstStreamPool.Get().(*RstStream)
}

// ReleaseRstStream ...
func ReleaseRstStream(rst *RstStream) {
	rst.Reset()
	rstStreamPool.Put(rst)
}

// Reset ...
func (rst *RstStream) Reset() {
	rst.code = 0
}

// CopyTo ...
func (rst *RstStream) CopyTo(r *RstStream) {
	r.code = rst.code
}

// Error ...
func (rst *RstStream) Error() error {
	return Error(rst.code)
}

// ReadFrame ...
func (rst *RstStream) ReadFrame(fr *Frame) error {
	if len(fr.payload) < 4 {
		return ErrMissingBytes
	}

	rst.code = bytesToUint32(fr.payload)
	return nil
}

// WriteFrame ...
func (rst *RstStream) WriteFrame(fr *Frame) {
	fr.kind = FrameResetStream
	fr.payload = appendUint32Bytes(fr.payload[:0], rst.code)
}
