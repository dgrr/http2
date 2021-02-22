package http2

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/valyala/bytebufferpool"
	"github.com/valyala/fasthttp"
)

type clientPool struct {
	lck  sync.Mutex
	idle []*ClientStream
}

func (cp *clientPool) Get(id uint32) (strm *ClientStream) {
	cp.lck.Lock()
	defer cp.lck.Unlock()

	if len(cp.idle) == 0 {
		strm = &ClientStream{}
		initClientStream(strm)
	} else {
		strm = cp.idle[len(cp.idle)-1]
		cp.idle = cp.idle[:len(cp.idle)-1]
	}

	strm.id = id

	return
}

func (cp *clientPool) Put(strm *ClientStream) {
	cp.lck.Lock()
	cp.idle = append(cp.idle, strm)
	cp.lck.Unlock()
}

type ClientStream struct {
	id    uint32
	state StreamState

	writer chan<- *Frame
	reader chan *Frame
	err    chan error
}

func (strm *ClientStream) Close() {
	close(strm.reader)
	close(strm.err)
}

var clientStreamPool clientPool

func initClientStream(strm *ClientStream) {
	strm.id = 0
	strm.state = StateIdle
	strm.reader = make(chan *Frame, 1)
	strm.err = make(chan error, 1)
}

func acquireClientStream(id uint32) *ClientStream {
	return clientStreamPool.Get(id)
}

func releaseClientStream(strm *ClientStream) {
	clientStreamPool.Put(strm)
}

type Client struct {
	lck sync.Mutex

	c  net.Conn
	br *bufio.Reader
	bw *bufio.Writer

	enc *HPACK
	dec *HPACK

	p      *fasthttp.HostClient
	nextID uint32

	st *Settings

	writer chan *Frame

	enableCompression bool

	closer chan struct{}
	strms  sync.Map
}

func NewClient() *Client {
	return &Client{
		nextID: 1,
		enc:    AcquireHPACK(),
		dec:    AcquireHPACK(),
		st:     AcquireSettings(),
		writer: make(chan *Frame, 1024),
		closer: make(chan struct{}, 1),
	}
}

// TODO: Fix a leak that happens when a request still processing but the server is closing.
func (c *Client) Close() error {
	close(c.closer)

	err := c.c.Close()

	c.releaseStreams()
	ReleaseHPACK(c.enc)
	ReleaseHPACK(c.dec)
	ReleaseSettings(c.st)

	close(c.writer)

	return err
}

func (c *Client) releaseStreams() {
	c.strms.Range(func(k, v interface{}) bool {
		v.(*ClientStream).Close()
		return true
	})
	c.strms = sync.Map{}
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

	state := conn.ConnectionState()
	if p := state.NegotiatedProtocol; p != "h2" {
		return fmt.Errorf("server doesn't support HTTP/2. Proto %s <> h2", p)
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
			if st.IsAck() {
				ReleaseFrame(fr)
			} else {
				c.enc.SetMaxTableSize(int(st.HeaderTableSize()))

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
			ReleaseFrame(fr)
		case FramePing:
			if !fr.HasFlag(FlagAck) {
				fr.AddFlag(FlagAck)
				c.writeFrame(fr)
			} else {
				pfr := AcquirePing()
				pfr.ReadFrame(fr)

				ts := binary.BigEndian.Uint64(pfr.Data())
				// TODO: Convert to callback
				fmt.Println("HTTP/2 Ping:", time.Now().Sub(time.Unix(0, int64(ts))))

				ReleasePing(pfr)
				ReleaseFrame(fr)
			}
		default:
			strm := c.getStream(fr.Stream())
			if strm != nil {
				strm.reader <- fr
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

var defaultPingTimeout = time.Second * 5

func (c *Client) writeLoop() {
	defer func() {
		c.lck.Lock()
		if c.c != nil {
			c.c.Close()
		}
		c.lck.Unlock()
	}()

	var err error
	expectedID := uint32(1)
	timer := time.NewTimer(defaultPingTimeout)

	// if the writer is full, then we use the buffered
	buffered := make(chan *Frame, 128)

loop:
	for err == nil {
		select {
		case fr, ok := <-c.writer:
			if !ok {
				break loop
			}

			if fr.Stream() != 0 && expectedID < fr.Stream() {
				select {
				case c.writer <- fr:
				default:
					buffered <- fr
				}
				continue
			}

			if fr.Stream() != 0 {
				expectedID = fr.Stream() + 2
			}

			_, err = fr.WriteTo(c.bw)
			if err == nil {
				err = c.bw.Flush()
			}

			if err != nil {
				strm := c.getStream(fr.Stream())
				if strm != nil {
					strm.err <- err
				} else {
					log.Println("writeLoop", err)
				}
			}

			timer.Stop()
			timer.Reset(defaultPingTimeout)

			ReleaseFrame(fr)
		case fr := <-buffered:
			select {
			case c.writer <- fr:
			default:
				buffered <- fr
			}
		case <-timer.C:
			fr := AcquireFrame()
			pfr := AcquirePing()
			// write current timestamp
			binary.BigEndian.PutUint64(pfr.data[:], uint64(time.Now().UnixNano()))

			pfr.WriteFrame(fr)
			ReleasePing(pfr)

			_, err = fr.WriteTo(c.bw)
			ReleaseFrame(fr)

			timer.Reset(defaultPingTimeout)
		case <-c.closer:
			break loop
		}
	}

	close(buffered)

	if err != nil {
		log.Println("writeLoop", err)
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

	id := c.nextID
	c.nextID += 2

	c.lck.Unlock()

	strm := acquireClientStream(id)

	c.strms.Store(strm.id, strm)

	c.writeRequest(strm, req)
	err := c.readResponse(strm, res)
	if strm.state == StateHalfClosed {
		c.writeReset(strm.id)
	}

	if c.enableCompression {
		encoding := res.Header.Peek("Content-Encoding")
		if len(encoding) > 0 {
			n := 0
			bb := bytebufferpool.Get()
			switch encoding[0] {
			case 'b':
				n, err = fasthttp.WriteUnbrotli(bb, res.Body())
			case 'd':
				n, err = fasthttp.WriteInflate(bb, res.Body())
			case 'g':
				n, err = fasthttp.WriteGunzip(bb, res.Body())
			}
			if n > 0 {
				res.SetBody(bb.B)
			}
			bytebufferpool.Put(bb)
		}
	}

	c.strms.Delete(strm.id)
	releaseClientStream(strm)

	return err
}

func (c *Client) writeRequest(strm *ClientStream, req *fasthttp.Request) {
	// TODO: Send continuation if needed
	hasBody := len(req.Body()) > 0

	if c.enableCompression {
		req.Header.Set("Accept-Encoding", "gzip, deflate, br")
	}

	fr := AcquireFrame()
	h := AcquireHeaders()
	hf := AcquireHeaderField()

	defer ReleaseHeaders(h)
	defer ReleaseHeaderField(hf)

	c.lck.Lock()
	hf.SetBytes(strAuthority, req.URI().Host())
	h.rawHeaders = c.enc.AppendHeader(h.rawHeaders, hf, true)

	hf.SetBytes(strMethod, req.Header.Method())
	h.rawHeaders = c.enc.AppendHeader(h.rawHeaders, hf, true)

	hf.SetBytes(strPath, req.URI().RequestURI())
	h.rawHeaders = c.enc.AppendHeader(h.rawHeaders, hf, true)

	hf.SetBytes(strScheme, req.URI().Scheme())
	h.rawHeaders = c.enc.AppendHeader(h.rawHeaders, hf, true)

	hf.SetBytes(strUserAgent, req.Header.UserAgent())
	h.rawHeaders = c.enc.AppendHeader(h.rawHeaders, hf, true)

	req.Header.VisitAll(func(k, v []byte) {
		if bytes.EqualFold(k, strUserAgent) {
			return
		}

		hf.SetBytes(toLower(k), v)
		h.rawHeaders = c.enc.AppendHeader(h.rawHeaders, hf, false)
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
	// then setting the END_STREAM sets the connection to half-closed state.
	strm.state = StateHalfClosed
}

func (c *Client) writeFrame(fr *Frame) {
	c.writer <- fr
}

func (c *Client) readResponse(strm *ClientStream, res *fasthttp.Response) error {
	var (
		fr  *Frame
		ok  bool
		err error
	)

	for strm.state != StateClosed {
		select {
		case fr, ok = <-strm.reader:
		case err, ok = <-strm.err:
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
		b, err = c.dec.Next(hf, b)
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
