package http2

import (
	"fmt"
	"io"
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

	windowSize uint32 // client's window size
	unackedSettings int
	lastStreamOpen  uint32
	streamsOpen     int
	isClosing       bool // after recv goaway
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

func (ctx *connCtx) rewriteFrame() error {
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

func (ctx *connCtx) writeFrame(fr *Frame) error {
	_, err := fr.WriteTo(ctx.c)
	return err
}

// Server ...
//
// TODO: Shared windowSize
type Server struct {
	s *fasthttp.Server
}

// serveConn ...
func (s *Server) serveConn(c net.Conn) error {
	defer c.Close()

	if !ReadPreface(c) {
		return ErrBadPreface
	}

	ctx := acquireConnCtx(c)
	defer releaseConnCtx(ctx)

	var err error
	streams := make(map[uint32]*Stream)

	for err == nil {
		// TODO: works without reseting? fr.Reset()
		err = ctx.readFrame()
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			break
		}

		// wrong stream id
		if ctx.fr.stream&1 == 0 {
			// TODO: Handle
		}

		strm := streams[ctx.fr.stream]
		if strm == nil {
			strm = acquireStream(ctx.fr.stream)
			streams[ctx.fr.stream] = strm
			ctx.lastStreamOpen = strm.id
			ctx.streamsOpen++
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

		if ctx.isClosing && ctx.streamsOpen == 0 {
			break
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
	switch ctx.fr.Type() {
	case FrameHeaders:
		err = s.handleHeaders(ctx, strm)
	case FrameContinuation:
		err = s.handleContinuation(ctx, strm)
	case FrameData:
		err = s.handleData(ctx, strm)
	case FramePriority:
		println("priority")
		// TODO: If a PRIORITY frame is received with a stream identifier of 0x0, the recipient MUST respond with a connection error
	case FrameResetStream:
		err = s.handleReset(ctx, strm)
	case FrameSettings:
		err = s.handleSettings(ctx, strm)
	case FramePushPromise:
		println("pp")
	case FramePing:
		ctx.fr.AddFlag(FlagAck)
		err = ctx.rewriteFrame()
	case FrameGoAway:
		err = s.handleGoAway(ctx, strm)
	case FrameWindowUpdate:
		s.handleWindowUpdate(ctx, strm)
	}

	if ctx.fr.HasFlag(FlagEndStream) {
		switch strm.state {
		case StateOpen:
			strm.state = StateHalfClosed
		case StateHalfClosed:
			strm.state = StateClosed
			ctx.streamsOpen--
		}
	}

	if err == nil && strm.istate == stateExecHandler {
		s.s.Handler(strm.ctx)
		err = s.tryReply(ctx, strm)
		strm.istate = stateNone
	}

	return err
}

type StreamState int8

func (s StreamState) String() string {
	switch s {
	case StateIdle:
		return "Idle"
	case StateReserved:
		return "Reserved"
	case StateOpen:
		return "Open"
	case StateHalfClosed:
		return "HalfClosed"
	case StateClosed:
		return "Closed"
	}

	return "IDK"
}

const (
	StateIdle StreamState = iota
	StateReserved
	StateOpen
	StateHalfClosed
	StateClosed
)

type internalState int8

const (
	stateNone internalState = iota
	stateAwaitData
	stateExecHandler
	stateAfterPushPromise
)

type Stream struct {
	id     uint32
	state  StreamState
	istate internalState
	ctx    *fasthttp.RequestCtx

	hfr *Headers
}

func acquireStream(id uint32) *Stream {
	strm := streamPool.Get().(*Stream)
	strm.ctx.Request.Reset()
	strm.ctx.Response.Reset()

	strm.state = StateIdle
	strm.istate = stateNone
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
		err = s.parseHeaders(ctx, strm)
	}

	return err
}

func (s *Server) handleContinuation(ctx *connCtx, strm *Stream) (err error) {
	fr := AcquireContinuation()
	defer ReleaseContinuation(fr)

	fr.ReadFrame(ctx.fr)
	if fr.HasEndHeaders() {
		err = s.parseHeaders(ctx, strm)
	}

	return
}

func (s *Server) parseHeaders(ctx *connCtx, strm *Stream) (err error) {
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

		if strm.ctx.Request.Header.IsGet() ||
			strm.ctx.Request.Header.IsHead() {
			strm.istate = stateExecHandler
		} else { // post, put or delete
			strm.istate = stateAwaitData
		}
	}

	return
}

func (s *Server) handleData(ctx *connCtx, strm *Stream) error {
	dfr := AcquireData()
	defer ReleaseData(dfr)

	dfr.ReadFrame(ctx.fr)
	strm.ctx.Request.SetBody(dfr.Data())

	strm.istate = stateExecHandler

	return nil
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
	ctx.streamsOpen--

	return nil
}

func (s *Server) handleGoAway(ctx *connCtx, strm *Stream) error {
	fr := AcquireFrame()
	defer ReleaseFrame(fr)

	ga := AcquireGoAway()
	defer ReleaseGoAway(ga)

	ctx.isClosing = true

	ga.SetStream(ctx.lastStreamOpen)
	// TODO: Replace with proper code
	ga.SetCode(0x0)

	fr.SetStream(strm.id)

	ga.WriteFrame(fr)
	return ctx.writeFrame(fr)
}

func (s *Server) handleWindowUpdate(ctx *connCtx, strm *Stream) error {
	wu := AcquireWindowUpdate()
	defer ReleaseWindowUpdate(wu)

	wu.ReadFrame(ctx.fr)
	ctx.windowSize += wu.increment

	return nil
}

func (s *Server) handleSettings(ctx *connCtx, strm *Stream) error {
	st := AcquireSettings()
	defer ReleaseSettings(st)

	st.ReadFrame(ctx.fr)

	if st.IsAck() {
		ctx.unackedSettings--
		if ctx.unackedSettings < 0 {
			ctx.unackedSettings = 0
		}
		return nil
	}

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

	ctx.windowSize = st.MaxWindowSize()

	fr := AcquireFrame()
	defer ReleaseFrame(fr)

	ctx.st.WriteFrame(fr)
	err := ctx.writeFrame(fr)
	if err == nil {
		fr.Reset()
		fr.SetType(FrameSettings)
		fr.AddFlag(FlagAck)
		err = ctx.writeFrame(fr)
	}

	return err
}

func (s *Server) tryReply(ctx *connCtx, strm *Stream) error {
	dfr := AcquireData()
	defer ReleaseData(dfr)

	hfr := AcquireHeaders()
	defer ReleaseHeaders(hfr)

	// if n := len(strm.ctx.Response.Body()) - int(ctx.st.windowSize); n > 0 {
	// 	if err := ctx.writeWindowUpdate(strm, uint32(n+1)); err != nil {
	// 		return err
	// 	}
	// }

	err := ctx.writeHeaders(strm, hfr)
	if err == nil {
		err = ctx.writeData(strm, dfr)
	}

	return err
}

func (ctx *connCtx) writeWindowUpdate(strm *Stream, n uint32) error {
	fr := AcquireFrame()
	defer ReleaseFrame(fr)

	wu := AcquireWindowUpdate()
	defer ReleaseWindowUpdate(wu)

	wu.SetIncrement(n)
	wu.WriteFrame(fr)
	ctx.st.SetMaxWindowSize(ctx.st.MaxWindowSize() + wu.increment)

	fr.SetStream(strm.id)
	_, err := fr.WriteTo(ctx.c)

	return err
}

func (ctx *connCtx) writeReset(strm *Stream) error {
	rfr := AcquireRstStream()
	defer ReleaseRstStream(rfr)

	fr := AcquireFrame()
	defer ReleaseFrame(fr)

	fr.SetStream(strm.id)

	// TODO: Replace with proper code
	rfr.SetCode(0x0)
	rfr.WriteFrame(fr)

	_, err := fr.WriteTo(ctx.c)
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

	body := strm.ctx.Response.Body()

	fr.SetStream(strm.id)
	fr.SetMaxLen(uint32(len(body)))

	dfr.SetData(body)
	dfr.SetPadding(false)
	dfr.SetEndStream(true)
	dfr.WriteFrame(fr)

	_, err := fr.WriteTo(ctx.c)
	if err == nil && strm.state == StateHalfClosed {
		strm.state = StateClosed
	}

	return err
}
