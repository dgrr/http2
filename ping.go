package http2

import (
	"sync"
)

const FramePing FrameType = 0x6

// Ping ...
//
// https://tools.ietf.org/html/rfc7540#section-6.7
type Ping struct {
	ack bool
	b   []byte // data
}

var pingPool = sync.Pool{
	New: func() interface{} {
		return &Ping{
			b: make([]byte, 8),
		}
	},
}

// AcquirePing ...
func AcquirePing() *Ping {
	return pingPool.Get().(*Ping)
}

// ReleasePing ...
func ReleasePing(pp *Ping) {
	pp.Reset()
	pingPool.Put(pp)
}

// Reset ...
func (ping *Ping) Reset() {
	ping.ack = false
	ping.b = ping.b[:0]
}

// CopyTo ...
func (ping *Ping) CopyTo(p *Ping) {
	p.ack = ping.ack
	p.b = append(p.b[:0], ping.b...)
}

// Write ...
func (ping *Ping) Write(b []byte) (n int, err error) {
	n = len(b)
	if n+len(ping.b) > 8 {
		err = ErrTooManyBytes
	} else {
		ping.b = append(ping.b, b...)
	}
	return
}

// SetData ...
func (ping *Ping) SetData(b []byte) {
	n := len(b)
	if n > 8 {
		n = 8
	}
	ping.b = append(ping.b[:0], b[:n]...)
}

// ReadFrame ...
func (ping *Ping) ReadFrame(fr *Frame) error {
	ping.ack = fr.Has(FlagAck)
	ping.SetData(fr.payload)
	return nil
}

// WriteFrame ...
func (ping *Ping) WriteFrame(fr *Frame) error {
	if ping.ack {
		fr.Add(FlagAck)
	}
	return nil
}
