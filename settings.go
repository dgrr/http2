package http2

import (
	"io"
	"sync"
)

var settingsPool = sync.Pool{
	New: func() interface{} {
		return &Settings{
			tableSize:  defaultHeaderTableSize,
			maxStreams: defaultConcurrentStreams,
			windowSize: defaultWindowSize,
			frameSize:  defaultframeSize,
		}
	},
}

const (
	// default Settings parameters
	defaultHeaderTableSize   uint32 = 4096
	defaultConcurrentStreams uint32 = 100
	defaultWindowSize        uint32 = 1<<15 - 1
	defaultframeSize         uint32 = 1 << 14

	windowSizeSize = 1<<31 - 1
	maxFrameSize   = 1<<24 - 1

	// FrameSettings string values (https://httpwg.org/specs/rfc7540.html#SettingValues)
	HeaderTableSize      uint16 = 0x1
	EnablePush           uint16 = 0x2
	MaxConcurrentStreams uint16 = 0x3
	MaxWindowSize        uint16 = 0x4
	MaxFrameSize         uint16 = 0x5
	MaxHeaderListSize    uint16 = 0x6
)

// Settings is the options to stablish between endpoints
// when starting the connection.
//
// This options have been humanize.
type Settings struct {
	noCopy      noCopy
	ack         bool
	rawSettings []byte
	tableSize   uint32
	enablePush  bool
	maxStreams  uint32
	windowSize  uint32
	frameSize   uint32
	headerSize  uint32
}

// AcquireSettings gets a Settings object from the pool with default values.
func AcquireSettings() *Settings {
	return settingsPool.Get().(*Settings)
}

// ReleaseSettings puts st into settings pool to be reused in the future.
func ReleaseSettings(st *Settings) {
	st.Reset()
	settingsPool.Put(st)
}

// Reset resets settings to default values
func (st *Settings) Reset() {
	// default settings
	st.tableSize = defaultHeaderTableSize
	st.maxStreams = defaultConcurrentStreams
	st.windowSize = defaultWindowSize
	st.frameSize = defaultframeSize
	st.enablePush = false
	st.headerSize = 0
	st.rawSettings = st.rawSettings[:0]
	st.ack = false
}

// SetHeaderTableSize sets the maximum size of the header
// compression table used to decode header blocks.
//
// Default value is 4096
func (st *Settings) SetHeaderTableSize(size uint32) {
	st.tableSize = size
}

// HeaderTableSize returns the maximum size of the header
// compression table used to decode header blocks.
//
// Default value is 4096
func (st *Settings) HeaderTableSize() uint32 {
	return st.tableSize
}

// Push allows to set the PushPromise settings.
//
// If value is true the Push Promise will be enable.
// if not the Push Promise will be disabled.
func (st *Settings) Push(value bool) {
	st.enablePush = value
}

// SetMaxConcurrentStreams sets the maximum number of
// concurrent Streams that the sender will allow.
//
// Default value is 100. This value does not have max limit.
func (st *Settings) SetMaxConcurrentStreams(streams uint32) {
	st.maxStreams = streams
}

// MaxConcurrentStreams returns the maximum number of
// concurrent Streams that the sender will allow.
//
// Default value is 100. This value does not have max limit.
func (st *Settings) MaxConcurrentStreams() uint32 {
	return st.maxStreams
}

// SetMaxWindowSize sets the sender's initial window size
// for Stream-level flow control.
//
// Default value is 1 << 16 - 1
// Maximum value is 1 << 31 - 1
func (st *Settings) SetMaxWindowSize(size uint32) {
	st.windowSize = size
}

// MaxWindowSize returns the sender's initial window size
// for Stream-level flow control.
//
// Default value is 1 << 16 - 1
// Maximum value is 1 << 31 - 1
func (st *Settings) MaxWindowSize() uint32 {
	return st.windowSize
}

// SetMaxFrameSize sets the size of the largest frame
// Payload that the sender is willing to receive.
//
// Default value is 1 << 14
// Maximum value is 1 << 24 - 1
func (st *Settings) SetMaxFrameSize(size uint32) {
	st.frameSize = size
}

// MaxFrameSize returns the size of the largest frame
// Payload that the sender is willing to receive.
//
// Default value is 1 << 14
// Maximum value is 1 << 24 - 1
func (st *Settings) MaxFrameSize() uint32 {
	return st.frameSize
}

// SetMaxFrameSize sets maximum size of header list.
//
// If this value is 0 indicates that there are no limit.
func (st *Settings) SetMaxHeaderListSize(size uint32) {
	st.headerSize = size
}

// MaxFrameSize returns maximum size of header list.
//
// If this value is 0 indicates that there are no limit.
func (st *Settings) MaxHeaderListSize() uint32 {
	return st.headerSize
}

// Read reads from d and decodes the readed values into st.
func (st *Settings) Read(d []byte) { // TODO: return error?
	var b []byte
	var key uint16
	var value uint32
	last, i, len := 0, 6, len(d)
	for i <= len {
		b = d[last:i]
		key = uint16(b[0])<<8 | uint16(b[1])
		value = uint32(b[2])<<24 | uint32(b[3])<<16 | uint32(b[4])<<8 | uint32(b[5])

		switch key {
		case HeaderTableSize:
			st.tableSize = value
		case EnablePush:
			st.enablePush = (value != 0)
		case MaxConcurrentStreams:
			st.maxStreams = value
		case MaxWindowSize:
			st.windowSize = value
		case MaxFrameSize:
			st.frameSize = value
		case MaxHeaderListSize:
			st.headerSize = value
		}
		last = i
		i += 6
	}
}

// Encode encodes settings to be sended through the wire
func (st *Settings) Encode() {
	st.rawSettings = st.rawSettings[:0]
	if st.tableSize != 0 {
		st.rawSettings = append(st.rawSettings,
			byte(HeaderTableSize>>8), byte(HeaderTableSize),
			byte(st.tableSize>>24), byte(st.tableSize>>16),
			byte(st.tableSize>>8), byte(st.tableSize),
		)
	}
	if st.enablePush {
		st.rawSettings = append(st.rawSettings,
			byte(EnablePush>>8), byte(EnablePush),
			0, 0, 0, 1,
		)
	}
	if st.maxStreams != 0 {
		st.rawSettings = append(st.rawSettings,
			byte(MaxConcurrentStreams>>8), byte(MaxConcurrentStreams),
			byte(st.maxStreams>>24), byte(st.maxStreams>>16),
			byte(st.maxStreams>>8), byte(st.maxStreams),
		)
	}
	if st.windowSize != 0 {
		st.rawSettings = append(st.rawSettings,
			byte(MaxWindowSize>>8), byte(MaxWindowSize),
			byte(st.windowSize>>24), byte(st.windowSize>>16),
			byte(st.windowSize>>8), byte(st.windowSize),
		)
	}
	if st.frameSize != 0 {
		st.rawSettings = append(st.rawSettings,
			byte(MaxFrameSize>>8), byte(MaxFrameSize),
			byte(st.frameSize>>24), byte(st.frameSize>>16),
			byte(st.frameSize>>8), byte(st.frameSize),
		)
	}
	if st.headerSize != 0 {
		st.rawSettings = append(st.rawSettings,
			byte(MaxHeaderListSize>>8), byte(MaxHeaderListSize),
			byte(st.headerSize>>24), byte(st.headerSize>>16),
			byte(st.headerSize>>8), byte(st.headerSize),
		)
	}
}

// ReadFrame reads and decodes frame payload into st values
func (st *Settings) ReadFrame(fr *Frame) error {
	if fr._type != FrameSettings { // TODO: Probably repeated checking
		return ErrFrameMismatch
	}
	st.ack = fr.Has(FlagAck)
	st.Read(fr.Payload())
	return nil
}

// IsAck returns true if settings has FlagAck set.
func (st *Settings) IsAck() bool {
	return st.ack
}

// SetAck sets FlagAck when WriteTo is called.
func (st *Settings) SetAck(ack bool) {
	st.ack = ack
}

// WriteTo writes settings frame to bw
func (st *Settings) WriteTo(bw io.Writer) (int64, error) {
	fr := AcquireFrame()
	defer ReleaseFrame(fr)

	st.Encode()

	fr._type = FrameSettings
	if st.ack {
		fr.Add(FlagAck)
	}
	fr.SetPayload(st.rawSettings)
	return fr.WriteTo(bw)
}
