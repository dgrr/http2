package http2

import (
	"sync"

	"github.com/dgrr/http2/http2utils"
)

const FramePushPromise FrameType = 0x5

// PushPromise ...
//
// https://tools.ietf.org/html/rfc7540#section-6.6
type PushPromise struct {
	pad    bool
	ended  bool
	stream uint32
	header []byte // header block fragment
}

var pushPromisePool = sync.Pool{
	New: func() interface{} {
		return &PushPromise{}
	},
}

// AcquirePushPromise ...
func AcquirePushPromise() *PushPromise {
	return pushPromisePool.Get().(*PushPromise)
}

// ReleasePushPromise ...
func ReleasePushPromise(pp *PushPromise) {
	pp.Reset()
	pushPromisePool.Put(pp)
}

// Reset ...
func (pp *PushPromise) Reset() {
	pp.stream = 0
}

func (pp *PushPromise) SetHeader(h []byte) {
	pp.header = append(pp.header[:0], h...)
}

func (pp *PushPromise) Write(b []byte) (int, error) {
	n := len(b)
	pp.header = append(pp.header, b...)
	return n, nil
}

// ReadFrame ...
func (pp *PushPromise) ReadFrame(fr *Frame) (err error) {
	payload := fr.Payload()
	if fr.HasFlag(FlagPadded) {
		payload = http2utils.CutPadding(payload, fr.Len())
	}

	if len(fr.payload) < 4 {
		err = ErrMissingBytes
	} else {
		pp.stream = http2utils.BytesToUint32(payload) & (1<<31 - 1)
		pp.header = append(pp.header, payload[4:]...)
		pp.ended = fr.HasFlag(FlagEndHeaders)
	}
	return
}

// WriteFrame ...
func (pp *PushPromise) WriteFrame(fr *Frame) (err error) {
	fr.SetType(FramePushPromise)
	fr.payload = fr.payload[:0]
	if pp.pad {
		fr.AddFlag(FlagPadded)
		// TODO: Write padding flag
	}
	_, err = fr.Write(pp.header)
	// TODO: write padding
	return
}
