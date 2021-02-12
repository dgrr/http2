package http2

import (
	"net"
	"sync"

	"github.com/valyala/fasthttp"
)

func nextStreamID(current, clientLast uint32) uint32 {
	n := uint32(1)
	for current < clientLast {
		if current&1 == 0 {
			n = 2
		}
		current += n
	}
	return current
}

// ConfigureServer configures the fasthttp's server to handle
// HTTP/2 connections. The HTTP/2 connection can be only
// established if the fasthttp server is using TLS.
//
// Future implementations may support HTTP/2 through plain TCP.
func ConfigureServer(s *fasthttp.Server) *Server {
	s2 := &Server{
		s: s,
	}
	s.NextProto(H2TLSProto, s2.serveConn)
	return s2
}

var streamPool = sync.Pool{
	New: func() interface{} {
		return &Stream{
			ctx: &fasthttp.RequestCtx{},
		}
	},
}

// Server ...
type Server struct {
	s *fasthttp.Server
}

// serveConn ...
func (s *Server) serveConn(c net.Conn) error {
	if !ReadPreface(c) {
		return ErrBadPreface
	}

	fr := AcquireFrame()
	defer ReleaseFrame(fr)

	// prepare to send the empty settings frame
	err := (&Settings{}).WriteFrame(fr)
	if err == nil {
		_, err = fr.WriteTo(c)
	}

	streams := make(map[uint32]*Stream)

	for err == nil {
		// TODO: works without reseting? fr.Reset()

		_, err = fr.ReadFrom(c) // TODO: Use ReadFromLimitPayload?
		if err != nil {
			break
		}

		if fr.stream == 0 {
			// blah blah blah
			continue
		}

		strm, ok := streams[fr.stream]
		if !ok {
			strm = acquireStream()
			streams[fr.stream] = strm
		}

		switch fr.Type() {
		case FrameHeaders:
			ok, err = strm.ProcessHeaders(fr)
			if err == nil && ok {
				s.s.Handler(strm.ctx)
			}
		case FrameContinuation:
			println("continuation")
		case FrameData:
			println("data")
		case FramePriority:
			println("priority")
			// TODO: If a PRIORITY frame is received with a stream identifier of 0x0, the recipient MUST respond with a connection error
		case FrameResetStream:
			println("reset")
		case FrameSettings:
			println("settings")
			// TODO: Check if the client's settings fit the server ones
		case FramePushPromise:
			println("pp")
		case FramePing:
			println("ping")
		case FrameGoAway:
			println("away")
		case FrameWindowUpdate:
			println("update")
		}
	}

	return err
}

type State int8

const (
	StateIdle State = iota
	StateReserved
	StateOpen
	StateHalfClosed
	StateClosed
)

type Stream struct {
	state State
	ctx   *fasthttp.RequestCtx

	hp  *HPACK
	hfr *Headers
}

func acquireStream() *Stream {
	strm := streamPool.Get().(*Stream)
	strm.state = StateIdle
	return strm
}

func releaseStream(strm *Stream) {
	if strm.hp != nil {
		ReleaseHPACK(strm.hp)
		strm.hp = nil
	}
	if strm.hfr != nil {
		ReleaseHeaders(strm.hfr)
		strm.hfr = nil
	}

	streamPool.Put(strm)
}

func (strm *Stream) ProcessHeaders(fr *Frame) (bool, error) {
	if strm.hfr == nil {
		strm.hfr = AcquireHeaders()
	}

	err := strm.hfr.ReadFrame(fr)
	if err != nil {
		return false, err
	}

	if strm.hfr.EndHeaders() {
		if strm.hp == nil {
			strm.hp = AcquireHPACK()
		}

		b := strm.hfr.rawHeaders
		for len(b) > 0 {
			hf := AcquireHeaderField()
			b, err = strm.hp.Next(hf, b)
			if err != nil {
				break
			}
			strm.hp.AddField(hf)
		}

		fasthttpRequestHeaders(strm.hp, &strm.ctx.Request)
	}

	return strm.hfr.EndHeaders(), err
}
