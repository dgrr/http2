package http2

import (
	"bufio"
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
		return &ServerStream{
			ctx: &fasthttp.RequestCtx{},
		}
	},
}

// connCtx is an object intended to handle the input connection
// regardless the stream.
type connCtx struct {
	c  net.Conn
	br *bufio.Reader
	bw *bufio.Writer

	hp *HPACK

	st *Settings

	cst        *Settings
	windowSize uint32

	unackedSettings int
	lastStreamOpen  uint32
	streamsOpen     int
	isClosing       bool // after recv goaway

	strms []*ServerStream
}

var connCtxPool = sync.Pool{
	New: func() interface{} {
		return &connCtx{}
	},
}

func acquireConnCtx(c net.Conn) *connCtx {
	ctx := connCtxPool.Get().(*connCtx)
	ctx.c = c
	ctx.br = bufio.NewReader(c)
	ctx.bw = bufio.NewWriter(c)
	ctx.hp = AcquireHPACK()
	ctx.st = AcquireSettings()
	ctx.strms = make([]*ServerStream, 0, 128)
	return ctx
}

func releaseConnCtx(ctx *connCtx) {
	ReleaseHPACK(ctx.hp)
	ReleaseSettings(ctx.st)
}

func (ctx *connCtx) writeFrame(fr *Frame) error {
	_, err := fr.WriteTo(ctx.bw)
	if err == nil {
		err = ctx.bw.Flush()
	}
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
	streams := make(map[uint32]*ServerStream)

	fr := AcquireFrame()

	// write own settings
	ctx.st.WriteFrame(fr)
	err = ctx.writeFrame(fr)
	if err == nil {
		err = ctx.writeWindowUpdate(nil, ctx.st.windowSize-65535)
	}

	ReleaseFrame(fr)

	var (
		wch       = make(chan *Frame, 1024)
		cementery = make(chan *ServerStream, 1)
	)

	go s.writeLoop(ctx, wch, cementery)

	for err == nil {
		fr := AcquireFrame()

		_, err = fr.ReadFrom(ctx.br)
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			break
		}

		switch fr.Type() {
		case FrameGoAway:
			// TODO:
		case FrameWindowUpdate:
			ctx.handleWindowUpdate(fr)
		case FrameSettings:
			ctx.handleSettings(fr)
		default:
			strm := streams[fr.Stream()]
			if strm == nil {
				strm = acquireStream(fr.Stream(), ctx, wch)
				streams[fr.Stream()] = strm
				strm.windowSize = ctx.windowSize
				ctx.lastStreamOpen = strm.id
				ctx.streamsOpen++
				// TODO: Use a pool
				go strm.loop()
			}

			if strm.IsClosed() {
				log.Printf(
					"recv %s after close\n", fr.Type())
				continue
			}

			strm.rch <- fr
		}

		// err = s.Handle(ctx, strm)
		// if strm.IsClosed() {
		// 	ctx.streamsOpen--
		// 	releaseStream(strm)
		// 	// delete(streams, strm.id)
		// }
		//
		// if ctx.isClosing && ctx.streamsOpen == 0 {
		// 	break
		// }

		// currentID = serverNextStreamID(currentID, ctx.fr.stream)
	}

	return err
}

func (s *Server) writeLoop(ctx *connCtx, wch <-chan *Frame, cementery chan *ServerStream) {
	for {
		select {
		case fr, ok := <-wch:
			if !ok {
				return
			}

			err := ctx.writeFrame(fr)
			if err != nil {
				// TODO: handle xd
				log.Println(err)
			}
		case strm, ok := <-cementery:
			if !ok {
				return
			}
			releaseStream(strm)
		}
	}
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

func (strm *ServerStream) handleFrame(fr *Frame) (err error) {
	switch fr.Type() {
	case FrameHeaders:
		err = strm.handleHeaders(fr)
	case FrameContinuation:
		err = strm.handleContinuation(fr)
	case FrameData:
		err = strm.handleData(fr)
	case FramePriority:
		println("priority")
		// TODO: If a PRIORITY frame is received with a stream identifier of 0x0, the recipient MUST respond with a connection error
	case FrameResetStream:
		err = strm.handleReset(fr)
	case FramePushPromise:
		println("pp")
	case FramePing:
		fr.AddFlag(FlagAck)
		strm.writeFrame(fr)
	case FrameWindowUpdate:
		strm.handleWindowUpdate(fr)
	}

	if fr.HasFlag(FlagEndStream) {
		switch strm.state {
		case StateOpen:
			strm.state = StateHalfClosed
		case StateHalfClosed:
			strm.state = StateClosed
			// TODO: ctx.streamsOpen--
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

type internalState int8

const (
	stateNone internalState = iota
	stateAwaitData
	stateExecHandler
	stateAfterPushPromise
)

type ServerStream struct {
	id         uint32
	state      StreamState
	istate     internalState
	windowSize uint32

	ctx     *connCtx
	fastCtx *fasthttp.RequestCtx

	// chan to write
	wch chan<- *Frame
	// chan to read
	rch chan *Frame

	// when the stream dies, it goes there
	cementery chan<- *ServerStream

	hfr *Headers
}

func acquireStream(id uint32, ctx *connCtx, wch chan<- *Frame) *ServerStream {
	strm := streamPool.Get().(*ServerStream)
	strm.ctx.Request.Reset()
	strm.ctx.Response.Reset()

	strm.state = StateIdle
	strm.istate = stateNone
	strm.id = id
	strm.ctx = ctx
	strm.wch = wch
	strm.rch = make(chan *Frame, 128)

	return strm
}

func releaseStream(strm *ServerStream) {
	if strm.hfr != nil {
		ReleaseHeaders(strm.hfr)
		strm.hfr = nil
	}

	streamPool.Put(strm)
}

func (strm *ServerStream) State() StreamState {
	return strm.state
}

func (strm *ServerStream) IsClosed() bool {
	return strm.state == StateClosed
}

func (strm *ServerStream) writeFrame(fr *Frame) {
	strm.wch <- fr
}

func (strm *ServerStream) loop() {
	for {
		select {
		case fr, ok := <-strm.rch:
			if !ok {
				return
			}

			err := strm.handleFrame(fr)
			if err == nil && strm.istate == stateExecHandler {
				s.s.Handler(strm.ctx)
				err = s.tryReply(ctx, strm)
				strm.istate = stateNone
			}
		}
	}
}

func (strm *ServerStream) handleHeaders(fr *Frame) error {
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
	err := strm.hfr.ReadFrame(fr)
	if err != nil {
		return err
	}

	return strm.parseHeaders()
}

func (strm *ServerStream) handleContinuation(fr *Frame) (err error) {
	cfr := AcquireContinuation()
	defer ReleaseContinuation(cfr)

	cfr.ReadFrame(fr)
	err = strm.parseHeaders()

	return
}

func (strm *ServerStream) parseHeaders() (err error) {
	hf := AcquireHeaderField()
	b := strm.hfr.rawHeaders

	for len(b) > 0 {
		b, err = strm.hp.Next(hf, b)
		if err != nil {
			break
		}

		fasthttpRequestHeaders(hf, &strm.ctx.Request)
	}

	if err == nil {
		if strm.hfr.EndStream() {
			strm.istate = stateExecHandler
		} else {
			strm.istate = stateAwaitData
		}
		strm.ctx.Request.SetRequestURIBytes(
			strm.ctx.Request.URI().FullURI())
		strm.ctx.Request.Header.SetProtocolBytes(strHTTP2)
	}

	return
}

func (strm *ServerStream) handleData(fr *Frame) error {
	dfr := AcquireData()
	defer ReleaseData(dfr)

	dfr.ReadFrame(fr)
	if dfr.Len() > 0 {
		strm.ctx.Request.AppendBody(dfr.Data())
		strm.writeWindowUpdate(uint32(len(strm.ctx.Request.Body())))
	}
	if dfr.EndStream() {
		strm.istate = stateExecHandler
	}

	return nil
}

func (strm *ServerStream) handleReset(fr *Frame) error {
	// fr := AcquireRstStream()
	// defer ReleaseRstStream(fr)

	// TODO: handle error codes
	// err := fr.ReadFrame(ctx.fr)
	// if err != nil {
	// 	return err
	// }

	strm.state = StateClosed
	// TODO: propagate ctx.streamsOpen--

	return nil
}

func (strm *ServerStream) handleGoAway(fr *Frame) error {
	// TODO:
	// fr.Reset()
	//
	// ga := AcquireGoAway()
	// defer ReleaseGoAway(ga)
	//
	// // TODO: ... ctx.isClosing = true
	//
	// ga.SetStream(ctx.lastStreamOpen)
	// // TODO: Replace with proper code
	// ga.SetCode(0x0)
	//
	// fr.SetStream(strm.id)
	//
	// ga.WriteFrame(fr)
	// return ctx.writeFrame(fr)
	return nil
}

func (strm *ServerStream) handleWindowUpdate(fr *Frame) error {
	wu := AcquireWindowUpdate()
	defer ReleaseWindowUpdate(wu)

	wu.ReadFrame(fr)

	// TODO: Should be shared ...
	strm.windowSize += wu.increment

	return nil
}

func (strm *ServerStream) handleSettings(fr *Frame) error {
	// TODO: ...
	st := AcquireSettings()

	st.ReadFrame(fr)

	if st.IsAck() {
		ctx.unackedSettings--
		if ctx.unackedSettings < 0 {
			ctx.unackedSettings = 0
		}
		ReleaseSettings(st)
		return nil
	}

	if ctx.cst == nil {
		ctx.cst = st
		ctx.windowSize = st.windowSize
	} else {
		st.CopyTo(ctx.cst)
		defer ReleaseSettings(st)
	}

	fr := AcquireFrame()
	defer ReleaseFrame(fr)

	fr.SetType(FrameSettings)
	fr.AddFlag(FlagAck)
	err := ctx.writeFrame(fr)
	if err == nil {
		err = ctx.bw.Flush()
	}

	return err
}

func (s *Server) tryReply(ctx *connCtx, strm *ServerStream) error {
	dfr := AcquireData()
	defer ReleaseData(dfr)

	hfr := AcquireHeaders()
	defer ReleaseHeaders(hfr)

	err := ctx.writeHeaders(strm, hfr)
	if err == nil {
		err = ctx.writeData(strm, dfr)
	}
	if err == nil {
		err = ctx.bw.Flush()
	}

	return err
}

func (ctx *connCtx) writeWindowUpdate(strm *ServerStream, n uint32) error {
	fr := AcquireFrame()
	defer ReleaseFrame(fr)

	wu := AcquireWindowUpdate()
	defer ReleaseWindowUpdate(wu)

	id := uint32(0)
	if strm != nil {
		id = strm.id
		strm.windowSize += n
	} else {
		ctx.windowSize += n
	}

	wu.SetIncrement(n)
	wu.WriteFrame(fr)

	fr.SetStream(id)
	_, err := fr.WriteTo(ctx.bw)
	if err == nil {
		err = ctx.bw.Flush()
	}

	return err
}

func (ctx *connCtx) writeReset(strm *ServerStream) error {
	rfr := AcquireRstStream()
	defer ReleaseRstStream(rfr)

	fr := AcquireFrame()
	defer ReleaseFrame(fr)

	fr.SetStream(strm.id)

	// TODO: Replace with proper code
	rfr.SetCode(0x0)
	rfr.WriteFrame(fr)

	_, err := fr.WriteTo(ctx.bw)
	if err == nil {
		err = ctx.bw.Flush()
	}

	return err
}

func (ctx *connCtx) writeHeaders(strm *ServerStream, hfr *Headers) error {
	fr := AcquireFrame()
	defer ReleaseFrame(fr)

	fr.SetStream(strm.id)

	fasthttpResponseHeaders(hfr, ctx.hp, &strm.ctx.Response)
	hfr.SetEndHeaders(true)
	hfr.WriteFrame(fr)

	_, err := fr.WriteTo(ctx.bw)
	return err
}

func (ctx *connCtx) writeData(strm *ServerStream, dfr *Data) error {
	fr := AcquireFrame()
	defer ReleaseFrame(fr)

	fr.SetStream(strm.id)
	fr.SetMaxLen(strm.windowSize)

	var (
		n   int64
		err error
	)

	body := strm.ctx.Response.Body()
	step := 1 << 14

	for i := 0; err == nil && i < len(body); i += step {
		if i+step > len(body) {
			step = len(body) - i
		}

		dfr.SetData(body[i : i+step])
		dfr.SetPadding(false)
		dfr.SetEndStream(i+step == len(body))
		dfr.WriteFrame(fr)

		n, err = fr.WriteTo(ctx.bw)
		if err == nil {
			strm.windowSize -= uint32(n)
		}
	}
	if err == nil && strm.state == StateHalfClosed {
		strm.state = StateClosed
	}

	return err
}
