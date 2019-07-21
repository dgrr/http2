// +build fasthttp

package http2

import (
	"crypto/tls"
	"errors"
	"log"
	"net"
	"sync"

	"github.com/valyala/fasthttp"
)

type RequestHandler func(*Ctx)

// Server ...
type Server struct {
	s *fasthttp.Server

	ctxPool sync.Pool
}

func (s *Server) ConfigureServer(ss *fasthttp.Server) {
	s.s = ss
	ss.NextProto(H2TLSProto, s.serveConn)
}

// ListenAndServeTLS ...
func (s *Server) ListenAndServeTLS(addr, certFile, keyFile string) error {
	tlsConfig, err := acquireTLSConfig(certFile, keyFile)
	if err != nil {
		return err
	}

	ln, err := tls.Listen("tcp4", addr, tlsConfig)
	if err == nil {
		err = s.Serve(ln)
	}

	return err
}

func acquireTLSConfig(certFile, keyFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{
			cert,
		},
		NextProtos: []string{
			H2TLSProto,
		},
	}
	tlsConfig.BuildNameToCertificate()

	return tlsConfig, nil
}

// Serve ...
func (s *Server) Serve(ln net.Listener) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			// TODO: Handle
			break
		}

		if cTLS, ok := c.(connTLSer); ok {
			err = cTLS.Handshake()
			if err == nil {
				switch cTLS.ConnectionState().NegotiatedProtocol {
				case H2TLSProto:
				default:
					err = errUpgrade
				}
			}
		}

		if err != nil {
			log.Printf("error serving conn: %s\n", err)
			continue
		}

		go s.serveConn(c)
	}

	return nil
}

var errUpgrade = errors.New("error upgrading the connection")

func (s *Server) acquireCtx(c net.Conn) (ctx *Ctx) {
	ctxR := s.ctxPool.Get()
	if ctxR == nil {
		ctx = &Ctx{}
	} else {
		ctx = ctxR.(*Ctx)
	}
	ctx.c = c
	return ctx
}

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
		if cStreamID == 0x0 {
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
		case FrameHeaders:
			println("headers")
			err = ctx.Request.Header.Read(fr)
			shouldHandle = err == nil && (ctx.Request.Header.IsGet() || ctx.Request.Header.IsHead())
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
		case FrameContinuation:
			println("continuation")
		}

		if shouldHandle {
			rctx := translateFromCtx(ctx)
			s.s.Handler(rctx)
			translateToCtx(ctx, rctx)
			err = ctx.writeResponse()
		}
	}

	return
}
