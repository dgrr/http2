package fasthttp

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

// Byteorder must be big endian
// Values are unsigned unless otherwise indicated

const (
	h2TLSProto = "h2"
	h2Clean    = "h2c"
	// Frame default size
	// http://httpwg.org/specs/rfc7540.html#FrameHeader
	defaultFrameSize = 9 // bytes

	// default Settings parameters
	defaultHeaderTableSize   uint32 = 4096
	defaultConcurrentStreams uint32 = 100
	defaultWindowSize        uint32 = 1<<15 - 1
	defaultMaxFrameSize      uint32 = 1 << 14

	maxWindowSize = 1<<31 - 1
	maxFrameSize  = 1<<24 - 1

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

	// FrameSettings string values (https://httpwg.org/specs/rfc7540.html#SettingValues)
	HeaderTableSize      uint16 = 0x1
	EnablePush           uint16 = 0x2
	MaxConcurrentStreams uint16 = 0x3
	InitialWindowSize    uint16 = 0x4
	MaxFrameSize         uint16 = 0x5
	MaxHeaderListSize    uint16 = 0x6

	// Error codes (http://httpwg.org/specs/rfc7540.html#ErrorCodes)
	//
	// Errors must be uint32 because of FrameReset
	NoError              uint32 = 0x0
	ProtocolError        uint32 = 0x1
	InternalError        uint32 = 0x2
	FlowControlError     uint32 = 0x3
	SettingsTimeoutError uint32 = 0x4
	StreamClosedError    uint32 = 0x5
	FrameSizeError       uint32 = 0x6
	RefusedStreamError   uint32 = 0x7
	CancelError          uint32 = 0x8
	CompressionError     uint32 = 0x9
	ConnectionError      uint32 = 0xa
	EnhanceYourCalm      uint32 = 0xb
	InadequateSecurity   uint32 = 0xc
	HTTP11Required       uint32 = 0xd
)

var (
	// http://httpwg.org/specs/rfc7540.html#ConnectionHeader
	http2Preface = []byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")

	framePool = sync.Pool{
		New: func() interface{} {
			return &Frame{
				header: make([]byte, defaultFrameSize),
			} // TODO: Create new pool or reuse any of fasthttp existing pools for rawFrame?
		},
	}
	settingsPool = sync.Pool{
		New: func() interface{} {
			return &Settings{
				HeaderTableSize:      defaultHeaderTableSize,
				MaxConcurrentStreams: defaultConcurrentStreams,
				InitialWindowSize:    defaultWindowSize,
				MaxFrameSize:         defaultMaxFrameSize,
			}
		},
	}
)

type errHTTP2 struct {
	err         error
	frameToSend byte
}

func (e errHTTP2) Error() string {
	return e.err.Error()
}

func ErrorH2(code uint32) error {
	if code >= 0x0 && code <= 0x13 {
		return errHTTP2{
			err:         errParser[code],
			frameToSend: FrameResetStream,
		}
	}
	return nil
}

var (
	errParser = []error{
		NoError:              errors.New("No errors"),
		ProtocolError:        errors.New("Protocol error"),
		InternalError:        errors.New("Internal error"),
		FlowControlError:     errors.New("Flow control error"),
		SettingsTimeoutError: errors.New("Settings timeout"),
		StreamClosedError:    errors.New("Stream have been closed"),
		FrameSizeError:       errors.New("Frame size error"),
		RefusedStreamError:   errors.New("Refused Stream"),
		CancelError:          errors.New("Canceled"),
		CompressionError:     errors.New("Compression error"),
		ConnectionError:      errors.New("Connection error"),
		EnhanceYourCalm:      errors.New("Enhance your calm"),
		InadequateSecurity:   errors.New("Inadequate security"),
		HTTP11Required:       errors.New("HTTP/1.1 required"),
	}
	// This error codes must be used with FrameGoAway
	errUnknowFrameType = errors.New("error bad frame type")
	errZeroPayload     = errors.New("frame Payload len = 0")
	errBadPreface      = errors.New("bad preface size")
	errFrameMismatch   = errors.New("frame type mismatch from called function")
	errNilWriter       = errors.New("Writer cannot be nil")
	errNilReader       = errors.New("Reader cannot be nil")
	errUnknown         = errors.New("Unknown error")
	errBitOverflow     = errors.New("Bit overflow")
)

// Stream states
const (
	h2idle = iota
	h2open
	h2half_closed
	h2closed
)

func readPreface(br *bufio.Reader) bool {
	n := len(http2Preface)
	b, err := br.Peek(n)
	if err == nil {
		if bytes.Equal(b, http2Preface) {
			// Discard HTTP2 preface
			br.Discard(n)
			b = nil
			return true
		}
	}
	b = nil
	return false
}

func upgradeTLS(c net.Conn) *bufio.Reader {
	cc, isTLS := c.(connTLSer)
	if isTLS {
		state := cc.ConnectionState()
		if state.NegotiatedProtocol != h2TLSProto || state.Version < tls.VersionTLS12 {
			br := bufio.NewReader(c)
			if readPreface(br) {
				return br
			}
			return nil
		}
		// HTTP2 using TLS must be used with TLS1.2 or higher (https://httpwg.org/specs/rfc7540.html#TLSUsage)
		//
		// successful HTTP2 protocol negotiation
		return bufio.NewReader(c)
	}

	return nil
}

func upgradeHTTP(ctx *RequestCtx) bool {
	if ctx.Request.Header.ConnectionUpgrade() &&
		b2s(ctx.Request.Header.Peek("Upgrade")) == h2Clean {
		// HTTP2 connection stablished via HTTP
		ctx.SetStatusCode(StatusSwitchingProtocols)
		ctx.Response.Header.Add("Connection", "Upgrade")
		ctx.Response.Header.Add("Upgrade", h2Clean)
		// TODO: Add support for http2-settings header value
		return true
	}
	// The server connection preface consists of a potentially empty SETTINGS frame
	// (Section 6.5) that MUST be the first frame the server sends in the HTTP/2 connection.
	// (https://httpwg.org/specs/rfc7540.html#ConnectionHeader)
	// TODO: Write settings frame
	// TODO: receive preface
	// TODO: Read settings and store it in Request.st
	return false
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
	fr.payload = fr.payload[:cap(fr.payload)]
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

// serveConnH2 serves HTTP2 connection
// TODO: Apply restrictions
func (s *Server) serveConnH2(conn net.Conn, br *bufio.Reader) (err error) {
	if br == nil {
		br = bufio.NewReader(conn)
	}
	var (
		bw      = bufio.NewWriter(conn)
		streams = make(map[uint32]*RequestCtx)
		// One HTTP2 connection can have more than one context
		ok    bool
		maxID uint32
		isTLS bool
		//lastFrame uint8 = 0xff
		nextFrame uint8 = 0xff
		ctx       *RequestCtx
	)
	_, isTLS = conn.(connTLSer)
	fr := AcquireFrame()
	h2Opts := AcquireSettings()
	hpack := AcquireHPack()
	defer ReleaseFrame(fr)
	defer ReleaseSettings(h2Opts)
	defer ReleaseHPack(hpack)

	err = writeSettings(s.H2Settings, bw)
	if err != nil {
		// TODO: Report error
		return err
	}
	bw.Flush()

	for {
		if atomic.LoadInt32(&s.stop) == 1 {
			err = nil
			break
		}
		fr.Reset()
		_, err = fr.ReadFrom(br)
		if err != nil {
			break
		}
		// A stream identifier of zero (0x0) is used for connection control messages
		// [...] Therefore, stream 0x1 cannot be selected as a new stream identifier
		// by a client that upgrades from HTTP/1.1.
		// (https://httpwg.org/specs/rfc7540.html#StreamsLayer)
		if fr.Stream != 0x0 {
			// TODO: Check stream of 0x1 (can be checked with isTLS var)
			ctx, ok = streams[fr.Stream]
			if !ok {
				// Streams initiated by a client MUST use odd-numbered stream identifiers
				// and must be greater than the last one.
				// https://httpwg.org/specs/rfc7540.html#StreamIdentifiers
				if fr.Stream&1 == 0 || fr.Stream <= maxID {
					// TODO: See https://httpwg.org/specs/rfc7540.html#Reliability
					err = errHTTP2{
						err:         errParser[RefusedStreamError],
						frameToSend: FrameResetStream,
					}
					// TODO: do not break
					break
				}
				ctx = s.acquireCtx(conn)
				maxID = fr.Stream
				ctx.Request.isTLS = isTLS
				ctx.Request.Header.hpack = hpack
				ctx.Request.Header.isHTTP2 = true
				ctx.Request.Header.disableNormalizing = true
				ctx.Response.isHTTP2 = true
				ctx.Response.Header.hpack = hpack
				ctx.Response.Header.isHTTP2 = true
				ctx.Response.Header.disableNormalizing = true
			}
			if nextFrame != 0xff && nextFrame != fr.Type {
				s.Logger.Printf("Unexpected frame %d<>%d\n", nextFrame, fr.Type)
				continue
			}
		}
		switch fr.Type {
		case FrameData:
			nextFrame, err = parseFrameData(fr, ctx)
		case FrameHeader:
			nextFrame, err = parseFrameHeaders(fr, ctx)
		case FramePriority:
			// TODO:
			stream := bytesToUint32(fr.payload[:4])
			weight := uint8(fr.payload[4])
			fr.payload = fr.payload[5:]
			_ = stream
			_ = weight
		case FrameResetStream:
			code := bytesToUint32(fr.payload[:4])
			if code > HTTP11Required {
				// TODO: Handle
				return nil
			}
			ctx.s.Logger.Printf("Reset frame received: %s\n", errParser[code])
		case FrameSettings:
			if !fr.Has(FlagAck) {
				// TODO: Error if flag ack and length != 0 or stream != 0
				//
				// Receipt of a SETTINGS frame with the ACK flag set and a
				// length field value other than 0 MUST be treated as a connection error
				h2Opts.Decode(fr.payload)
				// Response
				writeSettings(nil, bw)
			}
			continue
		case FramePushPromise:
			nextFrame, err = parseFramePushPromise(fr, ctx)
		case FramePing:
			parseFramePing(fr, ctx)
		case FrameGoAway:
			err = parseFrameGoAway(fr, ctx)
		case FrameWindowUpdate:
			// TODO:
			continue
		case FrameContinuation:
			nextFrame, err = parseFrameContinuation(fr, ctx)
		default:
			err = errHTTP2{
				err:         errUnknowFrameType,
				frameToSend: FrameGoAway,
			}
		}
		if err != nil {
			break
		}
		if nextFrame == 0xff {
			s.Handler(ctx)
			writeResponse(ctx, bw)
			bw.Flush()
		}
		//lastFrame = fr.Type
		streams[fr.Stream] = ctx
	}
	for _, ctx = range streams {
		s.releaseCtx(ctx)
	}
	streams = nil
	// TODO: Report error
	return err
}

// TODO: See https://blog.minio.io/golang-internals-part-2-nice-benefits-of-named-return-values-1e95305c8687

// TODO: Handle errors
// https://httpwg.org/specs/rfc7540.html#DATA
func parseFrameData(fr *Frame, ctx *RequestCtx) (nextFrame uint8, err error) {
	nextFrame = 0xff
	if fr.Has(FlagPadded) {
		// Getting padding from the first byte of readed payload
		fr.Length -= uint32(fr.payload[0])
		ctx.Request.AppendBody(fr.payload[1:fr.Length])
		fr.Length--
	}
	if fr.Has(FlagEndStream) {
		ctx.state = h2half_closed
	}
	return
}

// https://httpwg.org/specs/rfc7540.html#HEADERS
func parseFrameHeaders(fr *Frame, ctx *RequestCtx) (nextFrame uint8, err error) {
	nextFrame = FrameContinuation
	// If frame is Header and Stream is idle the Stream must
	// be changed to the open state.
	// If frame state is open right now we must check FlagEndStream
	// flag value. If it is found the stream must be changed to RST.
	// https://httpwg.org/specs/rfc7540.html#StreamStates
	// TODO: Check Push Promise
	if ctx.state == h2idle {
		ctx.state = h2open
	} else if ctx.state == h2half_closed {
		ctx.state = h2closed
	}
	if fr.Has(FlagPadded) {
		fr.Length -= uint32(fr.payload[0])
		fr.payload = fr.payload[1:fr.Length]
		fr.Length--
	}
	if fr.Has(FlagPriority) {
		// TODO: Check stream dependency
		// Deleting Stream Dependency field
		fr.payload = fr.payload[4:]
		fr.Length -= 4

		fr.payload = fr.payload[1:]
		fr.Length--
		// TODO: Check flag priority
	}
	// TODO: prepare to hpack or hpack here
	ctx.Request.Header.rawHeaders = append(
		ctx.Request.Header.rawHeaders[:0], fr.payload...,
	)
	if fr.Has(FlagEndHeaders) {
		_, err = ctx.Request.Header.parseRawHeaders()
		if ctx.Request.Header.IsPost() {
			nextFrame = FrameData // can be data or not
		} else {
			nextFrame = 0xff // execute handler
		}
	}
	if fr.Has(FlagEndStream) && err == nil {
		if ctx.state == h2open {
			ctx.state = h2half_closed
		}
		_, err = ctx.Request.Header.parseRawHeaders()
	}
	return nextFrame, err
}

func parseFramePushPromise(fr *Frame, ctx *RequestCtx) (nextFrame uint8, err error) {
	if ctx.state != h2open && ctx.state != h2half_closed {
		// TODO: send error
		return 0xff, err
	}
	nextFrame = FrameContinuation
	if fr.Has(FlagPadded) {
		fr.Length -= uint32(fr.payload[0])
		fr.payload = fr.payload[1:fr.Length]
		fr.Length--
	}
	// TODO
	stream := bytesToUint32(fr.payload[:4])
	_ = stream
	ctx.Request.Header.rawHeaders = append(
		ctx.Request.Header.rawHeaders[:0], fr.payload...,
	)
	if fr.Has(FlagEndHeaders) {
		_, err = ctx.Request.Header.parse(ctx.Request.Header.rawHeaders)
		if ctx.IsPost() {
			nextFrame = FrameData
		} else {
			nextFrame = 0xff
		}
	}
	return nextFrame, err
}

func parseFramePing(fr *Frame, ctx *RequestCtx) {
	fr.Add(FlagAck)
	//writeFrame(fr, ctx)
}

func parseFrameGoAway(fr *Frame, ctx *RequestCtx) error {
	//stream := bytesToUint32(fr.payload[:4])
	code := bytesToUint32(fr.payload[4:8])
	debug := fr.payload[8:]
	if debug != nil {
		return fmt.Errorf("%s: (%s)", errParser[code], debug)
	}
	return errParser[code]
}

func parseFrameContinuation(fr *Frame, ctx *RequestCtx) (nextFrame uint8, err error) {
	// If the END_HEADERS bit is not set, this frame MUST be followed
	// by another CONTINUATION frame
	nextFrame = FrameContinuation
	ctx.Request.Header.rawHeaders = append(
		ctx.Request.Header.rawHeaders, fr.payload...,
	)
	if fr.Has(FlagEndHeaders) {
		_, err = ctx.Request.Header.parse(ctx.Request.Header.rawHeaders)
		if ctx.IsPost() {
			nextFrame = FrameData
		} else {
			nextFrame = 0xff
		}
	}
	return nextFrame, err
}

func writeSettings(st *Settings, bw io.Writer) (err error) {
	fr := AcquireFrame()
	defer ReleaseFrame(fr)

	fr.Set(FrameSettings)
	if st == nil {
		fr.Add(FlagAck)
	} else {
		st.Encode()
		fr.SetPayload(st.rawSettings)
	}
	_, err = fr.WriteTo(bw)
	return err
}
