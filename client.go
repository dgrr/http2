package http2

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"log"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/valyala/bytebufferpool"
	"github.com/valyala/fasthttp"
)

var clientStreamPool = sync.Pool{
	New: func() interface{} {
		return &ClientStream{}
	},
}

type ClientStream struct {
	id    uint32
	state StreamState

	windowSize uint32

	ch    chan *Frame
	errch chan error
}

func acquireClientStream(id uint32) *ClientStream {
	strm := clientStreamPool.Get().(*ClientStream)

	strm.id = id
	strm.state = StateIdle
	strm.ch = make(chan *Frame, 1)
	strm.errch = make(chan error, 1)

	return strm
}

func releaseClientStream(strm *ClientStream) {
	defer func() {
		recover()
	}()

	close(strm.ch)
	close(strm.errch)
	streamPool.Put(strm)
}

type Client struct {
	lck sync.Mutex

	c  net.Conn
	br *bufio.Reader
	bw *bufio.Writer
	hp *HPACK

	p      *fasthttp.HostClient
	nextID uint32

	st *Settings

	wch chan *Frame
	rch chan *Frame

	enableCompression bool

	closer chan struct{}
	strms  sync.Map
}

func NewClient() *Client {
	return &Client{
		nextID: 1,
		hp:     AcquireHPACK(),
		st:     AcquireSettings(),
		wch:    make(chan *Frame, 1024),
		rch:    make(chan *Frame, 1024),
		closer: make(chan struct{}, 1),
	}
}

func (c *Client) Close() error {
	close(c.closer)
	c.releaseStreams()
	ReleaseHPACK(c.hp)
	ReleaseSettings(c.st)
	close(c.wch)
	close(c.rch)
	return nil
}

func (c *Client) releaseStreams() {
	c.strms.Range(func(k, v interface{}) bool {
		releaseClientStream(v.(*ClientStream))
		return true
	})
}

type Options int8

const (
	OptionEnableCompression Options = iota
)

// TODO: checkout https://github.com/golang/net/blob/4acb7895a057/http2/transport.go#L570
func ConfigureClient(c *fasthttp.HostClient, opts ...Options) error {
	c2 := NewClient()
	if c.TLSConfig == nil {
		c.TLSConfig = &tls.Config{
			MaxVersion: tls.VersionTLS13,
			MinVersion: tls.VersionTLS12,
		}
	}
	c.TLSConfig.NextProtos = append(c.TLSConfig.NextProtos, "h2")

	err := c2.Dial(c.Addr, c.TLSConfig)
	if err != nil {
		return err
	}

	// TODO: Checkout the tlsconfig....

	for _, opt := range opts {
		switch opt {
		case OptionEnableCompression:
			c2.enableCompression = true
		}
	}

	c.Transport = c2.Do

	return nil
}

func (c *Client) Dial(addr string, tlsConfig *tls.Config) error {
	conn, err := tls.Dial("tcp", addr, tlsConfig)
	if err != nil {
		return err
	}

	err = conn.Handshake()
	if err != nil {
		conn.Close()
		return err
	}

	c.c = conn
	c.br = bufio.NewReader(conn)
	c.bw = bufio.NewWriter(conn)

	err = c.Handshake()
	if err != nil {
		conn.Close()
		return err
	}

	go c.readLoop()
	go c.writeLoop()

	c.writeWindowUpdate(c.st.MaxWindowSize())

	return nil
}

func (c *Client) Handshake() error {
	_, err := c.bw.Write(http2Preface)
	if err == nil {
		err = c.bw.Flush()
	}
	if err != nil {
		return err
	}

	fr := AcquireFrame()
	defer ReleaseFrame(fr)

	c.st.SetMaxWindowSize(1 << 22)
	c.st.WriteFrame(fr)

	_, err = fr.WriteTo(c.bw)
	if err == nil {
		err = c.bw.Flush()
	}

	return err
}

func (c *Client) getStream(id uint32) *ClientStream {
	ci, ok := c.strms.Load(id)
	if ok {
		return ci.(*ClientStream)
	}
	return nil
}

func (c *Client) readLoop() {
	defer c.Close()

	for {
		fr := AcquireFrame()

		_, err := fr.ReadFrom(c.br)
		if err != nil {
			log.Println("readLoop", err)
			return
		}

		switch fr.Type() {
		case FrameSettings:
			st := AcquireSettings()
			st.ReadFrame(fr)
			if !st.IsAck() {
				c.hp.SetMaxTableSize(int(st.HeaderTableSize()))

				st.Reset()
				fr.Reset()

				st.SetAck(true)
				st.WriteFrame(fr)

				st.WriteFrame(fr)
				c.writeFrame(fr)

				ReleaseSettings(st)
				fr = nil
			}
		case FrameWindowUpdate:
			ReleaseFrame(fr)
		case FrameGoAway:
			c.handleGoAway(fr)
		default:
			strm := c.getStream(fr.Stream())
			if strm != nil {
				strm.ch <- fr
			} else {
				log.Println(fr.Stream(), "not found", fr.Type())
				ReleaseFrame(fr)
			}
		}
	}
}

func (c *Client) handleGoAway(fr *Frame) {
	ga := AcquireGoAway()
	defer ReleaseGoAway(ga)

	ga.ReadFrame(fr)

	log.Printf("%d: %s\n", ga.Code(), ga.Data())
}

func (c *Client) writeLoop() {
	defer c.c.Close()

	for {
		select {
		case fr, ok := <-c.wch:
			if !ok {
				return
			}

			_, err := fr.WriteTo(c.bw)
			if err == nil {
				err = c.bw.Flush()
			}
			if err != nil {
				strm := c.getStream(fr.Stream())
				if strm != nil {
					strm.errch <- err
				} else {
					log.Println("writeLoop", err)
				}
			}

			ReleaseFrame(fr)
		case <-time.After(time.Second * 5):
		// TODO: PING ...
		case <-c.closer:
			return
		}
	}
}

func (c *Client) Do(req *fasthttp.Request, res *fasthttp.Response) error {
	c.lck.Lock()
	if c.c == nil {
		err := c.Dial(c.p.Addr, c.p.TLSConfig)
		if err != nil {
			c.lck.Unlock()
			return err
		}
	}
	c.lck.Unlock()

	strm := acquireClientStream(c.nextID)
	atomic.AddUint32(&c.nextID, 2)

	c.strms.Store(strm.id, strm)

	c.writeRequest(strm, req)
	err := c.readResponse(strm, res)
	if strm.state == StateHalfClosed {
		c.writeReset(strm.id)
	}

	if c.enableCompression {
		encoding := res.Header.Peek("Content-Encoding")
		if bytes.Contains(encoding, strGzip) {
			bb := bytebufferpool.Get()
			n, err := fasthttp.WriteGunzip(bb, res.Body())
			if err != nil {
				return err
			}
			res.SetBody(bb.B[:n])
			bytebufferpool.Put(bb)
		}
	}

	// TODO: remove strm from slice
	releaseClientStream(strm)

	return err
}

func (c *Client) writeRequest(strm *ClientStream, req *fasthttp.Request) {
	// TODO: Send continuation if needed
	hasBody := len(req.Body()) > 0

	if c.enableCompression {
		req.Header.Set("Accept-Encoding", "gzip")
	}

	fr := AcquireFrame()
	h := AcquireHeaders()
	hf := AcquireHeaderField()

	defer ReleaseHeaders(h)
	defer ReleaseHeaderField(hf)

	c.lck.Lock()
	hf.SetBytes(strAuthority, req.URI().Host())
	h.rawHeaders = c.hp.AppendHeader(h.rawHeaders, hf)

	hf.SetBytes(strMethod, req.Header.Method())
	h.rawHeaders = c.hp.AppendHeader(h.rawHeaders, hf)

	hf.SetBytes(strScheme, req.URI().Scheme())
	h.rawHeaders = c.hp.AppendHeader(h.rawHeaders, hf)

	hf.SetBytes(strPath, req.URI().RequestURI())
	h.rawHeaders = c.hp.AppendHeader(h.rawHeaders, hf)

	req.Header.VisitAll(func(k, v []byte) {
		hf.SetBytes(toLower(k), v)
		h.rawHeaders = c.hp.AppendHeader(h.rawHeaders, hf)
	})
	c.lck.Unlock()

	h.SetPadding(false)
	h.SetEndStream(!hasBody)
	h.SetEndHeaders(true)

	h.WriteFrame(fr)
	fr.SetStream(strm.id)

	c.writeFrame(fr)

	if hasBody { // has body
		fr = AcquireFrame() // shadow

		dfr := AcquireData()
		defer ReleaseData(dfr)

		dfr.SetEndStream(true)
		dfr.SetData(req.Body())

		fr.SetStream(strm.id)

		dfr.WriteFrame(fr)

		c.writeFrame(fr)
	}

	// sending a frame headers sets the stream to the `open` state
	strm.state = StateHalfClosed
}

func (c *Client) writeFrame(fr *Frame) {
	c.wch <- fr
}

func (c *Client) readResponse(strm *ClientStream, res *fasthttp.Response) error {
	var (
		fr  *Frame
		ok  bool
		err error
	)

	for strm.state != StateClosed {
		select {
		case fr, ok = <-strm.ch:
		case err, ok = <-strm.errch:
			return err
		}
		if !ok {
			break
		}

		var isEnd bool
		switch fr.Type() {
		case FrameHeaders:
			isEnd, err = c.handleHeaders(fr, res)
		case FrameData:
			isEnd, err = c.handleData(fr, res)
		}

		ReleaseFrame(fr)

		if err != nil {
			return err
		}

		if isEnd {
			strm.state++
		}
	}

	return err
}

func (c *Client) writeWindowUpdate(update uint32) {
	fr := AcquireFrame()
	wu := AcquireWindowUpdate()
	wu.SetIncrement(update)

	wu.WriteFrame(fr)
	c.writeFrame(fr)

	ReleaseWindowUpdate(wu)
}

func (c *Client) writeReset(id uint32) {
	fr := AcquireFrame()
	rst := AcquireRstStream()

	rst.SetCode(0)

	rst.WriteFrame(fr)
	fr.SetStream(id)

	c.writeFrame(fr)
}

func (c *Client) handleHeaders(fr *Frame, res *fasthttp.Response) (bool, error) {
	h := AcquireHeaders()
	hf := AcquireHeaderField()

	defer ReleaseHeaders(h)
	defer ReleaseHeaderField(hf)

	err := h.ReadFrame(fr)
	if err != nil {
		return false, err
	}

	b := fr.Payload()

	c.lck.Lock()
	for len(b) > 0 {
		b, err = c.hp.Next(hf, b)
		if err != nil {
			return false, err
		}

		if hf.IsPseudo() {
			if hf.key[1] == 's' { // status
				n, err := strconv.ParseInt(hf.Value(), 10, 64)
				if err != nil {
					return false, err
				}

				res.SetStatusCode(int(n))
				continue
			}
		}

		if bytes.Equal(hf.KeyBytes(), strContentLength) {
			n, _ := strconv.Atoi(hf.Value())
			res.Header.SetContentLength(n)
		} else {
			res.Header.AddBytesKV(hf.KeyBytes(), hf.ValueBytes())
		}
	}
	c.lck.Unlock()

	return h.EndStream(), err
}

func (c *Client) handleData(fr *Frame, res *fasthttp.Response) (bool, error) {
	dfr := AcquireData()
	defer ReleaseData(dfr)

	err := dfr.ReadFrame(fr)
	if err != nil {
		return false, err
	}

	if dfr.Len() > 0 {
		res.AppendBody(dfr.Data())
		c.writeWindowUpdate(dfr.Len())
	}

	return dfr.EndStream(), nil
}
