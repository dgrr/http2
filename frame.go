package http2

import (
	"io"
	"sync"
)

const (
	// Frame default size
	// http://httpwg.org/specs/rfc7540.html#FrameHeader
	defaultFrameSize = 9
	// https://httpwg.org/specs/rfc7540.html#SETTINGS_MAX_FRAME_SIZE
	defaultMaxLen = 1 << 14
	maxPayloadLen = 1<<24 - 1

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

var framePool = sync.Pool{
	New: func() interface{} {
		fr := &Frame{}
		fr.Header.maxLen = defaultMaxLen
		return fr
	},
}

// Header represents HTTP/2 frame header.
type Header struct {
	noCopy noCopy

	// TODO: if length is granther than 16384 the body must not be
	// readed unless settings specify it

	// Len is the payload length
	Len    uint32 // 24 bits
	maxLen uint32

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
	h.Type = 0
	h.Flags = 0
	h.Stream = 0
	h.Len = 0
	h.maxLen = defaultMaxLen
}

// MaxLen returns maximum negotiated payload length.
func (h *Header) MaxLen() uint32 {
	return h.maxLen
}

// ReadFrom reads header from reader.
//
// Unlike io.ReaderFrom this method does not read until io.EOF
func (h *Header) ReadFrom(br io.Reader) (int64, error) {
	n, err := br.Read(h.rawHeader[:])
	if err == nil {
		if n != defaultFrameSize {
			err = Error(FrameSizeError)
		} else {
			h.rawToLen()                    // 3
			h.Type = uint8(h.rawHeader[3])  // 1
			h.Flags = uint8(h.rawHeader[4]) // 1
			h.rawToStream()                 // 4
		}
	}
	return int64(n), err
}

// WriteTo writes header to bw encoding the values.
//
// It is ready to be used with a net.Conn Writer.
func (h *Header) WriteTo(bw io.Writer) (int64, error) {
	// TODO: Bound checking? Debug.
	h.lengthToRaw()
	h.rawHeader[3] = byte(h.Type)
	h.rawHeader[4] = byte(h.Flags)
	h.streamToRaw()

	n, err := bw.Write(h.rawHeader[:])
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

// Header returns frame header bytes.
func (h *Header) Header() []byte {
	return h.rawHeader[:]
}

func uint32ToBytes(b []byte, n uint32) {
	_ = b[3] // bound checking
	b[0] = byte(n>>24) & (1<<8 - 1)
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

func (h *Header) lengthToRaw() {
	_ = h.rawHeader[2] // bound checking
	h.rawHeader[0] = byte(h.Len >> 16)
	h.rawHeader[1] = byte(h.Len >> 8)
	h.rawHeader[2] = byte(h.Len)
}

func (h *Header) streamToRaw() {
	uint32ToBytes(h.rawHeader[5:], h.Stream)
}

func (h *Header) rawToLen() {
	_ = h.rawHeader[2] // bound checking
	h.Len = uint32(h.rawHeader[0])<<16 |
		uint32(h.rawHeader[1])<<8 |
		uint32(h.rawHeader[2])
}

func (h *Header) rawToStream() {
	h.Stream = bytesToUint32(h.rawHeader[5:])
}

// Frame is frame representation of HTTP2 protocol
//
// Use AcquireFrame instead of creating Frame every time
// if you are going to use Frame as your own and ReleaseFrame to
// delete the Frame
//
// Frame instance MUST NOT be used from concurrently running goroutines.
type Frame struct {
	noCopy noCopy

	// Header is frame Header.
	//
	// Frame.Read function also fills Header values.
	Header Header

	payload []byte
}

// AcquireFrame gets a Frame from pool.
func AcquireFrame() *Frame {
	return framePool.Get().(*Frame)
}

// ReleaseFrame reset and puts fr to the pool.
func ReleaseFrame(fr *Frame) {
	fr.Reset()
	framePool.Put(fr)
}

// Reset resets the frame
func (fr *Frame) Reset() {
	fr.Header.Reset()
	fr.payload = fr.payload[:0]
}

// Payload returns processed payload deleting padding and additional headers.
func (fr *Frame) Payload() []byte {
	return fr.payload
}

// SetPayload sets new payload to fr
//
// This function returns ErrPayloadExceeds if
// payload length exceeds negotiated maximum size.
func (fr *Frame) SetPayload(b []byte) (err error) {
	_, err = fr.appendToCheckingLen(fr.payload[:0], b)
	return
}

// Write writes b to frame payload.
//
// This function is compatible with io.Writer
func (fr *Frame) Write(b []byte) (int, error) {
	return fr.AppendPayload(b)
}

// AppendPayload appends bytes to frame payload
func (fr *Frame) AppendPayload(b []byte) (int, error) {
	return fr.appendToCheckingLen(fr.payload, b)
}

func (fr *Frame) appendToCheckingLen(b, bb []byte) (n int, err error) {
	n = len(bb)
	if uint32(n+len(b)) > fr.Header.maxLen {
		err = ErrPayloadExceeds
	} else {
		fr.payload = append(b, bb...)
		fr.Header.Len = uint32(len(fr.payload))
	}
	return
}

// ReadFrom reads frame header and payload from br
//
// Frame.ReadFrom it's compatible with io.ReaderFrom
// but this implementation does not read until io.EOF
func (fr *Frame) ReadFrom(br io.Reader) (int64, error) {
	_, err := fr.Header.ReadFrom(br)
	if err != nil {
		return 0, err
	}
	nn := fr.Header.Len
	if n := int(nn) - len(fr.payload); n > 0 {
		// resizing payload buffer
		fr.payload = append(fr.payload, make([]byte, n)...)
	}
	n, err := br.Read(fr.payload[:nn])
	if err == nil {
		fr.payload = fr.payload[:n]
	}
	return int64(n), err
}

// WriteTo writes header and payload to bw encoding fr values.
func (fr *Frame) WriteTo(bw io.Writer) (nn int64, err error) {
	var n int
	nn, err = fr.Header.WriteTo(bw)
	if err == nil {
		n, err = bw.Write(fr.payload)
		nn += int64(n)
	}
	return nn, err
}
