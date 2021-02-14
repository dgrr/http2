package http2

import (
	"sync"
)

const FrameWindowUpdate FrameType = 0x8

// WindowUpdate ...
//
// https://tools.ietf.org/html/rfc7540#section-6.9
type WindowUpdate struct {
	increment uint32
}

var windowUpdatePool = sync.Pool{
	New: func() interface{} {
		return &WindowUpdate{}
	},
}

// AcquireWindowUpdate ...
func AcquireWindowUpdate() *WindowUpdate {
	wu := windowUpdatePool.Get().(*WindowUpdate)
	wu.Reset()
	return wu
}

// ReleaseWindowUpdate ...
func ReleaseWindowUpdate(wu *WindowUpdate) {
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
	wu.increment = increment
}

// ReadFrame ...
func (wu *WindowUpdate) ReadFrame(fr *Frame) error {
	if len(fr.payload) < 4 {
		wu.increment = 0
		return ErrMissingBytes
	}

	wu.increment = bytesToUint32(fr.payload) & (1<<31 - 1)

	return nil
}

// WriteFrame ...
func (wu *WindowUpdate) WriteFrame(fr *Frame) {
	fr.kind = FrameWindowUpdate
	fr.payload = appendUint32Bytes(fr.payload[:0], wu.increment)
	fr.length = 4
}
