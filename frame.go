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
	maxPayloadLen = 1<<24 - 1 // this value cannot be exceeded because of the length frame field.

	// FrameType (http://httpwg.org/specs/rfc7540.html#Frame_types)
	// TODO: Define new type? type Frame uint8. There are any disadvantage?
	FrameData         uint8 = 0x0
	FrameHeaders      uint8 = 0x1
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
		fr.maxLen = defaultMaxLen
		return fr
	},
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

	// TODO: if length is granther than 16384 the body must not be
	// readed unless settings specify it

	// Len is the payload length
	length uint32 // 24 bits
	maxLen uint32

	_type uint8 // 8 bits

	// flags is the flags the frame contains
	flags uint8 // 8 bits

	// Stream is the id of the stream
	stream uint32 // 31 bits

	rawHeader [defaultFrameSize]byte
	payload   []byte
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

// Reset resets header values.
func (fr *Frame) Reset() {
	fr._type = 0
	fr.flags = 0
	fr.stream = 0
	fr.length = 0
	fr.maxLen = defaultMaxLen
	resetBytes(fr.rawHeader[:])
	fr.payload = fr.payload[:0]
}

func resetBytes(b []byte) { // TODO: to asm using SSE if possible (github.com/tmthrgd/go-memset)
	n := len(b)
	for i := 0; i < n; i++ {
		b[i] = 0
	}
}

// Type returns the frame type (https://httpwg.org/specs/rfc7540.html#Frame_types)
func (fr *Frame) Type() uint8 {
	return fr._type
}

// Set_type sets the frame type for the current frame.
func (fr *Frame) SetType(_type uint8) {
	fr._type = _type
}

// Stream returns the stream id of the current frame.
func (fr *Frame) Stream() uint32 {
	return fr.stream
}

// SetStreams sets the stream id on the current frame.
//
// This function deletes the reserved bit (first bit).
func (fr *Frame) SetStream(stream uint32) {
	fr.stream = stream & (1<<31 - 1) // TODO: Delete the first bit?
}

// Len returns the payload length
func (fr *Frame) Len() uint32 {
	return fr.length
}

// MaxLen returns max negotiated payload length.
func (fr *Frame) MaxLen() uint32 {
	return fr.maxLen
}

// SetMaxLen sets max payload length to be readed.
func (fr *Frame) SetMaxLen(maxLen uint32) {
	fr.maxLen = maxLen
}

// Has returns boolean value indicating if frame flags has f
func (fr *Frame) Has(f uint8) bool {
	return (fr.flags & f) == f
}

// Add adds a flag to frame flags.
func (fr *Frame) Add(f uint8) {
	fr.flags |= f
}

// Delete deletes f from frame flags
func (fr *Frame) Delete(f uint8) {
	fr.flags ^= f
}

// Header returns frame header bytes.
func (fr *Frame) Header() []byte {
	return fr.rawHeader[:]
}

func (fr *Frame) parseValues() {
	fr.rawToLen()                     // 3
	fr._type = uint8(fr.rawHeader[3]) // 1
	fr.flags = uint8(fr.rawHeader[4]) // 1
	fr.rawToStream()                  // 4
}

func (fr *Frame) parseHeader() {
	fr.lenToRaw()                    // 2
	fr.rawHeader[3] = byte(fr._type) // 1
	fr.rawHeader[4] = byte(fr.flags) // 1
	fr.streamToRaw()                 // 4
}

// ReadFrom reads frame from Reader.
//
// This function returns readed bytes and/or error.
//
// Unlike io.ReaderFrom this method does not read until io.EOF
func (fr *Frame) ReadFrom(br io.Reader) (rdb int64, err error) {
	var n int
	n, err = br.Read(fr.rawHeader[:])
	if err == nil {
		if n != defaultFrameSize {
			err = Error(FrameSizeError) // TODO: ?
		} else {
			rdb += int64(n)
			// parsing length and other fields.
			fr.parseValues()
			if fr.length > fr.maxLen {
				// TODO: error oversize
			} else if fr.length > 0 {
				// uint32 must be extended to int64.
				fr.payload = fr.payload[:cap(fr.payload)]
				nn := int64(fr.length) - int64(cap(fr.payload))
				if nn > 0 {
					// TODO: ...
					fr.payload = append(fr.payload, make([]byte, nn)...)
				}
				nn = int64(fr.length) // TODO: Change nn by fr.Len?
				n, err = br.Read(fr.payload[:nn])
				if err == nil {
					rdb += int64(n)
					fr.payload = fr.payload[:n]
				}
			}
		}
	}
	return
}

// WriteTo writes frame to the Writer.
//
// This function returns Frame bytes written and/or error.
func (fr *Frame) WriteTo(bw io.Writer) (wrb int64, err error) {
	var n int
	fr.parseHeader()

	n, err = bw.Write(fr.rawHeader[:])
	if err == nil {
		wrb += int64(n)
		n, err = bw.Write(fr.payload[:fr.length]) // TODO: Must payload be limited here?
		if err == nil {
			wrb += int64(n)
		}
	}
	return
}

func uint24ToBytes(b []byte, n uint32) {
	_ = b[2] // bound cfrecking
	b[0] = byte(n >> 16)
	b[1] = byte(n >> 8)
	b[2] = byte(n)
}

func bytesToUint24(b []byte) uint32 {
	_ = b[2] // bound checking
	return uint32(b[0])<<16 |
		uint32(b[1])<<8 |
		uint32(b[2])
}

func appendUint32Bytes(dst []byte, n uint32) []byte {
	dst = append(dst, byte(n>>24))
	dst = append(dst, byte(n>>16))
	dst = append(dst, byte(n>>8))
	dst = append(dst, byte(n))
	return dst
}

func uint32ToBytes(b []byte, n uint32) {
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
	return n
}

func (fr *Frame) rawToStream() {
	fr.stream = bytesToUint32(fr.rawHeader[5:]) & (1<<31 - 1)
}

func (fr *Frame) streamToRaw() {
	uint32ToBytes(fr.rawHeader[5:], fr.stream)
}

func (fr *Frame) rawToLen() {
	fr.length = bytesToUint24(fr.rawHeader[:3]) // & (1<<24 - 1)
}

func (fr *Frame) lenToRaw() {
	uint24ToBytes(fr.rawHeader[:3], fr.length)
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
	_, err = fr.appendCheckingLen(fr.payload[:0], b)
	return
}

// Write writes b to frame payload.
//
// If the result payload exceeds the max length ErrPayloadExceeds is returned.
// TODO: Add example of continuation frame
func (fr *Frame) Write(b []byte) (int, error) {
	return fr.AppendPayload(b)
}

// AppendPayload appends bytes to frame payload
//
// If the result payload exceeds the max length ErrPayloadExceeds is returned.
// TODO: Add example of continuation frame
func (fr *Frame) AppendPayload(b []byte) (int, error) {
	return fr.appendCheckingLen(fr.payload, b)
}

func (fr *Frame) appendCheckingLen(b, bb []byte) (n int, err error) {
	n = len(bb)
	if uint32(n+len(b)) > fr.maxLen {
		err = ErrPayloadExceeds
	} else {
		fr.payload = append(b, bb...)
		fr.length = uint32(len(fr.payload))
	}
	return
}
