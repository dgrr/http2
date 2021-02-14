package http2

import (
	"fmt"
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

	// Frame Flag (described along the frame types)
	// More flags have been ignored due to redundancy
	FlagAck        FrameFlags = 0x1
	FlagEndStream  FrameFlags = 0x1
	FlagEndHeaders FrameFlags = 0x4
	FlagPadded     FrameFlags = 0x8
	FlagPriority   FrameFlags = 0x20
)

type FrameType int8

func (ft FrameType) String() string {
	switch ft {
	case FrameData:
		return "FrameData"
	case FrameHeaders:
		return "FrameHeaders"
	case FramePriority:
		return "FramePriority"
	case FrameResetStream:
		return "FrameResetStream"
	case FrameSettings:
		return "FrameSettings"
	case FramePushPromise:
		return "FramePushPromise"
	case FramePing:
		return "FramePing"
	case FrameGoAway:
		return "FrameGoAway"
	case FrameWindowUpdate:
		return "FrameWindowUpdate"
	case FrameContinuation:
		return "FrameContinuation"
	}
	return "IDK"
}

type FrameFlags int8

// TODO: Develop methods for FrameFlags

var framePool = sync.Pool{
	New: func() interface{} {
		return &Frame{}
	},
}

// Frame is frame representation of HTTP2 protocol
//
// Use AcquireFrame instead of creating Frame every time
// if you are going to use Frame as your own and ReleaseFrame to
// delete the Frame
//
// Frame instance MUST NOT be used from different goroutines.
//
// https://tools.ietf.org/html/rfc7540#section-4.1
type Frame struct {
	length uint32     // 24 bits
	kind   FrameType  // 8 bits
	flags  FrameFlags // 8 bits
	stream uint32     // 31 bits

	maxLen uint32

	rawHeader [defaultFrameSize]byte
	payload   []byte
}

// AcquireFrame gets a Frame from pool.
func AcquireFrame() *Frame {
	fr := framePool.Get().(*Frame)
	fr.Reset()
	return fr
}

// ReleaseFrame reset and puts fr to the pool.
func ReleaseFrame(fr *Frame) {
	framePool.Put(fr)
}

// Reset resets header values.
func (fr *Frame) Reset() {
	fr.kind = 0
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
func (fr *Frame) Type() FrameType {
	return fr.kind
}

func (fr *Frame) Flags() FrameFlags {
	return fr.flags
}

// Setkind sets the frame type for the current frame.
func (fr *Frame) SetType(kind FrameType) {
	fr.kind = kind
}

// Stream returns the stream id of the current frame.
func (fr *Frame) Stream() uint32 {
	return fr.stream
}

// SetStream sets the stream id on the current frame.
//
// This function DOESN'T delete the reserved bit (first bit)
// in order to support personalized implementations of the protocol.
func (fr *Frame) SetStream(stream uint32) {
	fr.stream = stream
}

// Len returns the payload length
func (fr *Frame) Len() uint32 {
	return fr.length
}

// MaxLen returns max negotiated payload length.
func (fr *Frame) MaxLen() uint32 {
	return fr.maxLen
}

// SetMaxLen sets max payload length to read.
func (fr *Frame) SetMaxLen(maxLen uint32) {
	fr.maxLen = maxLen
}

// HasFlag returns if `f` is in the frame flags or not.
func (fr *Frame) HasFlag(f FrameFlags) bool {
	return fr.flags&f == f
}

// AddFlag adds a flag to frame flags.
func (fr *Frame) AddFlag(f FrameFlags) {
	fr.flags |= f
}

// DelFlag deletes f from frame flags
func (fr *Frame) DelFlag(f FrameFlags) {
	fr.flags ^= f
}

// RawHeader returns frame header bytes.
func (fr *Frame) RawHeader() []byte {
	return fr.rawHeader[:]
}

func (fr *Frame) parseValues() {
	fr.rawToLen()                          // 3
	fr.kind = FrameType(fr.rawHeader[3])   // 1
	fr.flags = FrameFlags(fr.rawHeader[4]) // 1
	fr.rawToStream()                       // 4
}

func (fr *Frame) parseHeader() {
	fr.lenToRaw()                    // 2
	fr.rawHeader[3] = byte(fr.kind)  // 1
	fr.rawHeader[4] = byte(fr.flags) // 1
	fr.streamToRaw()                 // 4
}

// ReadFrom reads frame from Reader.
//
// This function returns readed bytes and/or error.
//
// Unlike io.ReaderFrom this method does not read until io.EOF
func (fr *Frame) ReadFrom(br io.Reader) (int64, error) {
	return fr.readFrom(br, 0)
}

// ReadFromLimitPayload reads frame from reader limiting the payload.
func (fr *Frame) ReadFromLimitPayload(br io.Reader, max uint32) (int64, error) {
	return fr.readFrom(br, max)
}

// TODO: Delete rb?
func (fr *Frame) readFrom(br io.Reader, max uint32) (int64, error) {
	n, err := br.Read(fr.rawHeader[:])
	if err != nil {
		return -1, err
	}

	rn := int64(n)

	if n != defaultFrameSize {
		err = Error(FrameSizeError) // TODO: ?
	} else {
		// Parsing Frame's Header field.
		fr.parseValues()
		if max > 0 && fr.length > max {
			// TODO: Discard bytes and return an error
		} else if fr.length > 0 {
			// uint32 should be extended to int64.
			nn := int64(fr.length)
			if nn < 0 {
				panic(fmt.Sprintf("length is lower than 0 (%d). Overflow? (%d)", nn, fr.length))
			}
			fr.payload = resize(fr.payload, nn)

			n, err = br.Read(fr.payload[:nn])
			if err == nil {
				rn += int64(n)
				fr.payload = fr.payload[:n]
			}
		}
	}

	return rn, err
}

// WriteTo writes frame to the Writer.
//
// This function returns Frame bytes written and/or error.
func (fr *Frame) WriteTo(w io.Writer) (wb int64, err error) {
	var n int
	fr.parseHeader()

	n, err = w.Write(fr.rawHeader[:])
	if err == nil {
		wb += int64(n)
		n, err = w.Write(fr.payload[:fr.length]) // TODO: Must payload be limited here?
		if err == nil {
			wb += int64(n)
		}
	}

	return
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
// TODO: Split a frame if the length is exceeded
func (fr *Frame) AppendPayload(src []byte) (int, error) {
	return fr.appendCheckingLen(fr.payload, src)
}

func (fr *Frame) appendCheckingLen(dst, src []byte) (n int, err error) {
	n = len(src)
	if fr.maxLen > 0 && uint32(n+len(dst)) > fr.maxLen {
		err = ErrPayloadExceeds
	} else {
		fr.payload = append(dst, src...)
		fr.length = uint32(len(fr.payload))
	}

	return
}
