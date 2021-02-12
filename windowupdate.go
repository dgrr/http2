package http2

import (
	"sync"
)

const FrameWindowUpdate FrameType = 0x8

// WindowUpdate ...
//
// https://tools.ietf.org/html/rfc7540#section-6.9
type WindowUpdate struct {
	noCopy    noCopy
	increment uint32
}

var windowUpdatePool = sync.Pool{
	New: func() interface{} {
		return &WindowUpdate{}
	},
}

// AcquireWindowUpdate ...
func AcquireWindowUpdate() *WindowUpdate {
	return windowUpdatePool.Get().(*WindowUpdate)
}

// ReleaseWindowUpdate ...
func ReleaseWindowUpdate(wu *WindowUpdate) {
	wu.Reset()
	windowUpdatePool.Put(wu)
}

// Reset ...
func (wu *WindowUpdate) Reset() {
	wu.increment = 0
}

// CopyTo ...
func (wu *WindowUpdate) CopyTo(w *WindowUpdate) {
	w.increment = wu.increment
}

// Increment ...
func (wu *WindowUpdate) Increment() uint32 {
	return wu.increment
}

// SetIncrement ...
func (wu *WindowUpdate) SetIncrement(increment uint32) {
	wu.increment = increment & (1<<31 - 1)
}

// ReadFrame ...
func (wu *WindowUpdate) ReadFrame(fr *Frame) (err error) {
	if len(fr.payload) < 4 {
		err = ErrMissingBytes
	} else {
		wu.increment = bytesToUint32(fr.payload) & (1<<31 - 1)
	}
	return
}

// WriteFrame ...
func (wu *WindowUpdate) WriteFrame(fr *Frame) {
	fr.kind = FrameWindowUpdate
	fr.payload = appendUint32Bytes(fr.payload[:0], wu.increment)
}
