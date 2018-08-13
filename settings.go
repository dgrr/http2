package fasthttp2

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
)

var settingsPool = sync.Pool{
	New: func() interface{} {
		return &Settings{
			HeaderTableSize:      defaultHeaderTableSize,
			MaxConcurrentStreams: defaultConcurrentStreams,
			InitialWindowSize:    defaultWindowSize,
			MaxFrameSize:         defaultMaxFrameSize,
		}
	},
}

const (
	// default Settings parameters
	defaultHeaderTableSize   uint32 = 4096
	defaultConcurrentStreams uint32 = 100
	defaultWindowSize        uint32 = 1<<15 - 1
	defaultMaxFrameSize      uint32 = 1 << 14

	maxWindowSize = 1<<31 - 1
	maxFrameSize  = 1<<24 - 1

	// FrameSettings string values (https://httpwg.org/specs/rfc7540.html#SettingValues)
	HeaderTableSize      uint16 = 0x1
	EnablePush           uint16 = 0x2
	MaxConcurrentStreams uint16 = 0x3
	InitialWindowSize    uint16 = 0x4
	MaxFrameSize         uint16 = 0x5
	MaxHeaderListSize    uint16 = 0x6
)

// Settings is the options to stablish between endpoints
// when starting the connection.
//
// This options have been humanize.
type Settings struct {
	// TODO: Add noCopy

	rawSettings []byte

	// Allows the sender to inform the remote endpoint
	// of the maximum size of the header compression table
	// used to decode header blocks.
	//
	// Default value is 4096
	HeaderTableSize uint32

	// DisablePush allows client to not send push request.
	//
	// (http://httpwg.org/specs/rfc7540.html#PushResources)
	//
	// Default value is false
	DisablePush bool

	// MaxConcurrentStreams indicates the maximum number of
	// concurrent Streams that the sender will allow.
	//
	// Default value is 100. This value does not have max limit.
	MaxConcurrentStreams uint32

	// InitialWindowSize indicates the sender's initial window size
	// for Stream-level flow control.
	//
	// Default value is 1 << 16 - 1
	// Maximum value is 1 << 31 - 1
	InitialWindowSize uint32

	// MaxFrameSize indicates the size of the largest frame
	// Payload that the sender is willing to receive.
	//
	// Default value is 1 << 14
	// Maximum value is 1 << 24 - 1
	MaxFrameSize uint32

	// This advisory setting informs a peer of the maximum size of
	// header list that the sender is prepared to accept.
	//
	// If this value is 0 indicates that there are no limit.
	MaxHeaderListSize uint32
}

// AcquireSettings returns a Settings object from the pool with default values.
func AcquireSettings() *Settings {
	return settingsPool.Get().(*Settings)
}

// ReleaseSettings puts s into settings pool.
func ReleaseSettings(st *Settings) {
	st.Reset()
	settingsPool.Put(st)
}

// Reset resets settings to default values
func (st *Settings) Reset() {
	// default settings
	st.HeaderTableSize = defaultHeaderTableSize
	st.MaxConcurrentStreams = defaultConcurrentStreams
	st.InitialWindowSize = defaultWindowSize
	st.MaxFrameSize = defaultMaxFrameSize
	st.rawSettings = st.rawSettings[:0]
	st.DisablePush = false
	st.MaxHeaderListSize = 0
}

// Decode decodes frame payload into st
func (st *Settings) Decode(d []byte) {
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
			st.HeaderTableSize = value
		case EnablePush:
			st.DisablePush = (value == 0)
		case MaxConcurrentStreams:
			st.MaxConcurrentStreams = value
		case InitialWindowSize:
			st.InitialWindowSize = value
		case MaxFrameSize:
			st.MaxFrameSize = value
		case MaxHeaderListSize:
			st.MaxHeaderListSize = value
		}
		last = i
		i += 6
	}
}

// Encode encodes setting to be sended through the wire
func (st *Settings) Encode() {
	st.rawSettings = st.rawSettings[:0]
	if st.HeaderTableSize != 0 {
		st.rawSettings = append(st.rawSettings,
			byte(HeaderTableSize>>8), byte(HeaderTableSize),
			byte(st.HeaderTableSize>>24), byte(st.HeaderTableSize>>16),
			byte(st.HeaderTableSize>>8), byte(st.HeaderTableSize),
		)
	}
	if !st.DisablePush {
		st.rawSettings = append(st.rawSettings,
			byte(EnablePush>>8), byte(EnablePush),
			0, 0, 0, 1,
		)
	}
	if st.MaxConcurrentStreams != 0 {
		st.rawSettings = append(st.rawSettings,
			byte(MaxConcurrentStreams>>8), byte(MaxConcurrentStreams),
			byte(st.MaxConcurrentStreams>>24), byte(st.MaxConcurrentStreams>>16),
			byte(st.MaxConcurrentStreams>>8), byte(st.MaxConcurrentStreams),
		)
	}
	if st.InitialWindowSize != 0 {
		st.rawSettings = append(st.rawSettings,
			byte(InitialWindowSize>>8), byte(InitialWindowSize),
			byte(st.InitialWindowSize>>24), byte(st.InitialWindowSize>>16),
			byte(st.InitialWindowSize>>8), byte(st.InitialWindowSize),
		)
	}
	if st.MaxFrameSize != 0 {
		st.rawSettings = append(st.rawSettings,
			byte(MaxFrameSize>>8), byte(MaxFrameSize),
			byte(st.MaxFrameSize>>24), byte(st.MaxFrameSize>>16),
			byte(st.MaxFrameSize>>8), byte(st.MaxFrameSize),
		)
	}
	if st.MaxHeaderListSize != 0 {
		st.rawSettings = append(st.rawSettings,
			byte(MaxHeaderListSize>>8), byte(MaxHeaderListSize),
			byte(st.MaxHeaderListSize>>24), byte(st.MaxHeaderListSize>>16),
			byte(st.MaxHeaderListSize>>8), byte(st.MaxHeaderListSize),
		)
	}
}

// DecodeFrame decodes frame payload into st
func (st *Settings) DecodeFrame(fr *Frame) error {
	if !fr.Is(FrameSettings) { // TODO: Probably repeated checking
		return errFrameMismatch
	}
	st.Decode(fr.Payload())
	return nil
}
