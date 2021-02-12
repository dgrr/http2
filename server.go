package http2

import (
	"crypto/tls"
	"errors"
	"github.com/valyala/fasthttp"
	"log"
	"net"
	"sync"
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
}


// Server ...
type Server struct {
	s *fasthttp.Server
}

// serveConn ...
func (s *Server) serveConn(c net.Conn) (err error) {
	if !ReadPreface(c) {
		return ErrBadPreface
	}

	var (
		cLastStreamID uint32 = 0
		cStreamID     uint32 = 0
		sStreamID     uint32 = 2
	)

	fr := AcquireFrame()
	defer ReleaseFrame(fr)

	// prepare to send the empty settings frame
	err = (&Settings{}).WriteFrame(fr)
	if err == nil {
		_, err = fr.WriteTo(c)
	}

	var (
		userSettings Settings
		ok           bool
		ctx          *Ctx
		hp           = AcquireHPACK()
		streams      = make(map[uint32]*Ctx)
	)
	defer ReleaseHPACK(hp)

	for err == nil {
		fr.Reset()
		shouldHandle := false
		cLastStreamID = cStreamID

		_, err = fr.ReadFrom(c) // TODO: Use ReadFromLimitPayload?
		if err != nil {
			break
		}

		cStreamID = fr.Stream()
		if cStreamID == 0 {
			println("control message")
			// TODO: stuff...
			continue
		}

		if cStreamID < cLastStreamID {
			println("lower")
			// TODO: Handle ...
		}

		if cStreamID&1 == 0 {
			panic("cannot be even")
			// TODO: do not use a panic
		}
		sStreamID = nextStreamID(sStreamID, cStreamID)

		// TODO: Add states for the contexts
		ctx, ok = streams[cStreamID]
		if !ok {
			ctx = s.acquireCtx(c)
			ctx.SetHPACK(hp)
			ctx.SetStream(cStreamID)

			// Adding the context to the stream list
			streams[cStreamID] = ctx
		}

		switch fr.Type() {
		case FrameHeaders, FrameContinuation:
			println("headers or continuation")
			err = ctx.Request.Header.Read(fr)
			shouldHandle = ctx.Request.Header.parsed &&
				(ctx.Request.Header.IsGet() || ctx.Request.Header.IsHead())
		case FrameData:
			println("data")
			err = ctx.Request.Read(fr)
			shouldHandle = err == nil && ctx.Request.Header.parsed
		case FramePriority:
			println("priority")
			p := AcquirePriority()
			p.ReadFrame(fr)
			ReleasePriority(p)
			// TODO: If a PRIORITY frame is received with a stream identifier of 0x0, the recipient MUST respond with a connection error
		case FrameResetStream:
			println("reset")
		case FrameSettings:
			println("settings")
			// TODO: Check if the client's settings fit the server ones
			// reading settings frame
			err = userSettings.ReadFrame(fr)
			if err == nil {
			}
		case FramePushPromise:
			println("pp")
		case FramePing:
			println("ping")
		case FrameGoAway:
			println("away")
		case FrameWindowUpdate:
			println("update")
		}

		if shouldHandle {
			s.s.Handler(ctx)
			err = ctx.writeResponse()
		}
	}

	return
}
