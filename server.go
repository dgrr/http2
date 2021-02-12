package http2

import (
	"net"

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

	for err == nil {
		_, err = fr.ReadFrom(c) // TODO: Use ReadFromLimitPayload?
		if err != nil {
			break
		}

		switch fr.Type() {
		case FrameHeaders, FrameContinuation:
			println("headers or continuation")
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
