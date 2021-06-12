package fasthttp2

import (
	"bufio"
	"crypto/tls"
	"errors"
	"net"
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

	nextID    uint32
	maxWindow int

	reqResCh chan reqRes
	writer   chan *http2.FrameHeader
	winUpCh  chan int

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
		winUpCh:  make(chan int, 1),
		reqResCh: make(chan reqRes, 128),
	}

	if err := cl.Handshake(); err != nil {
		c.Close()
		return nil, err
	}

	go cl.readLoop()
	go cl.writeLoop()
	go cl.handleReqs()

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
	rr := reqRes{
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
	}()

	for {
		fr, err := http2.ReadFrameFrom(c.br)
		if err != nil {
			// TODO: handle
			panic(err)
		}

		println("-", fr.Type().String(), fr.Len()+http2.DefaultFrameSize)

		switch fr.Type() {
		case http2.FrameSettings:
			st := fr.Body().(*http2.Settings)
			if !st.IsAck() { // if has ack, just ignore
				c.handleSettings(st)
			}
			println(st.IsAck())
		case http2.FrameHeaders:
			c.handleHeaders(fr.Body().(*http2.Headers))
		//
		case http2.FrameData:
			c.handleData(fr.Body().(*http2.Data))
		//
		case http2.FrameContinuation:
		//
		// TODO:
		case http2.FrameWindowUpdate:
			c.winUpCh <- fr.Body().(*http2.WindowUpdate).Increment()
		case http2.FramePing:
			c.handlePing(fr.Body().(*http2.Ping))
		case http2.FrameGoAway:
			println(
				fr.Body().(*http2.GoAway).Code().Error())
		}

		// c.updateWindow(fr.Len() + http2.DefaultFrameSize)

		http2.ReleaseFrameHeader(fr)
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
		case incr := <-c.winUpCh:
			if incr == 0 {
				c.maxWindow = 0
			} else {
				c.maxWindow += incr
			}

			println("win update", c.maxWindow)
		case <-ticker.C:
			c.sendPing()
		}
	}
}

func (c *Client) handleReqs() {
	for reqRes := range c.reqResCh {
		_ = reqRes
		// TODO: ...
	}
}

func (c *Client) updateWindow(n int) {
	fr := http2.AcquireFrameHeader()

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

	c.winUpCh <- 0
	c.winUpCh <- int(c.serverS.MaxWindowSize())

	fr := http2.AcquireFrameHeader()

	stRes := http2.AcquireFrame(http2.FrameSettings).(*http2.Settings)
	stRes.SetAck(true)

	fr.SetBody(stRes)

	c.writer <- fr
}

func (c *Client) handleHeaders(hdr *http2.Headers) {

}

func (c *Client) handleData(data *http2.Data) {

}

func (c *Client) handlePing(p *http2.Ping) {
	if p.IsAck() {
		println(
			time.Now().Sub(p.DataAsTime()).String())
	} else {
		// TODO: reply back
	}
}
