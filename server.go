package http2

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"

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
			fastCtx: &fasthttp.RequestCtx{},
		}
	},
}

// connCtx is an object intended to handle the input connection
// regardless the stream.
type connCtx struct {
	s  *Server
	c  net.Conn
	br *bufio.Reader
	bw *bufio.Writer

	hp *HPACK

	st *Settings

	unackedSettings int
	lastStreamOpen  uint32
	streamsOpen     int32
	isClosing       bool // after recv goaway

	strms map[uint32]*ServerStream
}

var connCtxPool = sync.Pool{
	New: func() interface{} {
		return &connCtx{}
	},
}

func acquireConnCtx(s *Server, c net.Conn) *connCtx {
	ctx := connCtxPool.Get().(*connCtx)
	ctx.s = s
	ctx.c = c
	ctx.br = bufio.NewReader(c)
	ctx.bw = bufio.NewWriter(c)
	ctx.hp = AcquireHPACK()
	ctx.st = AcquireSettings()
	ctx.unackedSettings = 0
	ctx.lastStreamOpen = 0
	ctx.streamsOpen = 0
	ctx.isClosing = false
	ctx.strms = make(map[uint32]*ServerStream)
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

func (ctx *connCtx) streamOpened() {
	atomic.AddInt32(&ctx.streamsOpen, 1)
}

func (ctx *connCtx) streamClosed() {
	atomic.AddInt32(&ctx.streamsOpen, -1)
}

func (ctx *connCtx) incrementWindow(inc uint32) {
	atomic.AddUint32(&ctx.st.windowSize, inc)
}

func (ctx *connCtx) decrementWindow(inc uint32) {
	// TODO: wtf? negative uint? xd
	atomic.AddUint32(&ctx.st.windowSize, -inc)
}

func (ctx *connCtx) handle(fastCtx *fasthttp.RequestCtx) {
	ctx.s.s.Handler(fastCtx)
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

	ctx := acquireConnCtx(s, c)
	defer releaseConnCtx(ctx)

	var err error

	fr := AcquireFrame()

	// write own settings
	ctx.st.WriteFrame(fr)
	err = ctx.writeFrame(fr)
	if err == nil {
		err = ctx.writeWindowUpdate(ctx.st.windowSize - 65535)
	}

	ReleaseFrame(fr)

	var (
		wch      = make(chan *Frame, 1024)
		cemetery = make(chan *ServerStream, 1)
	)

	go s.writeLoop(ctx, wch, cemetery)

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
			return nil
		case FrameWindowUpdate:
			ctx.handleWindowUpdate(fr)
		case FrameSettings:
			err = ctx.handleSettings(fr)
		default:
			strm := ctx.strms[fr.Stream()]
			if strm == nil {
				strm = acquireStream(fr.Stream(), ctx, wch, cemetery)
				ctx.strms[fr.Stream()] = strm
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

func (s *Server) writeLoop(ctx *connCtx, wch <-chan *Frame, cemetery chan *ServerStream) {
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
		case strm, ok := <-cemetery:
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
		// println("priority")
		// TODO: If a PRIORITY frame is received with a stream identifier of 0x0, the recipient MUST respond with a connection error
	case FrameResetStream:
		strm.handleReset(fr)
	case FramePushPromise:
		println("pp")
	case FramePing:
		fr.AddFlag(FlagAck)
		strm.writeFrame(fr)
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
	// TODO: Required?
	cemetery chan<- *ServerStream
}

func acquireStream(
	id uint32,
	ctx *connCtx,
	wch chan<- *Frame,
	cemetery chan<- *ServerStream,
) *ServerStream {
	strm := streamPool.Get().(*ServerStream)
	strm.fastCtx.Request.Reset()
	strm.fastCtx.Response.Reset()

	strm.state = StateIdle
	strm.istate = stateNone
	strm.id = id
	strm.ctx = ctx
	strm.wch = wch
	strm.cemetery = cemetery

	strm.rch = make(chan *Frame, 128)

	return strm
}

func releaseStream(strm *ServerStream) {
	close(strm.rch)

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
	for strm.state != StateClosed {
		select {
		case fr, ok := <-strm.rch:
			if !ok {
				return
			}

			err := strm.handleFrame(fr)
			if err == nil && strm.istate == stateExecHandler {
				strm.ctx.handle(strm.fastCtx)
				strm.tryReply()
				strm.istate = stateNone
			}
			if err != nil {
				log.Println(err)
			}

			ReleaseFrame(fr)
		}
	}

	strm.cemetery <- strm
}

func (strm *ServerStream) handleHeaders(fr *Frame) error {
	switch strm.state {
	case StateIdle:
		strm.state = StateOpen
	case StateReserved:
		strm.state = StateHalfClosed
	}

	hfr := AcquireHeaders()

	// Read data from ctx.fr to Headers
	err := hfr.ReadFrame(fr)
	if err != nil {
		return err
	}

	err = strm.parseHeaders(hfr.rawHeaders, hfr.EndHeaders())
	ReleaseHeaders(hfr)

	return err
}

func (strm *ServerStream) handleContinuation(fr *Frame) (err error) {
	cfr := AcquireContinuation()

	cfr.ReadFrame(fr)
	err = strm.parseHeaders(cfr.rawHeaders, cfr.EndHeaders())
	ReleaseContinuation(cfr)

	return
}

func (strm *ServerStream) parseHeaders(b []byte, isEnd bool) (err error) {
	hf := AcquireHeaderField()

	for len(b) > 0 {
		b, err = strm.ctx.hp.Next(hf, b)
		if err != nil {
			println(err, hf.Key())
			break
		}

		fasthttpRequestHeaders(hf, &strm.fastCtx.Request)
	}

	if err == nil {
		if isEnd {
			strm.istate = stateExecHandler
		} else {
			strm.istate = stateAwaitData
		}
		strm.fastCtx.Request.SetRequestURIBytes(
			strm.fastCtx.Request.URI().FullURI())
		strm.fastCtx.Request.Header.SetProtocolBytes(strHTTP2)
	}

	ReleaseHeaderField(hf)

	return
}

func (strm *ServerStream) handleData(fr *Frame) error {
	dfr := AcquireData()

	dfr.ReadFrame(fr)
	if dfr.Len() > 0 {
		strm.fastCtx.Request.AppendBody(dfr.Data())
		strm.writeWindowUpdate(uint32(len(strm.fastCtx.Request.Body())))
	}
	if dfr.EndStream() {
		strm.istate = stateExecHandler
	}

	ReleaseData(dfr)

	return nil
}

func (strm *ServerStream) handleReset(fr *Frame) {
	// fr := AcquireRstStream()
	// defer ReleaseRstStream(fr)

	// TODO: handle error codes
	// err := fr.ReadFrame(ctx.fr)
	// if err != nil {
	// 	return err
	// }

	strm.state = StateClosed
	// TODO: propagate ctx.streamsOpen--
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

func (ctx *connCtx) handleWindowUpdate(fr *Frame) {
	wu := AcquireWindowUpdate()
	defer ReleaseWindowUpdate(wu)

	wu.ReadFrame(fr)

	// TODO: Should be shared ...
	ctx.st.windowSize += wu.increment
}

func (ctx *connCtx) handleSettings(fr *Frame) error {
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

	st.CopyTo(ctx.st)

	fr.Reset()
	fr.SetType(FrameSettings)
	fr.AddFlag(FlagAck)

	// TODO: Concurrency ...
	err := ctx.writeFrame(fr)
	if err == nil {
		err = ctx.bw.Flush()
	}

	ReleaseSettings(st)

	return err
}

func (strm *ServerStream) tryReply() {
	dfr := AcquireData()
	hfr := AcquireHeaders()

	strm.writeHeaders(hfr)
	strm.writeData(dfr)

	ReleaseHeaders(hfr)
	ReleaseData(dfr)
}

func (ctx *connCtx) writeWindowUpdate(n uint32) error {
	fr := AcquireFrame()
	wu := AcquireWindowUpdate()

	// TODO: increment
	ctx.st.windowSize += n

	wu.SetIncrement(n)
	wu.WriteFrame(fr)

	fr.SetStream(0)

	_, err := fr.WriteTo(ctx.bw)
	if err == nil {
		err = ctx.bw.Flush()
	}

	ReleaseWindowUpdate(wu)

	return err
}

func (strm *ServerStream) writeWindowUpdate(n uint32) {
	fr := AcquireFrame()
	wu := AcquireWindowUpdate()

	// TODO: increment
	strm.windowSize += n

	wu.SetIncrement(n)
	wu.WriteFrame(fr)

	fr.SetStream(strm.id)

	strm.writeFrame(fr)

	ReleaseWindowUpdate(wu)
}

func (strm *ServerStream) writeReset() {
	fr := AcquireFrame()
	rfr := AcquireRstStream()

	fr.SetStream(strm.id)

	// TODO: Replace with proper code
	rfr.SetCode(0x0)
	rfr.WriteFrame(fr)

	strm.writeFrame(fr)

	ReleaseRstStream(rfr)
}

func (strm *ServerStream) writeHeaders(hfr *Headers) {
	fr := AcquireFrame()

	fr.SetStream(strm.id)

	// TODO: Check all ctx.hp access to make it concurrent etc
	fasthttpResponseHeaders(hfr, strm.ctx.hp, &strm.fastCtx.Response)
	hfr.SetEndHeaders(true)
	hfr.WriteFrame(fr)

	strm.writeFrame(fr)
}

func (strm *ServerStream) writeData(dfr *Data) {
	body := strm.fastCtx.Response.Body()
	step := 1 << 14

	for i := 0; i < len(body); i += step {
		if i+step > len(body) {
			step = len(body) - i
		}

		fr := AcquireFrame()

		fr.SetStream(strm.id)
		fr.SetMaxLen(strm.windowSize)

		dfr.SetData(body[i : i+step])
		dfr.SetPadding(false)
		dfr.SetEndStream(i+step == len(body))
		dfr.WriteFrame(fr)

		strm.writeFrame(fr)
		// TODO: strm.windowSize -= uint32(n)
	}
	if strm.state == StateHalfClosed {
		strm.state = StateClosed
	}
}
