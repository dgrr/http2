package http2

import (
	"sync"

	"github.com/dgrr/http2/http2utils"
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
func (wu *WindowUpdate) ReadFrame(fr *FrameHeader) error {
	if len(fr.payload) < 4 {
		wu.increment = 0
		return ErrMissingBytes
	}

	wu.increment = http2utils.BytesToUint32(fr.payload) & (1<<31 - 1)

	return nil
}

// WriteFrame ...
func (wu *WindowUpdate) WriteFrame(fr *FrameHeader) {
	fr.kind = FrameWindowUpdate
	fr.payload = http2utils.AppendUint32Bytes(fr.payload[:0], wu.increment)
	fr.length = 4
}
