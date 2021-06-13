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
	streamID uint32
	req      *fasthttp.Request
	res      *fasthttp.Response
	ch       chan error
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
	go cl.handleReqs()

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

		switch fr.Type() {
		case http2.FrameHeaders, http2.FrameData, http2.FrameContinuation:
			c.inData <- fr
		case http2.FrameSettings:
			st := fr.Body().(*http2.Settings)
			if !st.IsAck() { // if has ack, just ignore
				c.handleSettings(st)
			}

			http2.ReleaseFrameHeader(fr)
		case http2.FrameWindowUpdate:
			win := int32(fr.Body().(*http2.WindowUpdate).Increment())

			if fr.Stream() == 0 {
				if !atomic.CompareAndSwapInt32(&c.serverWindow, 0, win) {
					atomic.AddInt32(&c.serverWindow, win)
				}
			} else {
				// TODO: need to increment stream ...
			}

			http2.ReleaseFrameHeader(fr)
		case http2.FramePing:
			c.handlePing(fr.Body().(*http2.Ping))
			http2.ReleaseFrameHeader(fr)
		case http2.FrameGoAway:
			println(
				fr.Body().(*http2.GoAway).Code().Error())
			http2.ReleaseFrameHeader(fr)
		}
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

func (c *Client) handleReqs() {
	rrs := make([]*reqRes, 0, 8) // requests awaiting

	defer func() {
		for _, rr := range rrs {
			close(rr.ch)
		}
	}()

	getReqRes := func(id uint32) *reqRes {
		for _, rr := range rrs {
			if rr.streamID == id {
				return rr
			}
		}

		return nil
	}

	delReqRes := func(id uint32) {
		for i, rr := range rrs {
			if rr.streamID == id {
				rrs = append(rrs[:i], rrs[i+1:]...)
				break
			}
		}
	}

	for {
		select {
		case rr := <-c.reqResCh: // request from the user
			rr.streamID = c.nextID
			c.nextID += 2

			c.writeRequest(rr)

			rrs = append(rrs, rr)
		case fr, ok := <-c.inData: // response from the server
			if !ok {
				return
			}

			rr := getReqRes(fr.Stream())
			if rr == nil {
				panic("not found")
			}

			atomic.AddInt32(&c.currentWindow, -int32(fr.Len()))

			var (
				err error
			)

			println("-", fr.Stream(), fr.Type().String(), fr.Len())

			switch fr.Type() {
			case http2.FrameHeaders:
				err = c.readHeaders(fr, rr)
			case http2.FrameContinuation:
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
			}

			if err != nil {
				panic(err)
			}

			if fr.Flags().Has(http2.FlagEndStream) {
				close(rr.ch)
				delReqRes(rr.streamID)
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

func (c *Client) writeRequest(rr *reqRes) {
	req := rr.req

	hasBody := len(req.Body()) != 0

	// TODO: continue
	fr := http2.AcquireFrameHeader()
	fr.SetStream(rr.streamID)

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
		fr.SetStream(rr.streamID)

		data := http2.AcquireFrame(http2.FrameData).(*http2.Data)

		// TODO: max length
		data.SetEndStream(true)
		data.SetData(req.Body())

		c.writer <- fr
	}

	// sending a frame headers sets the stream to the `open` state
	// then setting the END_STREAM sets the connection to half-closed state.
	// strm.state = StateHalfClosed
}

func (c *Client) readHeaders(fr *http2.FrameHeader, rr *reqRes) error {
	var err error

	hf := http2.AcquireHeaderField()
	defer http2.ReleaseHeaderField(hf)

	res := rr.res

	h := fr.Body().(*http2.Headers)
	b := h.Headers()

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
