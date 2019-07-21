package http2

import (
	"sync"
)

const FrameContinuation uint8 = 0x9

// Continuation ...
//
// https://tools.ietf.org/html/rfc7540#section-6.10
type Continuation struct {
	noCopy noCopy
	ended  bool
	header []byte
}

var continuationPool = sync.Pool{
	New: func() interface{} {
		return &Continuation{}
	},
}

// AcquireContinuation ...
func AcquireContinuation() *Continuation {
	return continuationPool.Get().(*Continuation)
}

// ReleaseContinuation ...
func ReleaseContinuation(c *Continuation) {
	c.Reset()
	continuationPool.Put(c)
}

// Reset ...
func (c *Continuation) Reset() {
	c.ended = false
	c.header = c.header[:0]
}

func (c *Continuation) CopyTo(cc *Continuation) {
	cc.ended = c.ended
	cc.header = append(cc.header[:0], c.header...)
}

// Header returns Header bytes.
func (c *Continuation) Header() []byte {
	return c.header
}

// SetEndStream ...
func (c *Continuation) SetEndStream(value bool) {
	c.ended = value
}

// SetHeader ...
func (c *Continuation) SetHeader(b []byte) {
	c.header = append(c.header[:0], b...)
}

// AppendHeader ...
func (c *Continuation) AppendHeader(b []byte) {
	c.header = append(c.header, b...)
}

// Write ...
func (c *Continuation) Write(b []byte) (int, error) {
	n := len(b)
	c.AppendHeader(b)
	return n, nil
}

// ReadFrame reads decodes fr payload into c.
func (c *Continuation) ReadFrame(fr *Frame) (err error) {
	c.ended = fr.Has(FlagEndHeaders)
	c.SetHeader(fr.payload)
	return
}

// WriteFrame ...
func (c *Continuation) WriteFrame(fr *Frame) error {
	if c.ended {
		fr.Add(FlagEndHeaders)
	}
	fr.kind = FrameContinuation
	return fr.SetPayload(c.header)
}

// TODO: WriteTo ?
