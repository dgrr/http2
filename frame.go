package fasthttp2

import (
	"io"
	"sync"
)

const (
	// Frame default size
	// http://httpwg.org/specs/rfc7540.html#FrameHeader
	defaultFrameSize = 9

	// FrameType (http://httpwg.org/specs/rfc7540.html#FrameTypes)
	FrameData         uint8 = 0x0
	FrameHeader       uint8 = 0x1
	FramePriority     uint8 = 0x2
	FrameResetStream  uint8 = 0x3
	FrameSettings     uint8 = 0x4
	FramePushPromise  uint8 = 0x5
	FramePing         uint8 = 0x6
	FrameGoAway       uint8 = 0x7
	FrameWindowUpdate uint8 = 0x8
	FrameContinuation uint8 = 0x9

	minFrameType uint8 = 0x0
	maxFrameType uint8 = 0x9

	// Frame Flag (described along the frame types)
	// More flags have been ignored due to redundancy
	FlagAck        uint8 = 0x1
	FlagEndStream  uint8 = 0x1
	FlagEndHeaders uint8 = 0x4
	FlagPadded     uint8 = 0x8
	FlagPriority   uint8 = 0x20
)

var bytePool = sync.Pool{
	New: func() interface{} {
		return make([]byte, 512)
	},
}

func acquireByte() []byte {
	return bytePool.Get().([]byte)
}

func releaseByte(b []byte) {
	bytePool.Put(b)
}

var framePool = sync.Pool{
	New: func() interface{} {
		return &Frame{
			header: make([]byte, defaultFrameSize),
		}
	},
}

// TODO: Implement https://tools.ietf.org/html/draft-ietf-httpbis-alt-svc-06#section-4

// HTTP2 via http is not the same as following https schema.
// HTTPS identifies HTTP2 connection using TLS-ALPN
// HTTP  identifies HTTP2 connection using Upgrade header value.
//
// TLSConfig.NextProtos must be []string{"h2"}

// Frame is frame representation of HTTP2 protocol
//
// Use AcquireFrame instead of creating Frame every time
// if you are going to use Frame as your own and ReleaseFrame to
// delete the Frame
//
// Frame instance MUST NOT be used from concurrently running goroutines.
type Frame struct {
	header  []byte // TODO: Get payload inside header?
	payload []byte

	decoded bool
	// TODO: if length is granther than 16384 the body must not be
	// readed unless settings specify it

	// Length is the payload length
	Length uint32 // 24 bits

	// Type is the frame type (https://httpwg.org/specs/rfc7540.html#FrameTypes)
	Type uint8 // 8 bits

	// Flags is the flags the frame contains
	Flags uint8 // 8 bits

	// Stream is the id of the stream
	Stream uint32 // 31 bits
}

// AcquireFrame returns a Frame from pool.
func AcquireFrame() *Frame {
	return framePool.Get().(*Frame)
}

// ReleaseFrame returns fr Frame to the pool.
func ReleaseFrame(fr *Frame) {
	fr.Reset()
	framePool.Put(fr)
}

// Reset resets the frame
func (fr *Frame) Reset() {
	fr.Length = 0
	fr.Type = 0
	fr.Flags = 0
	fr.Stream = 0
	fr.Length = 0
	releaseByte(fr.payload[:cap(fr.payload)])
}

// Is returns boolean value indicating if frame Type is t
func (fr *Frame) Is(t uint8) bool {
	return (fr.Type == t)
}

// Has returns boolean value indicating if frame Flags has f
func (fr *Frame) Has(f uint8) bool {
	return (fr.Flags & f) == f
}

// Set sets type to fr
func (fr *Frame) Set(t uint8) {
	fr.Type = t
}

// Add adds a flag to fr
func (fr *Frame) Add(f uint8) {
	fr.Flags |= f
}

// Header returns header bytes of fr
func (fr *Frame) Header() []byte {
	return fr.header
}

// Payload returns processed payload deleting padding and additional headers.
func (fr *Frame) Payload() []byte {
	if fr.decoded {
		return fr.payload
	}
	// TODO: Decode payload deleting padding...
	fr.decoded = true
	return nil
}

// SetPayload sets new payload to fr
func (fr *Frame) SetPayload(b []byte) {
	fr.payload = append(fr.payload[:0], b...)
	fr.Length = uint32(len(fr.payload))
}

// Write writes b to frame payload.
//
// This function is compatible with io.Writer
func (fr *Frame) Write(b []byte) (int, error) {
	n := fr.AppendPayload(b)
	return n, nil
}

// AppendPayload appends bytes to frame payload
func (fr *Frame) AppendPayload(b []byte) int {
	n := len(b)
	fr.payload = append(fr.payload, b...)
	fr.Length += uint32(n)
	return n
}

// ReadFrom reads frame header and payload from br
//
// Frame.ReadFrom it's compatible with io.ReaderFrom
// but this implementation does not read until io.EOF
func (fr *Frame) ReadFrom(br io.Reader) (int64, error) {
	n, err := br.Read(fr.header[:defaultFrameSize])
	if err != nil {
		return 0, err
	}
	if n != defaultFrameSize {
		return 0, errParser[FrameSizeError]
	}
	fr.rawToLength()               // 3
	fr.Type = uint8(fr.header[3])  // 1
	fr.Flags = uint8(fr.header[4]) // 1
	fr.rawToStream()               // 4
	nn := fr.Length
	if n := int(nn) - len(fr.payload); n > 0 {
		// resizing payload buffer
		fr.payload = append(fr.payload, make([]byte, n)...)
	}
	n, err = br.Read(fr.payload[:nn])
	if err == nil {
		fr.payload = fr.payload[:n]
	}
	return int64(n), err
}

// WriteTo writes frame to bw encoding fr values.
func (fr *Frame) WriteTo(bw io.Writer) (nn int64, err error) {
	var n int
	// TODO: bw can be fr. Bug?
	fr.lengthToRaw()
	fr.header[3] = byte(fr.Type)
	fr.header[4] = byte(fr.Flags)
	fr.streamToRaw()

	n, err = bw.Write(fr.header)
	if err == nil {
		nn += int64(n)
		n, err = bw.Write(fr.payload)
	}
	nn += int64(n)
	return nn, err
}

func (fr *Frame) lengthToRaw() {
	_ = fr.header[2] // bound checking
	fr.header[0] = byte(fr.Length >> 16)
	fr.header[1] = byte(fr.Length >> 8)
	fr.header[2] = byte(fr.Length)
}

func uint32ToBytes(b []byte, n uint32) {
	// TODO: Delete first bit
	_ = b[3] // bound checking
	b[0] = byte(n >> 24)
	b[1] = byte(n >> 16)
	b[2] = byte(n >> 8)
	b[3] = byte(n)
}

func bytesToUint32(b []byte) uint32 {
	_ = b[3] // bound checking
	n := uint32(b[0])<<24 |
		uint32(b[1])<<16 |
		uint32(b[2])<<8 |
		uint32(b[3])
	return n & (1<<31 - 1)
}

func (fr *Frame) streamToRaw() {
	uint32ToBytes(fr.header[5:], fr.Stream)
}

func (fr *Frame) rawToLength() {
	_ = fr.header[2] // bound checking
	fr.Length = uint32(fr.header[0])<<16 |
		uint32(fr.header[1])<<8 |
		uint32(fr.header[2])
}

func (fr *Frame) rawToStream() {
	fr.Stream = bytesToUint32(fr.header[5:])
}
