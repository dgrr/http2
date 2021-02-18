package http2

import (
	"sync"
)

const FrameContinuation FrameType = 0x9

// Continuation ...
//
// https://tools.ietf.org/html/rfc7540#section-6.10
type Continuation struct {
	endHeaders bool
	rawHeaders []byte
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
	c.endHeaders = false
	c.rawHeaders = c.rawHeaders[:0]
}

func (c *Continuation) CopyTo(cc *Continuation) {
	cc.endHeaders = c.endHeaders
	cc.rawHeaders = append(cc.rawHeaders[:0], c.rawHeaders...)
}

// Header returns Header bytes.
func (c *Continuation) Header() []byte {
	return c.rawHeaders
}

// SetEndHeaders ...
func (c *Continuation) SetEndHeaders(value bool) {
	c.endHeaders = value
}

// HasEndHeaders ...
func (c *Continuation) EndHeaders() bool {
	return c.endHeaders
}

// SetHeader ...
func (c *Continuation) SetHeader(b []byte) {
	c.rawHeaders = append(c.rawHeaders[:0], b...)
}

// AppendHeader ...
func (c *Continuation) AppendHeader(b []byte) {
	c.rawHeaders = append(c.rawHeaders, b...)
}

// Write ...
func (c *Continuation) Write(b []byte) (int, error) {
	n := len(b)
	c.AppendHeader(b)
	return n, nil
}

// ReadFrame reads decodes fr payload into c.
func (c *Continuation) ReadFrame(fr *Frame) (err error) {
	c.endHeaders = fr.HasFlag(FlagEndHeaders)
	c.SetHeader(fr.payload)
	return
}

// WriteFrame ...
func (c *Continuation) WriteFrame(fr *Frame) error {
	fr.SetType(FrameContinuation)
	if c.endHeaders {
		fr.AddFlag(FlagEndHeaders)
	}
	return fr.SetPayload(c.rawHeaders)
}
