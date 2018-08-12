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
