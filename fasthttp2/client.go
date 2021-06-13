package fasthttp2

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"errors"
	"net"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/dgrr/http2"
	"github.com/valyala/fasthttp"
)

var (
	ErrServerSupport = errors.New("server doesn't support HTTP/2")
)

type Client struct {
	c net.Conn

	br *bufio.Reader
	bw *bufio.Writer

	enc *http2.HPACK // encoding hpack (ours)
	dec *http2.HPACK // decoding hpack (server's)

	nextID uint32

	serverWindow       int32
	serverStreamWindow int32
	maxWindow          int32
	currentWindow      int32

	reqResCh chan *reqRes

	writer chan *http2.FrameHeader

	inData chan *http2.FrameHeader

	st      http2.Settings
	serverS http2.Settings
}

type reqRes struct {
	req *fasthttp.Request
	res *fasthttp.Response
	ch  chan error
}

func Dial(addr string, tlsConfig *tls.Config) (*Client, error) {
	tcpAddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return nil, err
	}

	// TODO: timeout ???
	c, err := net.DialTCP("tcp", nil, tcpAddr)
	if err != nil {
		return nil, err
	}

	c.SetNoDelay(true)

	tlsConn := tls.Client(c, tlsConfig)

	if err := tlsConn.Handshake(); err != nil {
		c.Close()
		return nil, err
	}

	if tlsConn.ConnectionState().NegotiatedProtocol != "h2" {
		c.Close()
		return nil, ErrServerSupport
	}

	cl := &Client{
		c:        tlsConn,
		br:       bufio.NewReader(tlsConn),
		bw:       bufio.NewWriter(tlsConn),
		writer:   make(chan *http2.FrameHeader, 128),
		reqResCh: make(chan *reqRes, 128),
		inData:   make(chan *http2.FrameHeader, 128),
		enc:      http2.AcquireHPACK(),
		dec:      http2.AcquireHPACK(),
		nextID:   1,
	}

	if err := cl.Handshake(); err != nil {
		c.Close()
		return nil, err
	}

	go cl.readLoop()
	go cl.writeLoop()
	go cl.handleStreams()

	cl.maxWindow = 1 << 20
	cl.currentWindow = cl.maxWindow
	cl.updateWindow(0, int(cl.maxWindow))

	return cl, nil
}

func (c *Client) Handshake() error {
	err := http2.WritePreface(c.bw)
	if err == nil {
		err = c.bw.Flush()
	}

	if err != nil {
		return err
	}

	// TODO: Make an option
	c.st.SetMaxWindowSize(1 << 16) // 65536
	c.st.SetPush(false)            // do not support push promises

	fr := http2.AcquireFrameHeader()
	defer http2.ReleaseFrameHeader(fr)

	fr.SetBody(&c.st)

	_, err = fr.WriteTo(c.bw)
	if err == nil {
		err = c.bw.Flush()
	}

	return nil
}

func (c *Client) Do(req *fasthttp.Request, res *fasthttp.Response) (err error) {
	rr := &reqRes{
		req: req,
		res: res,
		ch:  make(chan error, 1),
	}

	c.reqResCh <- rr

	select {
	case err = <-rr.ch:
	}

	return
}

func (c *Client) readLoop() {
	defer func() {
		// TODO: fix race conditions
		close(c.writer)
		close(c.inData)
	}()

	for {
		fr, err := http2.ReadFrameFrom(c.br)
		if err != nil {
			// TODO: handle
			panic(err)
		}

		if fr.Stream() != 0 {
			c.inData <- fr
			continue
		}

		switch fr.Type() {
		case http2.FrameSettings:
			st := fr.Body().(*http2.Settings)
			if !st.IsAck() { // if has ack, just ignore
				c.handleSettings(st)
			}
		case http2.FrameWindowUpdate:
			win := int32(fr.Body().(*http2.WindowUpdate).Increment())

			if !atomic.CompareAndSwapInt32(&c.serverWindow, 0, win) {
				atomic.AddInt32(&c.serverWindow, win)
			}
		case http2.FramePing:
			c.handlePing(fr.Body().(*http2.Ping))
		case http2.FrameGoAway:
			println(
				fr.Body().(*http2.GoAway).Code().Error())
			c.c.Close()
			return
		}

		http2.ReleaseFrameHeader(fr)
	}
}

func (c *Client) handleState(fr *http2.FrameHeader, strm *http2.Stream) {
	switch strm.State() {
	case http2.StreamStateIdle:
		if fr.Type() == http2.FrameHeaders {
			strm.SetState(http2.StreamStateOpen)
		} // TODO: else push promise ...
	case http2.StreamStateReserved:
		// TODO: ...
	case http2.StreamStateOpen:
		if fr.Flags().Has(http2.FlagEndStream) {
			strm.SetState(http2.StreamStateHalfClosed)
		}
	case http2.StreamStateHalfClosed:
		if fr.Flags().Has(http2.FlagEndStream) {
			strm.SetState(http2.StreamStateClosed)
		} else if fr.Type() == http2.FrameResetStream {
			strm.SetState(http2.StreamStateClosed)
		}
	case http2.StreamStateClosed:
	}
}

func (c *Client) writeLoop() {
	ticker := time.NewTicker(time.Second * 3)
	defer ticker.Stop()

loop:
	for {
		select {
		case fr, ok := <-c.writer:
			if !ok {
				break loop
			}

			_, err := fr.WriteTo(c.bw)
			if err == nil {
				err = c.bw.Flush()
			}

			http2.ReleaseFrameHeader(fr)

			if err != nil {
				// TODO: handle errors
				return
			}
		case <-ticker.C:
			c.sendPing()
		}
	}
}

func (c *Client) handleStreams() {
	var strms http2.Streams

	defer func() {
		for _, strm := range strms.All() {
			close(strm.Data().(*reqRes).ch)
		}
	}()

	for {
		select {
		case rr := <-c.reqResCh: // request from the user
			strm := http2.NewStream(
				c.nextID, int(c.serverS.MaxWindowSize()))
			strm.SetData(rr)

			c.nextID += 2

			c.writeRequest(strm, rr.req)

			// https://datatracker.ietf.org/doc/html/rfc7540#section-8.1
			// writing the headers and/or the data makes the stream to become half-closed
			strm.SetState(http2.StreamStateHalfClosed)

			strms.Insert(strm)
		case fr, ok := <-c.inData: // response from the server
			if !ok {
				return
			}

			strm := strms.Get(fr.Stream())
			if strm == nil {
				panic("not found")
			}

			rr := strm.Data().(*reqRes)

			atomic.AddInt32(&c.currentWindow, -int32(fr.Len()))

			switch fr.Type() {
			case http2.FrameHeaders, http2.FrameContinuation:
				err := c.readHeaders(fr, rr)
				if err != nil {
					c.writeError(strm, err)
				}
			case http2.FrameData:
				data := fr.Body().(*http2.Data)
				if data.Len() > 0 {
					rr.res.AppendBody(data.Data())

					c.updateWindow(fr.Stream(), fr.Len())
				}

				myWin := atomic.LoadInt32(&c.currentWindow)
				if myWin < c.maxWindow/2 {
					nValue := c.maxWindow - myWin

					atomic.StoreInt32(&c.currentWindow, c.maxWindow)

					c.updateWindow(0, int(nValue))
				}
			case http2.FrameResetStream:
				rr.ch <- fr.Body().(*http2.RstStream).Error()
			}

			c.handleState(fr, strm)

			if strm.State() == http2.StreamStateClosed {
				close(rr.ch)
				strms.Del(strm.ID())
			}

			http2.ReleaseFrameHeader(fr)
		}
	}
}

func (c *Client) updateWindow(streamID uint32, n int) {
	fr := http2.AcquireFrameHeader()
	fr.SetStream(streamID)

	wu := http2.AcquireFrame(http2.FrameWindowUpdate).(*http2.WindowUpdate)
	wu.SetIncrement(n)

	fr.SetBody(wu)

	c.writer <- fr
}

func (c *Client) sendPing() {
	fr := http2.AcquireFrameHeader()

	ping := http2.AcquireFrame(http2.FramePing).(*http2.Ping)
	ping.SetCurrentTime()

	fr.SetBody(ping)

	c.writer <- fr
}

func (c *Client) handleSettings(st *http2.Settings) {
	st.CopyTo(&c.serverS)

	atomic.StoreInt32(&c.serverStreamWindow, int32(c.serverS.MaxWindowSize()))

	fr := http2.AcquireFrameHeader()

	stRes := http2.AcquireFrame(http2.FrameSettings).(*http2.Settings)
	stRes.SetAck(true)

	fr.SetBody(stRes)

	c.writer <- fr
}

func (c *Client) handlePing(p *http2.Ping) {
	if p.IsAck() {
		println(
			time.Now().Sub(p.DataAsTime()).String())
	} else {
		// TODO: reply back
	}
}

func (c *Client) writeRequest(strm *http2.Stream, req *fasthttp.Request) {
	hasBody := len(req.Body()) != 0

	// TODO: continue
	fr := http2.AcquireFrameHeader()
	fr.SetStream(strm.ID())

	h := http2.AcquireFrame(http2.FrameHeaders).(*http2.Headers)
	fr.SetBody(h)

	hf := http2.AcquireHeaderField()

	hf.SetBytes(http2.StringAuthority, req.URI().Host())
	c.enc.AppendHeaderField(h, hf, true)

	hf.SetBytes(http2.StringMethod, req.Header.Method())
	c.enc.AppendHeaderField(h, hf, true)

	hf.SetBytes(http2.StringPath, req.URI().RequestURI())
	c.enc.AppendHeaderField(h, hf, true)

	hf.SetBytes(http2.StringScheme, req.URI().Scheme())
	c.enc.AppendHeaderField(h, hf, true)

	hf.SetBytes(http2.StringUserAgent, req.Header.UserAgent())
	c.enc.AppendHeaderField(h, hf, true)

	req.Header.VisitAll(func(k, v []byte) {
		if bytes.EqualFold(k, http2.StringUserAgent) {
			return
		}

		hf.SetBytes(http2.ToLower(k), v)
		c.enc.AppendHeaderField(h, hf, false)
	})

	h.SetPadding(false)
	h.SetEndStream(!hasBody)
	h.SetEndHeaders(true)

	c.writer <- fr

	if hasBody { // has body
		fr = http2.AcquireFrameHeader()
		fr.SetStream(strm.ID())

		data := http2.AcquireFrame(http2.FrameData).(*http2.Data)

		// TODO: max length
		data.SetEndStream(true)
		data.SetData(req.Body())

		c.writer <- fr
	}
}

func (c *Client) readHeaders(fr *http2.FrameHeader, rr *reqRes) error {
	var err error
	res := rr.res

	h := fr.Body().(http2.FrameWithHeaders)
	b := h.Headers()

	hf := http2.AcquireHeaderField()
	defer http2.ReleaseHeaderField(hf)

	for len(b) > 0 {
		b, err = c.dec.Next(hf, b)
		if err != nil {
			return err
		}

		if hf.IsPseudo() {
			if hf.KeyBytes()[1] == 's' { // status
				n, err := strconv.ParseInt(hf.Value(), 10, 64)
				if err != nil {
					return err
				}

				res.SetStatusCode(int(n))
				continue
			}
		}

		if bytes.Equal(hf.KeyBytes(), http2.StringContentLength) {
			n, _ := strconv.Atoi(hf.Value())
			res.Header.SetContentLength(n)
		} else {
			res.Header.AddBytesKV(hf.KeyBytes(), hf.ValueBytes())
		}
	}

	return nil
}

func (c *Client) writeError(strm *http2.Stream, err error) {
	r := http2.AcquireFrame(http2.FrameResetStream).(*http2.RstStream)

	fr := http2.AcquireFrameHeader()
	fr.SetStream(strm.ID())
	fr.SetBody(r)

	if errors.Is(err, http2.Error{}) {
		r.SetCode(err.(http2.Error).Code())
	} else {
		r.SetCode(http2.InternalError)
	}

	c.writer <- fr
}
