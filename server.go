package http2

import (
	"fmt"
	"log"
	"net"
	"sync"

	"github.com/valyala/fasthttp"
)

func serverNextStreamID(current, clientLast uint32) uint32 {
	for current < clientLast {
		if current&1 == 0 {
			current++
		}
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
	if len(s.Name) != 0 {
		s.Name = "fasthttp using http2"
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

// connCtx is an object intended to handle the input connection
// regardless the stream.
type connCtx struct {
	c  net.Conn
	hp *HPACK
	fr *Frame // read frame
	st *Settings
}

var connCtxPool = sync.Pool{
	New: func() interface{} {
		return &connCtx{}
	},
}

func acquireConnCtx(c net.Conn) *connCtx {
	ctx := connCtxPool.Get().(*connCtx)
	ctx.c = c
	ctx.hp = AcquireHPACK()
	ctx.fr = AcquireFrame()
	ctx.st = AcquireSettings()
	return ctx
}

func releaseConnCtx(ctx *connCtx) {
	ReleaseHPACK(ctx.hp)
	ReleaseFrame(ctx.fr)
	ReleaseSettings(ctx.st)
}

func (ctx *connCtx) Write(b []byte) (int, error) {
	return ctx.c.Write(b)
}

func (ctx *connCtx) writeFrame() error {
	_, err := ctx.fr.WriteTo(ctx.c)
	return err
}

func (ctx *connCtx) readFrame() error {
	_, err := ctx.fr.ReadFrom(ctx.c)
	return err
}

func (ctx *connCtx) Read(b []byte) (int, error) {
	return ctx.Read(b)
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

	ctx := acquireConnCtx(c)
	defer releaseConnCtx(ctx)

	// prepare to send the empty settings frame
	err := ctx.st.WriteFrame(ctx.fr)
	if err == nil {
		err = ctx.writeFrame()
	}

	if err == nil {
		ctx.fr.Reset()
		(&WindowUpdate{increment: 1 << 14}).WriteFrame(ctx.fr)
		err = ctx.writeFrame()
	}
	if err != nil {
		return err
	}

	streams := make(map[uint32]*Stream)

	for err == nil {
		// TODO: works without reseting? fr.Reset()
		err = ctx.readFrame()
		if err != nil {
			break
		}

		strm := streams[ctx.fr.stream]
		if strm == nil {
			strm = acquireStream(s.s.Name, ctx.fr.stream)
			streams[ctx.fr.stream] = strm
		}
		if strm.IsClosed() {
			log.Println("error stream has been closed before...")
			continue
		}

		err = s.Handle(ctx, strm)

		if strm.IsClosed() {
			releaseStream(strm)
			// delete(streams, strm.id)
		}

		// currentID = serverNextStreamID(currentID, ctx.fr.stream)
	}

	return err
}

type HTTP2ProtoError struct {
	Code   int
	Reason string
}

func NewHTTP2ProtoError(code int, reason string) *HTTP2ProtoError {
	return &HTTP2ProtoError{
		Code:   code,
		Reason: reason,
	}
}

func (pe *HTTP2ProtoError) Error() string {
	return fmt.Sprintf("%d: %s", pe.Code, pe.Reason)
}

func (s *Server) Handle(ctx *connCtx, strm *Stream) (err error) {
	fr := ctx.fr

	switch ctx.fr.Type() {
	case FrameHeaders:
		println("headers")
		err = s.handleHeaders(ctx, strm)
	case FrameContinuation:
		println("continuation")
	case FrameData:
		println("data")
	case FramePriority:
		println("priority")
		// TODO: If a PRIORITY frame is received with a stream identifier of 0x0, the recipient MUST respond with a connection error
	case FrameResetStream:
		println("reset")
		err = s.handleReset(ctx, strm)
	case FrameSettings:
		println("settings")
		s.handleSettings(ctx, strm)
		// TODO: Check if the client's settings fit the server ones
	case FramePushPromise:
		println("pp")
	case FramePing:
		println("ping")
	case FrameGoAway:
		println("away")
		aw := &GoAway{}
		aw.ReadFrame(fr)
		println(aw.stream)
		println(aw.code)
	case FrameWindowUpdate:
		println("update")
	}

	if ctx.fr.HasFlag(FlagEndStream) {
		switch strm.state {
		case StateOpen:
			strm.state = StateHalfClosed
		case StateHalfClosed:
			strm.state = StateClosed
		}
	}

	return err
}

type StreamState int8

const (
	StateIdle StreamState = iota
	StateReserved
	StateOpen
	StateHalfClosed
	StateClosed
)

type Stream struct {
	id    uint32
	state StreamState
	ctx   *fasthttp.RequestCtx

	hfr *Headers
}

func acquireStream(serverName string, id uint32) *Stream {
	strm := streamPool.Get().(*Stream)
	strm.ctx.Request.Reset()
	strm.ctx.Response.Reset()
	strm.ctx.Response.Header.SetServer(serverName)

	strm.state = StateIdle
	strm.id = id

	return strm
}

func releaseStream(strm *Stream) {
	if strm.hfr != nil {
		ReleaseHeaders(strm.hfr)
		strm.hfr = nil
	}

	streamPool.Put(strm)
}

func (strm *Stream) State() StreamState {
	return strm.state
}

func (strm *Stream) IsClosed() bool {
	return strm.state == StateClosed
}

func (s *Server) handleHeaders(ctx *connCtx, strm *Stream) error {
	switch strm.state {
	case StateIdle:
		strm.state = StateOpen
	case StateReserved:
		strm.state = StateHalfClosed
	}

	if strm.hfr == nil {
		strm.hfr = AcquireHeaders()
	}

	// Read data from ctx.fr to Headers
	err := strm.hfr.ReadFrame(ctx.fr)
	if err != nil {
		return err
	}

	if strm.hfr.EndHeaders() {
		b := strm.hfr.rawHeaders
		for len(b) > 0 {
			hf := AcquireHeaderField()
			b, err = ctx.hp.Next(hf, b)
			if err != nil {
				break
			}
			ctx.hp.AddField(hf)
		}

		if err == nil {
			fasthttpRequestHeaders(ctx.hp, &strm.ctx.Request)

			// TODO: check if HEAD or GET
			s.s.Handler(strm.ctx)
			err = s.tryReply(ctx, strm)
		}
	}

	return err
}

func (s *Server) handleReset(ctx *connCtx, strm *Stream) error {
	// fr := AcquireRstStream()
	// defer ReleaseRstStream(fr)

	// TODO: handle error codes
	// err := fr.ReadFrame(ctx.fr)
	// if err != nil {
	// 	return err
	// }

	strm.state = StateClosed
	return nil
}

func (s *Server) handleSettings(ctx *connCtx, strm *Stream) (err error) {
	if ctx.fr.Len() == 0 {
		return ctx.writeFrame()
	}

	st := AcquireSettings()
	defer ReleaseSettings(st)
	st.ReadFrame(ctx.fr)

	if !ctx.st.IsAck() {
		ctx.st.SetAck(false)
		if st.HeaderTableSize() < ctx.st.HeaderTableSize() {
			ctx.st.SetHeaderTableSize(st.HeaderTableSize())
		}
		if st.MaxConcurrentStreams() > ctx.st.MaxConcurrentStreams() {
			ctx.st.SetMaxConcurrentStreams(st.MaxConcurrentStreams())
		}
		if st.MaxWindowSize() < ctx.st.MaxWindowSize() {
			ctx.st.SetMaxWindowSize(st.MaxWindowSize())
		}
		if st.MaxFrameSize() < ctx.st.MaxFrameSize() {
			ctx.st.SetMaxFrameSize(st.MaxFrameSize())
		}
		if st.MaxHeaderListSize() < ctx.st.MaxHeaderListSize() {
			ctx.st.SetMaxHeaderListSize(st.MaxHeaderListSize())
		}
		if !st.Push() {
			ctx.st.SetPush(false)
		}

		ctx.st.WriteFrame(ctx.fr)
		err = ctx.writeFrame()
	}

	return
}

func (s *Server) tryReply(ctx *connCtx, strm *Stream) error {
	dfr := AcquireData()
	defer ReleaseData(dfr)

	hfr := AcquireHeaders()
	defer ReleaseHeaders(hfr)

	err := ctx.writeHeaders(strm, hfr)
	if err == nil {
		err = ctx.writeData(strm, dfr)
	}

	return err
}

func (ctx *connCtx) writeHeaders(strm *Stream, hfr *Headers) error {
	fr := AcquireFrame()
	defer ReleaseFrame(fr)

	fr.SetStream(strm.id)

	ctx.hp.releaseFields()

	fasthttpResponseHeaders(ctx.hp, &strm.ctx.Response)

	hfr.rawHeaders = ctx.hp.MarshalTo(hfr.rawHeaders[:0])
	hfr.SetEndHeaders(true)

	hfr.WriteFrame(fr)
	_, err := fr.WriteTo(ctx.c)
	return err
}

func (ctx *connCtx) writeData(strm *Stream, dfr *Data) error {
	fr := AcquireFrame()
	defer ReleaseFrame(fr)

	fr.SetStream(strm.id)

	dfr.SetData(strm.ctx.Response.Body())
	dfr.SetPadding(false)
	dfr.SetEndStream(true)
	dfr.WriteFrame(fr)

	_, err := fr.WriteTo(ctx.c)
	return err
}
