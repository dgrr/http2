package http2

import (
	"sync"
)

// PushPromise ...
type PushPromise struct {
	noCopy noCopy
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
func (pp *PushPromise) ReadFrame(fr *Frame) error {
	payload := cutPadding(fr)
	// TODO: Check len
	pp.code = bytesToUint32(payload) & (1<<31 - 1)
	pp.header = append(pp.header, payload[4:]...)
	pp.ended = fr.Has(FlagEndHeaders)
}

// WriteFrame ...
func (pp *PushPromise) WriteFrame(fr *Frame) error {
	fr._type = FramePushPromise
	fr.payload = fr.payload[:0]
	if data.pad {
		fr.Add(FlagPadded)
		// TODO: Write padding flag
	}
	fr.Write(pp.header)
	// TODO: write padding
}
