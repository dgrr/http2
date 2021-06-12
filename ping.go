package http2

const FramePing FrameType = 0x6

var _ Frame = &Ping{}

// Ping ...
//
// https://tools.ietf.org/html/rfc7540#section-6.7
type Ping struct {
	ack  bool
	data [8]byte
}

func (ping *Ping) Type() FrameType {
	return FramePing
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

// Deserialize ...
func (ping *Ping) Deserialize(frh *FrameHeader) error {
	ping.ack = frh.Flags().Has(FlagAck)
	ping.SetData(frh.payload)
	return nil
}

func (ping *Ping) Data() []byte {
	return ping.data[:]
}

// Serialize ...
func (ping *Ping) Serialize(fr *FrameHeader) {
	if ping.ack {
		fr.SetFlags(fr.Flags().Add(FlagAck))
	}

	fr.setPayload(ping.data[:])
}
