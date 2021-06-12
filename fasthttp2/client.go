package fasthttp2

import (
	"bufio"
	"crypto/tls"
	"errors"
	"net"
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

	nextID uint32

	reqResCh chan *reqRes
	writer   chan *http2.FrameHeader
}

type reqRes struct {
	req *fasthttp.Request
	res *fasthttp.Response
	ch  chan struct{}
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
		c:  tlsConn,
		br: bufio.NewReader(tlsConn),
		bw: bufio.NewWriter(tlsConn),
	}

	if err := cl.Handshake(); err != nil {
		c.Close()
		return nil, err
	}

	go cl.readLoop()
	go cl.writeLoop()

	return cl, nil
}

func (c *Client) Handshake() error {
	// TODO: ...
}

func (c *Client) Do(req *fasthttp.Request, res *fasthttp.Response) error {
	nextID := atomic.AddUint32(&c.nextID, 2)

	return nil
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
		}

		switch fr.Type() {
		}

		// c.writer <-
	}
}

func (c *Client) writeLoop() {
	ticker := time.NewTicker(time.Second * 3)
	defer ticker.Stop()

loop:
	for {
		select {
		case reqRes, ok := <-c.writer:
			if !ok {
				break loop
			}
		case <-ticker.C:
			// TODO: ping
		}
	}
}
