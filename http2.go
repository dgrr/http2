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

// Byteorder must be big endian
// Values are unsigned unless otherwise indicated

const (
	h2TLSProto = "h2"
	h2Clean    = "h2c"
)

var (
	// http://httpwg.org/specs/rfc7540.html#ConnectionHeader
	http2Preface = []byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")
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

// Only TLS Upgrade
/*
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
*/

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
