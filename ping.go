package http2

import (
	"sync"
)

type Ping struct {
	ack bool
	b   []byte // data
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

// Write ...
func (ping *Ping) Write(b []byte) error {
	ping.b = append(ping.b, b...)
}

// SetData ...
func (ping *Ping) SetData(b []byte) error {
	ping.b = append(ping.b[:0], b...)
}

// ReadFrame ...
func (ping *Ping) ReadFrame(fr *Frame) error {
	ping.ack = fr.Has(FlagAck)
	ping.SetData(fr.payload)
	return nil
}

// WriteFrame ...
func (ping *Ping) WriteFrame(fr *Frame) error {
	return nil
}
