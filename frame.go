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

// Header represents HTTP/2 frame header.
type Header struct {
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

	rawHeader [defaultFrameSize]byte
}

// Reset resets header values.
func (h *Header) Reset() {
	fr.Type = 0
	fr.Flags = 0
	fr.Stream = 0
	fr.Length = 0
}

// ReadFrom reads header from reader.
//
// Unlike io.ReaderFrom this method does not read until io.EOF
func (h *Header) ReadFrom(br io.Reader) (int64, error) {
	n, err := h.Read(h.rawHeader[:])
	if err != nil {
		return n, err
	}
	if n != defaultFrameSize {
		return n, Error(FrameSizeError)
	}
	h.rawToLength()               // 3
	h.Type = uint8(fr.header[3])  // 1
	h.Flags = uint8(fr.header[4]) // 1
	h.rawToStream()               // 4
	return int64(n), err
}

// WriteTo writes header to bw encoding the values.
func (h *Header) WriteTo(bw io.Writer) (int64, error) {
	var n int
	// encoding header
	h.lengthToRaw()
	h.rawHeader[3] = byte(fr.Type)
	h.rawHeader[4] = byte(fr.Flags)
	h.streamToRaw()

	n, err = bw.Write(fr.rawHeader)
	return int64(n), err
}

// Is returns boolean value indicating if frame Type is t
func (h *Header) Is(t uint8) bool {
	return (h.Type == t)
}

// Has returns boolean value indicating if frame Flags has f
func (h *Header) Has(f uint8) bool {
	return (h.Flags & f) == f
}

// Set sets type to h.
func (h *Header) Set(t uint8) {
	h.Type = t
}

// Add adds a flag to h.
func (h *Header) Add(f uint8) {
	h.Flags |= f
}

// Delete deletes f from Flags
func (h *Header) Delete(f uint8) {
	h.Flags ^= f
}

// Header returns header bytes of fr
func (h *Header) RawHeader() []byte {
	return h.rawHeader
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
	Header Header

	payload []byte
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
	fr.Header.Reset()
	fr.payload = fr.payload[:0]
	//releaseByte(fr.payload[:cap(fr.payload)])
}

// Payload returns processed payload deleting padding and additional headers.
func (fr *Frame) Payload() []byte {
	return fr.payload
}

// SetPayload sets new payload to fr
func (fr *Frame) SetPayload(b []byte) {
	fr.payload = append(fr.payload[:0], b...)
	fr.Header.Length = uint32(len(fr.payload))
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
	fr.Header.Length += uint32(n)
	return n
}

// ReadFrom reads frame header and payload from br
//
// Frame.ReadFrom it's compatible with io.ReaderFrom
// but this implementation does not read until io.eof
func (fr *Frame) ReadFrom(br io.Reader) (int64, error) {
	_, err := fr.Header.ReadFrom(br)
	if err != nil {
		return 0, err
	}
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

// WriteTo writes header and payload to bw encoding fr values.
func (fr *Frame) WriteTo(bw io.Writer) (nn int64, err error) {
	var n int
	n, err = fr.Header.WriteTo(bw)
	if err == nil {
		nn += int64(n)
		n, err = bw.Write(fr.payload)
	}
	nn += int64(n)
	return nn, err
}
