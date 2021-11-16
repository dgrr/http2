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
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/valyala/fasthttp"
)

type Server struct {
	s *fasthttp.Server
}

type serverConn struct {
	c net.Conn
	h fasthttp.RequestHandler

	br *bufio.Reader
	bw *bufio.Writer

	enc *HPACK
	dec *HPACK

	lastID uint32

	clientWindow       int32
	clientStreamWindow int32
	maxWindow          int32
	currentWindow      int32

	writer *frameHeaderWriter
	reader chan *FrameHeader

	readTimeout time.Duration

	st      Settings
	clientS Settings
}

type frameHeaderWriter struct {
	Frame
	w   chan *FrameHeader
	err chan error
}

func (s *Server) ServeConn(c net.Conn) error {
	defer func() { _ = c.Close() }()

	if !ReadPreface(c) {
		return errors.New("wrong preface")
	}

	sc := &serverConn{
		c:      c,
		h:      s.s.Handler,
		br:     bufio.NewReader(c),
		bw:     bufio.NewWriterSize(c, 1<<14*10),
		enc:    AcquireHPACK(),
		dec:    AcquireHPACK(),
		lastID: 0,
		writer: &frameHeaderWriter{
			w:   make(chan *FrameHeader, 128),
			err: make(chan error),
		},
		reader:      make(chan *FrameHeader, 128),
		readTimeout: s.s.ReadTimeout,
	}

	sc.maxWindow = 1 << 22
	sc.currentWindow = sc.maxWindow

	sc.st.Reset()
	sc.st.SetMaxWindowSize(uint32(sc.maxWindow))
	sc.st.SetMaxConcurrentStreams(1024)

	if err := Handshake(false, sc.bw, &sc.st, sc.maxWindow); err != nil {
		return err
	}

	go func() {
		sc.handleStreams()
		close(sc.writer.w)
	}()
	go sc.writeLoop()

	defer func() {
		close(sc.reader)
	}()

	var (
		fr  *FrameHeader
		err error
	)

	// unset any deadline
	err = c.SetWriteDeadline(time.Time{})
	if err != nil {
		return err
	}
	err = c.SetReadDeadline(time.Time{})
	if err != nil {
		return err
	}

	for err == nil {
		fr, err = ReadFrameFromWithSize(sc.br, sc.clientS.frameSize)
		if err != nil {
			if errors.Is(err, ErrUnknownFrameType) {
				err = nil
				continue
			}
			break
		}

		if fr.Stream() != 0 && fr.Type() != FrameWindowUpdate {
			if fr.Stream()&1 == 0 {
				sc.writeGoAway(fr.Stream(), ProtocolError, "invalid stream id")
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
			win := int32(fr.Body().(*WindowUpdate).Increment())

			if !atomic.CompareAndSwapInt32(&sc.clientWindow, 0, win) {
				atomic.AddInt32(&sc.clientWindow, win)
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

	if errors.Is(err, io.EOF) {
		err = nil
	}

	return err
}

func (sc *serverConn) handlePing(ping *Ping) {
	fr := AcquireFrameHeader()
	ping.SetAck(true)
	fr.SetBody(ping)

	if err := sc.writer.receive(fr); err != nil {
		// TODO
	}
}

func (sc *serverConn) writePing() {
	fr := AcquireFrameHeader()

	ping := AcquireFrame(FramePing).(*Ping)
	ping.SetCurrentTime()

	fr.SetBody(ping)

	if err := sc.writer.receive(fr); err != nil {
		// TODO
	}
}

// handleStreams handles everything related to the streams and the HPACK table synchronously.
func (sc *serverConn) handleStreams() {
	strms := make(map[uint32]*Stream)
	closedStrms := make(map[uint32]struct{})
	var currentStrm uint32

	for fr := range sc.reader {
		strm, ok := strms[fr.Stream()]
		if !ok { // then create it
			if fr.Type() == FrameResetStream {
				// only send go away on idle stream not on already closed stream
				if _, ok = closedStrms[fr.Stream()]; !ok {
					sc.writeGoAway(fr.Stream(), ProtocolError, "RST_STREAM on idle stream")
				}
				continue
			}

			// We don't need to check frame WINDOW_UPDATEs because they do not arrive to this function.
			if _, ok = closedStrms[fr.Stream()]; ok {
				if fr.Type() != FramePriority {
					sc.writeGoAway(fr.Stream(), StreamClosedError, "frame on closed stream")
				}

				continue
			}

			if len(strms) >= int(sc.st.maxStreams) {
				sc.writeReset(fr.Stream(), RefusedStreamError)
				continue
			}

			strm = NewStream(fr.Stream(), sc.clientStreamWindow)
			strms[fr.Stream()] = strm

			if strm.ID() < sc.lastID {
				sc.writeGoAway(strm.ID(), ProtocolError, "stream id too low")
				continue
			}

			if fr.Type() == FrameHeaders {
				sc.lastID = strm.ID()
			}

			sc.createStream(sc.c, strm)
		}

		if currentStrm != 0 && currentStrm != fr.Stream() {
			sc.writeError(strm, NewGoAwayError(ProtocolError, "previous stream headers not ended"))
			continue
		}

		if err := sc.handleFrame(strm, fr); err != nil {
			sc.writeError(strm, err)
		}

		if strm.headersFinished {
			currentStrm = 0
		}

		handleState(fr, strm)

		if strm.State() < StreamStateHalfClosed && sc.readTimeout > 0 {
			if time.Since(strm.startedAt) > sc.readTimeout {
				sc.writeGoAway(strm.ID(), StreamCanceled, "timeout")
				strm.SetState(StreamStateClosed)
			}
		}

		switch strm.State() {
		case StreamStateHalfClosed:
			sc.handleEndRequest(strm)
			fallthrough
		case StreamStateClosed:
			ctxPool.Put(strm.ctx)
			closedStrms[strm.ID()] = struct{}{}
			delete(strms, strm.ID())
			streamPool.Put(strm)
		case StreamStateOpen:
			currentStrm = strm.ID()
		}
	}
}

func (sc *serverConn) writeReset(strm uint32, code ErrorCode) {
	r := AcquireFrame(FrameResetStream).(*RstStream)

	fr := AcquireFrameHeader()
	fr.SetStream(strm)
	fr.SetBody(r)

	r.SetCode(code)

	if err := sc.writer.receive(fr); err != nil {
		// TODO
	}
}

func (sc *serverConn) writeGoAway(strm uint32, code ErrorCode, message string) {
	ga := AcquireFrame(FrameGoAway).(*GoAway)

	fr := AcquireFrameHeader()

	ga.SetStream(strm)
	ga.SetCode(code)
	ga.SetData([]byte(message))

	fr.SetBody(ga)

	if err := sc.writer.receive(fr); err != nil {
		// TODO
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
		sc.writeGoAway(strm.ID(), streamErr.Code(), streamErr.Error())
	case FrameResetStream:
		sc.writeReset(strm.ID(), streamErr.Code())
	}

	strm.SetState(StreamStateClosed)
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
		}
	case StreamStateHalfClosed:
		if fr.Flags().Has(FlagEndStream) {
			strm.SetState(StreamStateClosed)
		} else if fr.Type() == FrameResetStream {
			strm.SetState(StreamStateClosed)
		}
	case StreamStateClosed:
	}
}

var logger = log.New(os.Stdout, "", log.LstdFlags)

var ctxPool = sync.Pool{
	New: func() interface{} {
		return &fasthttp.RequestCtx{}
	},
}

func (sc *serverConn) createStream(c net.Conn, strm *Stream) {
	ctx := ctxPool.Get().(*fasthttp.RequestCtx)
	ctx.Request.Reset()
	ctx.Response.Reset()

	ctx.Init2(c, logger, false)

	strm.startedAt = time.Now()
	strm.SetData(ctx)
}

func (sc *serverConn) handleFrame(strm *Stream, fr *FrameHeader) (err error) {
	ctx := strm.ctx

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

	switch fr.Type() {
	case FrameHeaders, FrameContinuation:
		if strm.headersFinished {
			if fr.Flags().Has(FlagEndStream) && fr.Flags().Has(FlagEndHeaders) && fr.Type() == FrameHeaders {
				// TODO handle trailers
			} else {
				return NewGoAwayError(ProtocolError, "stream not open")
			}
		}

		if fr.Flags().Has(FlagEndHeaders) {
			strm.headersFinished = true
		}

		b := append(strm.previousHeaderBytes, fr.Body().(FrameWithHeaders).Headers()...)
		hf := AcquireHeaderField()
		scheme := []byte("https")
		req := &ctx.Request

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
				scheme = append(scheme[:0], v...)
			case 'a': // authority
				req.Header.SetHostBytes(v)
				req.Header.AddBytesV("Host", v)
			case 'u': // user-agent
				req.Header.SetUserAgentBytes(v)
			case 'c': // content-type
				req.Header.SetContentTypeBytes(v)
			}
		}

		// calling req.URI() triggers a URL parsing, so because of that we need to delay the URL parsing.
		req.URI().SetSchemeBytes(scheme)
	case FrameData:
		if !strm.headersFinished {
			return NewGoAwayError(ProtocolError, "stream open")
		}

		if strm.State() >= StreamStateHalfClosed {
			return NewGoAwayError(StreamClosedError, "stream closed")
		}
		ctx.Request.AppendBody(
			fr.Body().(*Data).Data())
	case FrameResetStream:
		if strm.State() == StreamStateIdle {
			return NewGoAwayError(ProtocolError, "RST_STREAM on idle stream")
		}
	case FramePriority:
	default:
		return NewGoAwayError(ProtocolError, "invalid frame")
	}

	return err
}

// handleEndRequest dispatches the finished request to the handler.
func (sc *serverConn) handleEndRequest(strm *Stream) {
	ctx := strm.ctx
	ctx.Request.Header.SetProtocolBytes(StringHTTP2)

	sc.h(ctx)

	// control the stack after the dispatch
	//
	// this recover is here just in case the sc.writer<-fr fails.
	defer func() {
		if err := recover(); err != nil {
			// TODO: idk
		}
	}()

	hasBody := ctx.Response.IsBodyStream() || len(ctx.Response.Body()) > 0

	fr := AcquireFrameHeader()
	fr.SetStream(strm.ID())

	h := AcquireFrame(FrameHeaders).(*Headers)
	h.SetEndHeaders(true)
	h.SetEndStream(!hasBody)

	fr.SetBody(h)

	fasthttpResponseHeaders(h, sc.enc, &ctx.Response)

	if err := sc.writer.receive(fr); err != nil {
		// TODO
		return
	}

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
	writer  *frameHeaderWriter
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
		if err = s.writer.receive(fr); err != nil {
			// TODO
			return i, err
		}
	}

	return len(body), nil
}

func (s *streamWrite) ReadFrom(r io.Reader) (num int64, err error) {
	vbuf := copyBufPool.Get()
	buf := vbuf.([]byte)
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

		fr := AcquireFrameHeader()
		fr.SetStream(s.strm.ID())

		data := AcquireFrame(FrameData).(*Data)
		data.SetEndStream(err != nil || (s.size >= 0 && num+int64(n) >= s.size))
		data.SetPadding(false)
		data.SetData(buf[:n])
		fr.SetBody(data)

		if err = s.writer.receive(fr); err != nil {
			// TODO
			return num, err
		}
		num += int64(n)
		if err != nil || (s.size >= 0 && num >= s.size) {
			break
		}
	}
	copyBufPool.Put(vbuf)
	if errors.Is(err, io.EOF) {
		return num, nil
	}
	return num, err
}

func (sc *serverConn) writeData(strm *Stream, body []byte) {
	step := 1 << 14 // max frame size 16384

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

		if err := sc.writer.receive(fr); err != nil {
			// TODO
			return
		}
	}
}

func (sc *serverConn) writeLoop() {
	ticker := time.NewTicker(time.Second * 10)
	defer func() {
		ticker.Stop()
		close(sc.writer.err)
	}()

	buffered := 0

loop:
	for {
		select {
		case fr, ok := <-sc.writer.w:
			if !ok {
				break loop
			}

			_, err := fr.WriteTo(sc.bw)
			if err == nil && (len(sc.writer.w) == 0 || buffered > 10) {
				err = sc.bw.Flush()
				buffered = 0
			} else if err == nil {
				buffered++
			}

			ReleaseFrameHeader(fr)

			if err != nil {
				sc.writer.err <- err
				return
			}
		case <-ticker.C:
			sc.writePing()
		}
	}
}

func (sc *serverConn) handleSettings(st *Settings) {
	st.CopyTo(&sc.clientS)

	atomic.StoreInt32(&sc.clientStreamWindow, int32(sc.clientS.MaxWindowSize()))

	fr := AcquireFrameHeader()

	stRes := AcquireFrame(FrameSettings).(*Settings)
	stRes.SetAck(true)

	fr.SetBody(stRes)

	if err := sc.writer.receive(fr); err != nil {
		// TODO
	}
}

func (fhw *frameHeaderWriter) receive(fr *FrameHeader) error {
	select {
	case fhw.w <- fr:
		return nil
	case err := <-fhw.err:
		return err
	}
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
