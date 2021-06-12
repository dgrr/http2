package http2

import (
	"github.com/dgrr/http2/http2utils"
)

const FrameResetStream FrameType = 0x3

var _ Frame = &RstStream{}

// RstStream ...
//
// https://tools.ietf.org/html/rfc7540#section-6.4
type RstStream struct {
	code ErrorCode
}

func (rst *RstStream) Type() FrameType {
	return FrameResetStream
}

// Code ...
func (rst *RstStream) Code() ErrorCode {
	return rst.code
}

// SetCode ...
func (rst *RstStream) SetCode(code ErrorCode) {
	rst.code = code
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
	return NewError(rst.code, "")
}

// ReadFrame ...
func (rst *RstStream) Deserialize(fr *FrameHeader) error {
	if len(fr.payload) < 4 {
		return ErrMissingBytes
	}

	rst.code = ErrorCode(http2utils.BytesToUint32(fr.payload))

	return nil
}

// WriteFrame ...
func (rst *RstStream) Serialize(fr *FrameHeader) {
	fr.payload = http2utils.AppendUint32Bytes(fr.payload[:0], uint32(rst.code))
	fr.length = 4
}
