package http2

import (
	"sync"
)

const FramePing FrameType = 0x6

// Ping ...
//
// https://tools.ietf.org/html/rfc7540#section-6.7
type Ping struct {
	ack  bool
	data [8]byte
}

var pingPool = sync.Pool{
	New: func() interface{} {
		return &Ping{}
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
}

// CopyTo ...
func (ping *Ping) CopyTo(p *Ping) {
	p.ack = ping.ack
}

// Write ...
func (ping *Ping) Write(b []byte) (n int, err error) {
	copy(ping.data[:], b)
	return
}

// SetData ...
func (ping *Ping) SetData(b []byte) {
	copy(ping.data[:], b)
}

// ReadFrame ...
func (ping *Ping) ReadFrame(fr *Frame) error {
	ping.ack = fr.HasFlag(FlagAck)
	ping.SetData(fr.payload)
	return nil
}

func (ping *Ping) Data() []byte {
	return ping.data[:]
}

// WriteFrame ...
func (ping *Ping) WriteFrame(fr *Frame) error {
	fr.SetType(FramePing)
	if ping.ack {
		fr.AddFlag(FlagAck)
	}
	fr.SetPayload(ping.data[:])

	return nil
}
