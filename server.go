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

	writer chan *FrameHeader
	reader chan *FrameHeader

	st      Settings
	clientS Settings
}

func (s *Server) ServeConn(c net.Conn) error {
	defer c.Close()

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
		writer: make(chan *FrameHeader, 128),
		reader: make(chan *FrameHeader, 128),
	}

	sc.maxWindow = 1 << 22
	sc.currentWindow = sc.maxWindow

	sc.st.Reset()
	sc.st.SetMaxWindowSize(uint32(sc.maxWindow))
	sc.st.SetMaxConcurrentStreams(1024)

	if err := Handshake(false, sc.bw, &sc.st, sc.maxWindow); err != nil {
		return err
	}

	go sc.handleStreams()
	go sc.writeLoop()

	defer func() {
		close(sc.writer)
		close(sc.reader)
	}()

	var (
		fr  *FrameHeader
		err error
	)

	for err == nil {
		fr, err = ReadFrameFrom(sc.br)
		if err != nil {
			break
		}

		if fr.Stream() != 0 {
			if fr.Stream()&1 == 0 {
				sc.writeReset(fr.Stream(), ProtocolError)
			} else {
				sc.reader <- fr
			}

			continue
		}

		switch fr.Type() {
		case FrameSettings:
			st := fr.Body().(*Settings)
			if !st.IsAck() { // if has ack, just ignore
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
		}

		ReleaseFrameHeader(fr)
	}

	if err == io.EOF {
		err = nil
	}

	return err
}

func (sc *serverConn) handlePing(ping *Ping) {
	fr := AcquireFrameHeader()
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

func (sc *serverConn) handleStreams() {
	var strms = make(map[uint32]*Stream)

	for {
		select {
		case fr, ok := <-sc.reader:
			if !ok {
				return
			}

			strm, ok := strms[fr.Stream()]
			if !ok { // then create it
				if len(strms) > int(sc.st.maxStreams) {
					sc.writeReset(fr.Stream(), RefusedStreamError)
					continue
				}

				strm = NewStream(fr.Stream(), sc.clientStreamWindow)
				strms[fr.Stream()] = strm

				// TODO: sc.lastID = strm.ID()

				sc.createStream(sc.c, strm)
			}

			if err := sc.handleFrame(strm, fr); err != nil {
				sc.writeError(strm, err)
			}

			handleState(fr, strm)

			switch strm.State() {
			case StreamStateHalfClosed:
				sc.handleEndRequest(strm)
				fallthrough
			case StreamStateClosed:
				ctxPool.Put(strm.ctx)
				delete(strms, strm.ID())
				streamPool.Put(strm)
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
}

func (sc *serverConn) writeError(strm *Stream, err error) {
	code := ErrorCode(InternalError)
	if errors.Is(err, Error{}) {
		code = err.(Error).Code()
	}

	sc.writeReset(strm.ID(), code)
	strm.SetState(StreamStateClosed)
}

func handleState(fr *FrameHeader, strm *Stream) {
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

	// ctx.Ctx.Header.DisableNormalizing()
	// ctx.Ctx.URI().DisablePathNormalizing = true

	strm.SetData(ctx)
}

func (sc *serverConn) handleFrame(strm *Stream, fr *FrameHeader) (err error) {
	ctx := strm.ctx

	switch fr.Type() {
	case FrameHeaders, FrameContinuation:
		b := fr.Body().(FrameWithHeaders).Headers()
		hf := AcquireHeaderField()
		scheme := []byte("https")
		req := &ctx.Request

		for len(b) > 0 {
			b, err = sc.dec.Next(hf, b)
			if err != nil {
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
		ctx.Request.AppendBody(
			fr.Body().(*Data).Data())
	}

	return err
}

func (sc *serverConn) handleEndRequest(strm *Stream) {
	ctx := strm.ctx
	ctx.Request.Header.SetProtocolBytes(StringHTTP2)

	sc.h(ctx)

	hasBody := len(ctx.Response.Body()) != 0

	fr := AcquireFrameHeader()
	fr.SetStream(strm.ID())

	h := AcquireFrame(FrameHeaders).(*Headers)
	h.SetEndHeaders(true)
	h.SetEndStream(!hasBody)

	fr.SetBody(h)

	fasthttpResponseHeaders(h, sc.enc, &ctx.Response)

	sc.writer <- fr

	if hasBody {
		sc.writeData(strm, ctx.Response.Body())
	}
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
		sc.writer <- fr
	}
}

func (sc *serverConn) writeLoop() {
	ticker := time.NewTicker(time.Second * 10)
	defer ticker.Stop()

	buffered := 0

loop:
	for {
		select {
		case fr, ok := <-sc.writer:
			if !ok {
				break loop
			}

			_, err := fr.WriteTo(sc.bw)
			if err == nil && (len(sc.writer) == 0 || buffered > 10) {
				err = sc.bw.Flush()
				buffered = 0
			} else if err == nil {
				buffered++
			}

			ReleaseFrameHeader(fr)

			if err != nil {
				// TODO: handle errors
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

	res.Header.SetContentLength(len(res.Body()))
	// Remove the Connection field
	res.Header.Del("Connection")

	res.Header.VisitAll(func(k, v []byte) {
		hf.SetBytes(ToLower(k), v)
		dst.AppendHeaderField(hp, hf, false)
	})
}
