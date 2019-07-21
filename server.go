package http2

import (
	"crypto/tls"
	"errors"
	"log"
	"net"
	"sync"
)

type RequestHandler func(*Ctx)

// Server ...
type Server struct {
	Handler RequestHandler

	ctxPool sync.Pool
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
			switch cTLS.ConnectionState().NegotiatedProtocol {
			case H2TLSProto:
			default:
				err = errUpgrade
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
			ctx.SetStream(sStreamID)

			// Adding the context to the stream list
			streams[fr.Stream()] = ctx
		}

		switch fr.Type() {
		case FrameData:
			println("data")
			ctx.Request.body.Write(
				peekDataPayload(fr.Payload()),
			)
		case FrameHeaders:
			println("headers")
			ctx.Request.Header.rawHeaders = append(
				ctx.Request.Header.rawHeaders, peekHeadersPayload(fr)...,
			)
			// headers will be parsed in the handler
			if shouldServe = fr.Has(FlagEndHeaders) && fr.Has(FlagEndStream); shouldServe {
				ctx.Request.Header.parseRawHeaders()
			}
		case FramePriority:
			println("priority")
		case FrameResetStream:
			println("reset")
		case FrameSettings:
			println("settings")
			// TODO: Check if the client's settings fit the server ones
			// reading settings frame
			err = userSettings.ReadFrame(fr)
			if err == nil {
				userSettings.ack = true
				err = writeSettings(c, sStreamID, &userSettings)
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
	}

	return
}
