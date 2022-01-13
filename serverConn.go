package http2

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"runtime/debug"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/valyala/fasthttp"
)

type serverConn struct {
	c net.Conn
	h fasthttp.RequestHandler

	br *bufio.Reader
	bw *bufio.Writer

	enc HPACK
	dec HPACK

	lastID uint32

	// client's window
	// should be int64 because the user can try to overflow it
	clientWindow int64

	// our values
	maxWindow     int32
	currentWindow int32

	writer chan *FrameHeader
	reader chan *FrameHeader

	// maxRequestTime is the max time of a request over one single stream
	maxRequestTime time.Duration
	pingInterval   time.Duration

	st      Settings
	clientS Settings

	// pingTimer
	pingTimer       *time.Timer
	maxRequestTimer *time.Timer

	debug  bool
	logger fasthttp.Logger
}

func (sc *serverConn) Handshake() error {
	return Handshake(false, sc.bw, &sc.st, sc.maxWindow)
}

func (sc *serverConn) Serve() error {
	sc.maxRequestTimer = time.NewTimer(0)
	sc.clientWindow = int64(sc.clientS.MaxWindowSize())

	defer func() {
		if err := recover(); err != nil {
			sc.logger.Printf("Serve panicked: %s:\n%s\n", err, debug.Stack())
		}
	}()

	go sc.writeLoop()
	go func() {
		sc.handleStreams()
		// close the writer here to ensure that no pending requests
		// are writing to a closed channel
		close(sc.writer)
	}()

	defer func() {
		// close the reader here so we can stop handling stream updates
		close(sc.reader)
	}()

	var err error

	// unset any deadline
	if err = sc.c.SetWriteDeadline(time.Time{}); err == nil {
		err = sc.c.SetReadDeadline(time.Time{})
	}
	if err != nil {
		return err
	}

	err = sc.readLoop()
	if errors.Is(err, io.EOF) {
		err = nil
	}

	if sc.pingTimer != nil {
		sc.pingTimer.Stop()
	}

	sc.maxRequestTimer.Stop()

	return err
}

func (sc *serverConn) handlePing(ping *Ping) {
	fr := AcquireFrameHeader()
	ping.SetAck(true)
	fr.SetBody(ping)

	sc.writer <- fr
}

func (sc *serverConn) writePing() {
	fr := AcquireFrameHeader()

	ping := AcquireFrame(FramePing).(*Ping)
	ping.SetCurrentTime()

	fr.SetBody(ping)

	sc.writer <- fr
}

func (sc *serverConn) checkFrameWithStream(fr *FrameHeader) error {
	if fr.Stream()&1 == 0 {
		return NewGoAwayError(ProtocolError, "invalid stream id")
	}

	switch fr.Type() {
	case FramePing:
		return NewGoAwayError(ProtocolError, "ping is carrying a stream id")
	}

	return nil
}

func (sc *serverConn) readLoop() (err error) {
	defer func() {
		if err := recover(); err != nil {
			sc.logger.Printf("readLoop panicked: %s\n%s\n", err, debug.Stack())
		}
	}()

	var fr *FrameHeader

	for err == nil {
		fr, err = ReadFrameFromWithSize(sc.br, sc.clientS.frameSize)
		if err != nil {
			if errors.Is(err, ErrUnknownFrameType) {
				sc.writeGoAway(0, ProtocolError, "unknown frame type")
				err = nil
				continue
			}

			break
		}

		if fr.Stream() != 0 {
			err := sc.checkFrameWithStream(fr)
			if err != nil {
				sc.writeError(nil, err)
			} else {
				sc.reader <- fr
			}

			continue
		}

		// handle 'anonymous' frames (frames without stream_id)
		switch fr.Type() {
		case FrameSettings:
			st := fr.Body().(*Settings)
			if !st.IsAck() { // if it has ack, just ignore
				sc.handleSettings(st)
			}
		case FrameWindowUpdate:
			win := int64(fr.Body().(*WindowUpdate).Increment())
			if win == 0 {
				sc.writeGoAway(0, ProtocolError, "window increment of 0")
				// return
				continue
			}

			if atomic.AddInt64(&sc.clientWindow, win) >= 1<<31-1 {
				sc.writeGoAway(0, FlowControlError, "window is above limits")
			}
		case FramePing:
			ping := fr.Body().(*Ping)
			if !ping.IsAck() {
				sc.handlePing(ping)
			}
		case FrameGoAway:
			ga := fr.Body().(*GoAway)
			if ga.Code() == NoError {
				err = io.EOF
			} else {
				err = fmt.Errorf("goaway: %s: %s", ga.Code(), ga.Data())
			}
		default:
			sc.writeGoAway(0, ProtocolError, "invalid frame")
		}

		ReleaseFrameHeader(fr)
	}

	return
}

// handleStreams handles everything related to the streams
// and the HPACK table is accessed synchronously.
func (sc *serverConn) handleStreams() {
	defer func() {
		if err := recover(); err != nil {
			sc.logger.Printf("handleStreams panicked: %s\n%s\n", err, debug.Stack())
		}
	}()

	var strms Streams
	var reqTimerArmed bool

	closedStrms := make(map[uint32]struct{})

	closeStream := func(strm *Stream) {
		ctxPool.Put(strm.ctx)
		closedStrms[strm.ID()] = struct{}{}
		strms.Del(strm.ID())
		streamPool.Put(strm)
	}

	for {
		select {
		case <-sc.maxRequestTimer.C:
			deleteUntil := 0
			for _, strm := range strms {
				// the request is due if the startedAt time + maxRequestTime is in the past
				isDue := time.Now().After(
					strm.startedAt.Add(sc.maxRequestTime))
				if !isDue {
					break
				}

				deleteUntil++
			}

			for deleteUntil > 0 {
				strm := strms[0]

				sc.writeReset(strm.ID(), StreamCanceled)

				// set the state to closed in case it comes back to life later
				strm.SetState(StreamStateClosed)
				closeStream(strm)

				deleteUntil--
			}

			if reqTimerArmed = len(strms) != 0 && sc.maxRequestTime > 0; reqTimerArmed {
				// try to arm the timer
				when := strms[0].startedAt.Add(sc.maxRequestTime).Sub(time.Now())
				sc.maxRequestTimer.Reset(when)
			}
		case fr, ok := <-sc.reader:
			if !ok {
				return
			}

			var strm *Stream
			if fr.Stream() <= sc.lastID {
				strm = strms.Search(fr.Stream())
				if strm == nil {
					// TODO: not found??
				}
			}

			if strm == nil {
				// if the stream doesn't exist, create it
				if fr.Type() == FrameResetStream {
					// only send go away on idle stream not on an already-closed stream
					if _, ok := closedStrms[fr.Stream()]; !ok {
						sc.writeGoAway(fr.Stream(), ProtocolError, "RST_STREAM on idle stream")
					}

					continue
				}

				if _, ok := closedStrms[fr.Stream()]; ok {
					if fr.Type() != FramePriority {
						sc.writeGoAway(fr.Stream(), StreamClosedError, "frame on closed stream")
					}

					continue
				}

				if len(strms) >= int(sc.st.maxStreams) {
					sc.writeReset(fr.Stream(), RefusedStreamError)
					continue
				}

				if fr.Stream() < sc.lastID {
					sc.writeGoAway(fr.Stream(), ProtocolError, "stream ID is lower than the latest")
					continue
				}

				strm = NewStream(fr.Stream(), int32(sc.clientWindow))
				strms = append(strms, strm)

				if fr.Type() == FrameHeaders || fr.Type() == FramePushPromise {
					sc.lastID = fr.Stream()
				}

				sc.createStream(sc.c, fr.Type(), strm)

				if !reqTimerArmed && sc.maxRequestTime > 0 {
					reqTimerArmed = true
					sc.maxRequestTimer.Reset(sc.maxRequestTime)
				}
			}

			// if we have more than one stream (this one newly created) check if the previous finished sending the headers
			if fr.Type() == FrameHeaders {
				strm := strms.getPrevious(FrameHeaders)
				if strm != nil && strm.State() < StreamStateHalfClosed {
					sc.writeError(strm, NewGoAwayError(ProtocolError, "previous stream headers not ended"))
					continue
				}
			}

			if err := sc.handleFrame(strm, fr); err != nil {
				sc.writeError(strm, err)
			}

			handleState(fr, strm)

			switch strm.State() {
			case StreamStateHalfClosed:
				sc.handleEndRequest(strm)
				// we fallthrough because once we send the response
				// the stream is already consumed and thus finished
				fallthrough
			case StreamStateClosed:
				closeStream(strm)
			}
		}
	}
}

func (sc *serverConn) writeReset(strm uint32, code ErrorCode) {
	r := AcquireFrame(FrameResetStream).(*RstStream)

	fr := AcquireFrameHeader()
	fr.SetStream(strm)
	fr.SetBody(r)

	r.SetCode(code)

	sc.writer <- fr

	if sc.debug {
		sc.logger.Printf(
			"%s: Reset(stream=%d, code=%s)\n",
			sc.c.RemoteAddr(), strm, code,
		)
	}
}

func (sc *serverConn) writeGoAway(strm uint32, code ErrorCode, message string) {
	ga := AcquireFrame(FrameGoAway).(*GoAway)

	fr := AcquireFrameHeader()

	ga.SetStream(strm)
	ga.SetCode(code)
	ga.SetData([]byte(message))

	fr.SetBody(ga)

	sc.writer <- fr

	if sc.debug {
		sc.logger.Printf(
			"%s: GoAway(stream=%d, code=%s): %s\n",
			sc.c.RemoteAddr(), strm, code, message,
		)
	}
}

func (sc *serverConn) writeError(strm *Stream, err error) {
	streamErr := Error{}
	if !errors.As(err, &streamErr) {
		sc.writeReset(strm.ID(), InternalError)
		strm.SetState(StreamStateClosed)
		return
	}

	switch streamErr.frameType {
	case FrameGoAway:
		if strm == nil {
			sc.writeGoAway(0, streamErr.Code(), streamErr.Error())
		} else {
			sc.writeGoAway(strm.ID(), streamErr.Code(), streamErr.Error())
		}
	case FrameResetStream:
		sc.writeReset(strm.ID(), streamErr.Code())
	}

	if strm != nil {
		strm.SetState(StreamStateClosed)
	}
}

func handleState(fr *FrameHeader, strm *Stream) {
	if fr.Type() == FrameResetStream {
		strm.SetState(StreamStateClosed)
	}

	switch strm.State() {
	case StreamStateIdle:
		if fr.Type() == FrameHeaders {
			strm.SetState(StreamStateOpen)
			if fr.Flags().Has(FlagEndStream) {
				strm.SetState(StreamStateHalfClosed)
			}
		} // TODO: else push promise ...
	case StreamStateReserved:
		// TODO: ...
	case StreamStateOpen:
		if fr.Flags().Has(FlagEndStream) {
			strm.SetState(StreamStateHalfClosed)
		} else if fr.Type() == FrameResetStream {
			strm.SetState(StreamStateClosed)
		}
	case StreamStateHalfClosed:
		// a stream can only go from HalfClosed to Closed if the client
		// sends a ResetStream frame.
		if fr.Type() == FrameResetStream {
			strm.SetState(StreamStateClosed)
		}
	case StreamStateClosed:
	}
}

var logger = log.New(os.Stdout, "[HTTP/2] ", log.LstdFlags)

var ctxPool = sync.Pool{
	New: func() interface{} {
		return &fasthttp.RequestCtx{}
	},
}

func (sc *serverConn) createStream(c net.Conn, frameType FrameType, strm *Stream) {
	ctx := ctxPool.Get().(*fasthttp.RequestCtx)
	ctx.Request.Reset()
	ctx.Response.Reset()

	ctx.Init2(c, sc.logger, false)

	strm.origType = frameType
	strm.startedAt = time.Now()
	strm.SetData(ctx)
}

func (sc *serverConn) handleFrame(strm *Stream, fr *FrameHeader) error {
	err := sc.verifyState(strm, fr)
	if err != nil {
		return err
	}

	switch fr.Type() {
	case FrameHeaders, FrameContinuation:
		err = sc.handleHeaderFrame(strm, fr)
		if err != nil {
			return err
		}

		if fr.Flags().Has(FlagEndHeaders) {
			strm.headersFinished = true

			// calling req.URI() triggers a URL parsing, so because of that we need to delay the URL parsing.
			strm.ctx.Request.URI().SetSchemeBytes(strm.scheme)
		}
	case FrameData:
		if !strm.headersFinished {
			return NewGoAwayError(ProtocolError, "stream didn't end the headers")
		}

		if strm.State() >= StreamStateHalfClosed {
			return NewGoAwayError(StreamClosedError, "stream closed")
		}

		strm.ctx.Request.AppendBody(
			fr.Body().(*Data).Data())
	case FrameResetStream:
		if strm.State() == StreamStateIdle {
			return NewGoAwayError(ProtocolError, "RST_STREAM on idle stream")
		}
	case FramePriority:
		if strm.State() != StreamStateIdle && !strm.headersFinished {
			return NewGoAwayError(ProtocolError, "stream open")
		}

		if priorityFrame, ok := fr.Body().(*Priority); ok && priorityFrame.Stream() == strm.ID() {
			return NewGoAwayError(ProtocolError, "stream that depends on itself")
		}
	case FrameWindowUpdate:
		if strm.State() == StreamStateIdle {
			return NewGoAwayError(ProtocolError, "window update on idle stream")
		}

		win := int64(fr.Body().(*WindowUpdate).Increment())
		if win == 0 {
			return NewGoAwayError(ProtocolError, "window increment of 0")
		}

		if atomic.AddInt64(&strm.window, win) >= 1<<31-1 {
			return NewResetStreamError(FlowControlError, "window is above limits")
		}
	default:
		return NewGoAwayError(ProtocolError, "invalid frame")
	}

	return err
}

func (sc *serverConn) handleHeaderFrame(strm *Stream, fr *FrameHeader) error {
	if strm.headersFinished && !fr.Flags().Has(FlagEndStream|FlagEndHeaders) {
		// TODO handle trailers
		return NewGoAwayError(ProtocolError, "stream not open")
	}

	if headerFrame, ok := fr.Body().(*Headers); ok && headerFrame.Stream() == strm.ID() {
		return NewGoAwayError(ProtocolError, "stream that depends on itself")
	}

	b := append(strm.previousHeaderBytes, fr.Body().(FrameWithHeaders).Headers()...)
	hf := AcquireHeaderField()
	req := &strm.ctx.Request

	var err error

	for len(b) > 0 {
		pb := b

		b, err = sc.dec.Next(hf, b)
		if err != nil {
			if errors.Is(err, ErrUnexpectedSize) && len(pb) > 0 {
				err = nil
				strm.previousHeaderBytes = append(strm.previousHeaderBytes[:0], pb...)
			} else {
				err = ErrCompression
			}
			break
		}

		k, v := hf.KeyBytes(), hf.ValueBytes()
		if !hf.IsPseudo() &&
			!(bytes.Equal(k, StringUserAgent) ||
				bytes.Equal(k, StringContentType)) {
			req.Header.AddBytesKV(k, v)
			continue
		}

		if hf.IsPseudo() {
			k = k[1:]
		}

		switch k[0] {
		case 'm': // method
			req.Header.SetMethodBytes(v)
		case 'p': // path
			req.Header.SetRequestURIBytes(v)
		case 's': // scheme
			strm.scheme = append(strm.scheme[:0], v...)
		case 'a': // authority
			req.Header.SetHostBytes(v)
			req.Header.AddBytesV("Host", v)
		case 'u': // user-agent
			req.Header.SetUserAgentBytes(v)
		case 'c': // content-type
			req.Header.SetContentTypeBytes(v)
		}
	}

	return err
}

func (sc *serverConn) verifyState(strm *Stream, fr *FrameHeader) error {
	switch strm.State() {
	case StreamStateIdle:
		if fr.Type() != FrameHeaders && fr.Type() != FramePriority {
			return NewGoAwayError(ProtocolError, "wrong frame on idle stream")
		}
	case StreamStateHalfClosed:
		if fr.Type() != FrameWindowUpdate && fr.Type() != FramePriority && fr.Type() != FrameResetStream {
			return NewGoAwayError(StreamClosedError, "wrong frame on half-closed stream")
		}
	default:
	}

	return nil
}

// handleEndRequest dispatches the finished request to the handler.
func (sc *serverConn) handleEndRequest(strm *Stream) {
	ctx := strm.ctx
	ctx.Request.Header.SetProtocolBytes(StringHTTP2)

	sc.h(ctx)

	hasBody := ctx.Response.IsBodyStream() || len(ctx.Response.Body()) > 0

	fr := AcquireFrameHeader()
	fr.SetStream(strm.ID())

	h := AcquireFrame(FrameHeaders).(*Headers)
	h.SetEndHeaders(true)
	h.SetEndStream(!hasBody)

	fr.SetBody(h)

	fasthttpResponseHeaders(h, &sc.enc, &ctx.Response)

	sc.writer <- fr

	if hasBody {
		if ctx.Response.IsBodyStream() {
			streamWriter := acquireStreamWrite()
			streamWriter.strm = strm
			streamWriter.writer = sc.writer
			streamWriter.size = int64(ctx.Response.Header.ContentLength())
			_ = ctx.Response.BodyWriteTo(streamWriter)
			releaseStreamWrite(streamWriter)
		} else {
			sc.writeData(strm, ctx.Response.Body())
		}
	}
}

var (
	copyBufPool = sync.Pool{
		New: func() interface{} {
			return make([]byte, 1<<14) // max frame size 16384
		},
	}
	streamWritePool = sync.Pool{
		New: func() interface{} {
			return &streamWrite{}
		},
	}
)

type streamWrite struct {
	size    int64
	written int64
	strm    *Stream
	writer  chan<- *FrameHeader
}

func acquireStreamWrite() *streamWrite {
	v := streamWritePool.Get()
	if v == nil {
		return &streamWrite{}
	}
	return v.(*streamWrite)
}

func releaseStreamWrite(streamWrite *streamWrite) {
	streamWrite.Reset()
	streamWritePool.Put(streamWrite)
}

func (s *streamWrite) Reset() {
	s.size = 0
	s.written = 0
	s.strm = nil
	s.writer = nil
}

func (s *streamWrite) Write(body []byte) (n int, err error) {
	if (s.size <= 0 && s.written > 0) || (s.size > 0 && s.written >= s.size) {
		return 0, errors.New("writer closed")
	}

	step := 1 << 14 // max frame size 16384

	n = len(body)
	s.written += int64(n)

	end := s.size < 0 || s.written >= s.size
	for i := 0; i < n; i += step {
		if i+step >= n {
			step = n - i
		}

		fr := AcquireFrameHeader()
		fr.SetStream(s.strm.ID())

		data := AcquireFrame(FrameData).(*Data)
		data.SetEndStream(end && i+step == n)
		data.SetPadding(false)
		data.SetData(body[i : step+i])

		fr.SetBody(data)

		s.writer <- fr
	}

	return len(body), nil
}

func (s *streamWrite) ReadFrom(r io.Reader) (num int64, err error) {
	buf := copyBufPool.Get().([]byte)

	if s.size < 0 {
		lrSize := limitedReaderSize(r)
		if lrSize >= 0 {
			s.size = lrSize
		}
	}

	var n int
	for {
		n, err = r.Read(buf[0:])
		if n <= 0 && err == nil {
			err = errors.New("BUG: io.Reader returned 0, nil")
		}

		if err != nil {
			break
		}

		fr := AcquireFrameHeader()
		fr.SetStream(s.strm.ID())

		data := AcquireFrame(FrameData).(*Data)
		data.SetEndStream(err != nil || (s.size >= 0 && num+int64(n) >= s.size))
		data.SetPadding(false)
		data.SetData(buf[:n])
		fr.SetBody(data)

		s.writer <- fr

		num += int64(n)
		if s.size >= 0 && num >= s.size {
			break
		}
	}

	copyBufPool.Put(buf)
	if errors.Is(err, io.EOF) {
		return num, nil
	}

	return num, err
}

func (sc *serverConn) writeData(strm *Stream, body []byte) {
	step := 1 << 14 // max frame size 16384
	if strm.window > 0 && step > int(strm.window) {
		step = int(strm.window)
	}

	for i := 0; i < len(body); i += step {
		if i+step >= len(body) {
			step = len(body) - i
		}

		fr := AcquireFrameHeader()
		fr.SetStream(strm.ID())

		data := AcquireFrame(FrameData).(*Data)
		data.SetEndStream(i+step == len(body))
		data.SetPadding(false)
		data.SetData(body[i : step+i])

		fr.SetBody(data)

		sc.writer <- fr
	}
}

func (sc *serverConn) sendPingAndSchedule() {
	sc.writePing()

	sc.pingTimer.Reset(sc.pingInterval)
}

func (sc *serverConn) writeLoop() {
	if sc.pingInterval > 0 {
		sc.pingTimer = time.AfterFunc(sc.pingInterval, sc.sendPingAndSchedule)
	}

	buffered := 0

	for fr := range sc.writer {
		_, err := fr.WriteTo(sc.bw)
		if err == nil && (len(sc.writer) == 0 || buffered > 10) {
			err = sc.bw.Flush()
			buffered = 0
		} else if err == nil {
			buffered++
		}

		ReleaseFrameHeader(fr)

		if err != nil {
			sc.logger.Printf("ERROR: writeLoop: %s\n", err)
			// TODO: sc.writer.err <- err
			return
		}
	}
}

func (sc *serverConn) handleSettings(st *Settings) {
	st.CopyTo(&sc.clientS)

	// atomically update the new window
	atomic.StoreInt64(&sc.clientWindow, int64(sc.clientS.MaxWindowSize()))

	fr := AcquireFrameHeader()

	stRes := AcquireFrame(FrameSettings).(*Settings)
	stRes.SetAck(true)

	fr.SetBody(stRes)

	sc.writer <- fr
}

func fasthttpResponseHeaders(dst *Headers, hp *HPACK, res *fasthttp.Response) {
	hf := AcquireHeaderField()
	defer ReleaseHeaderField(hf)

	hf.SetKeyBytes(StringStatus)
	hf.SetValue(
		strconv.FormatInt(
			int64(res.Header.StatusCode()), 10,
		),
	)

	dst.AppendHeaderField(hp, hf, true)

	if !res.IsBodyStream() {
		res.Header.SetContentLength(len(res.Body()))
	}
	// Remove the Connection field
	res.Header.Del("Connection")
	// Remove the Transfer-Encoding field
	res.Header.Del("Transfer-Encoding")

	res.Header.VisitAll(func(k, v []byte) {
		hf.SetBytes(ToLower(k), v)
		dst.AppendHeaderField(hp, hf, false)
	})
}

func limitedReaderSize(r io.Reader) int64 {
	lr, ok := r.(*io.LimitedReader)
	if !ok {
		return -1
	}
	return lr.N
}
