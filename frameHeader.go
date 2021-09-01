package http2

import (
	"bufio"
	"fmt"
	"io"
	"sync"

	"github.com/dgrr/http2/http2utils"
)

const (
	// FrameHeader default size
	// http://httpwg.org/specs/rfc7540.html#FrameHeader
	DefaultFrameSize = 9
	// https://httpwg.org/specs/rfc7540.html#SETTINGS_MAX_FRAME_SIZE
	defaultMaxLen = 1 << 14

	// Frame Flag (described along the frame types)
	// More flags have been ignored due to redundancy
	FlagAck        FrameFlags = 0x1
	FlagEndStream  FrameFlags = 0x1
	FlagEndHeaders FrameFlags = 0x4
	FlagPadded     FrameFlags = 0x8
	FlagPriority   FrameFlags = 0x20
)

// TODO: Develop methods for FrameFlags

var frameHeaderPool = sync.Pool{
	New: func() interface{} {
		return &FrameHeader{}
	},
}

// FrameHeader is frame representation of HTTP2 protocol
//
// Use AcquireFrameHeader instead of creating FrameHeader every time
// if you are going to use FrameHeader as your own and ReleaseFrameHeader to
// delete the FrameHeader
//
// FrameHeader instance MUST NOT be used from different goroutines.
//
// https://tools.ietf.org/html/rfc7540#section-4.1
type FrameHeader struct {
	length int        // 24 bits
	kind   FrameType  // 8 bits
	flags  FrameFlags // 8 bits
	stream uint32     // 31 bits

	maxLen uint32

	rawHeader [DefaultFrameSize]byte
	payload   []byte

	fr Frame
}

// AcquireFrameHeader gets a FrameHeader from pool.
func AcquireFrameHeader() *FrameHeader {
	fr := frameHeaderPool.Get().(*FrameHeader)
	fr.Reset()
	return fr
}

// ReleaseFrameHeader reset and puts fr to the pool.
func ReleaseFrameHeader(fr *FrameHeader) {
	ReleaseFrame(fr.Body())
	frameHeaderPool.Put(fr)
}

// Reset resets header values.
func (frh *FrameHeader) Reset() {
	frh.kind = 0
	frh.flags = 0
	frh.stream = 0
	frh.length = 0
	frh.maxLen = defaultMaxLen
	frh.fr = nil
	frh.payload = frh.payload[:0]
}

// Type returns the frame type (https://httpwg.org/specs/rfc7540.html#Frame_types)
func (frh *FrameHeader) Type() FrameType {
	return frh.kind
}

// Flags ...
func (frh *FrameHeader) Flags() FrameFlags {
	return frh.flags
}

func (frh *FrameHeader) SetFlags(flags FrameFlags) {
	frh.flags = flags
}

// Stream returns the stream id of the current frame.
func (frh *FrameHeader) Stream() uint32 {
	return frh.stream
}

// SetStream sets the stream id on the current frame.
//
// This function DOESN'T delete the reserved bit (first bit)
// in order to support personalized implementations of the protocol.
func (frh *FrameHeader) SetStream(stream uint32) {
	frh.stream = stream
}

// Len returns the payload length
func (frh *FrameHeader) Len() int {
	return frh.length
}

// MaxLen returns max negotiated payload length.
func (frh *FrameHeader) MaxLen() uint32 {
	return frh.maxLen
}

func (frh *FrameHeader) parseValues(header []byte) {
	frh.length = int(http2utils.BytesToUint24(header[:3]))          // & (1<<24 - 1)    // 3
	frh.kind = FrameType(header[3])                                 // 1
	frh.flags = FrameFlags(header[4])                               // 1
	frh.stream = http2utils.BytesToUint32(header[5:]) & (1<<31 - 1) // 4
}

func (frh *FrameHeader) parseHeader(header []byte) {
	http2utils.Uint24ToBytes(header[:3], uint32(frh.length)) // 2
	header[3] = byte(frh.kind)                               // 1
	header[4] = byte(frh.flags)                              // 1
	http2utils.Uint32ToBytes(header[5:], frh.stream)         // 4
}

// ReadFrameFrom ...
func ReadFrameFrom(br *bufio.Reader) (*FrameHeader, error) {
	fr := AcquireFrameHeader()

	_, err := fr.ReadFrom(br)
	if err != nil {
		if fr.Body() != nil {
			ReleaseFrameHeader(fr)
		} else {
			frameHeaderPool.Put(fr)
		}

		fr = nil
	}

	return fr, err
}
func ReadFrameFromWithSize(br *bufio.Reader, max uint32) (*FrameHeader, error) {
	fr := AcquireFrameHeader()
	fr.maxLen = max
	_, err := fr.ReadFrom(br)
	if err != nil {
		if fr.Body() != nil {
			ReleaseFrameHeader(fr)
		} else {
			frameHeaderPool.Put(fr)
		}

		fr = nil
	}

	return fr, err
}

// ReadFrom reads frame from Reader.
//
// This function returns read bytes and/or error.
//
// Unlike io.ReaderFrom this method does not read until io.EOF
func (frh *FrameHeader) ReadFrom(br *bufio.Reader) (int64, error) {
	return frh.readFrom(br)
}

// TODO: Delete rb?
func (frh *FrameHeader) readFrom(br *bufio.Reader) (int64, error) {
	header, err := br.Peek(DefaultFrameSize)
	if err != nil {
		return -1, err
	}

	br.Discard(DefaultFrameSize)

	rn := int64(DefaultFrameSize)

	// Parsing FrameHeader's Header field.
	frh.parseValues(header)
	if err := frh.checkLen(); err != nil {
		return 0, err
	}

	if frh.kind > FrameContinuation {
		br.Discard(frh.length)
		return 0, ErrUnknowFrameType
	}
	frh.fr = AcquireFrame(frh.kind)

	// if max > 0 && frh.length > max {
	// TODO: Discard bytes and return an error
	if frh.length > 0 {
		n := frh.length
		if n < 0 {
			panic(fmt.Sprintf("length is less than 0 (%d). Overflow? (%d)", n, frh.length))
		}

		frh.payload = http2utils.Resize(frh.payload, n)

		n, err = io.ReadFull(br, frh.payload[:n])
		rn += int64(n)
	}

	return rn, frh.fr.Deserialize(frh)
}

// WriteTo writes frame to the Writer.
//
// This function returns FrameHeader bytes written and/or error.
func (frh *FrameHeader) WriteTo(w *bufio.Writer) (wb int64, err error) {
	frh.fr.Serialize(frh)

	frh.length = len(frh.payload)
	frh.parseHeader(frh.rawHeader[:])

	n, err := w.Write(frh.rawHeader[:])
	if err == nil {
		wb += int64(n)

		n, err = w.Write(frh.payload)
		wb += int64(n)
	}

	return wb, err
}

// Body ...
func (frh *FrameHeader) Body() Frame {
	return frh.fr
}

func (frh *FrameHeader) SetBody(fr Frame) {
	if fr == nil {
		panic("Body cannot be nil")
	}

	frh.kind = fr.Type()
	frh.fr = fr
}

func (fhr *FrameHeader) setPayload(payload []byte) {
	fhr.payload = append(fhr.payload[:0], payload...)
}

func (fhr *FrameHeader) checkLen() error {
	if fhr.maxLen != 0 && fhr.length > int(fhr.maxLen) {
		return ErrPayloadExceeds
	}
	return nil
}

func (frh *FrameHeader) appendCheckingLen(dst, src []byte) (n int, err error) {
	n = len(src)
	if frh.maxLen > 0 && uint32(n+len(dst)) > frh.maxLen {
		err = ErrPayloadExceeds
	} else {
		frh.payload = append(dst, src...)
		frh.length = len(frh.payload)
	}

	return
}
