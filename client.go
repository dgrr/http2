package http2

import (
	"bufio"
	"crypto/tls"
	"errors"
	"net"
	"sync/atomic"
	"time"
)

var (
	ErrServerSupport = errors.New("server doesn't support HTTP/2")
)

type Adaptor interface {
	SerializeRequest(strm *Stream, enc *HPACK, writer chan<- *FrameHeader)
	DeserializeResponse(fr *FrameHeader, dec *HPACK) error
	AppendBody(body []byte)
	Error(err error)
	Close()
}

type Client struct {
	c net.Conn

	br *bufio.Reader
	bw *bufio.Writer

	enc *HPACK // encoding hpack (ours)
	dec *HPACK // decoding hpack (server's)

	nextID uint32

	serverWindow       int32
	serverStreamWindow int32
	maxWindow          int32
	currentWindow      int32

	adptCh chan Adaptor

	writer chan *FrameHeader

	inData chan *FrameHeader

	st      Settings
	serverS Settings
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
		c:      tlsConn,
		br:     bufio.NewReader(tlsConn),
		bw:     bufio.NewWriter(tlsConn),
		writer: make(chan *FrameHeader, 128),
		adptCh: make(chan Adaptor, 128),
		inData: make(chan *FrameHeader, 128),
		enc:    AcquireHPACK(),
		dec:    AcquireHPACK(),
		nextID: 1,
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
	err := WritePreface(c.bw)
	if err == nil {
		err = c.bw.Flush()
	}

	if err != nil {
		return err
	}

	// TODO: Make an option
	c.st.SetMaxWindowSize(1 << 16) // 65536
	c.st.SetPush(false)            // do not support push promises

	fr := AcquireFrameHeader()
	defer ReleaseFrameHeader(fr)

	fr.SetBody(&c.st)

	_, err = fr.WriteTo(c.bw)
	if err == nil {
		err = c.bw.Flush()
	}

	return nil
}

func (c *Client) Register(adaptr Adaptor) {
	c.adptCh <- adaptr
}

func (c *Client) readLoop() {
	defer func() {
		// TODO: fix race conditions
		close(c.writer)
		close(c.inData)
	}()

	for {
		fr, err := ReadFrameFrom(c.br)
		if err != nil {
			// TODO: handle
			panic(err)
		}

		if fr.Stream() != 0 {
			c.inData <- fr
			continue
		}

		switch fr.Type() {
		case FrameSettings:
			st := fr.Body().(*Settings)
			if !st.IsAck() { // if has ack, just ignore
				c.handleSettings(st)
			}
		case FrameWindowUpdate:
			win := int32(fr.Body().(*WindowUpdate).Increment())

			if !atomic.CompareAndSwapInt32(&c.serverWindow, 0, win) {
				atomic.AddInt32(&c.serverWindow, win)
			}
		case FramePing:
			c.handlePing(fr.Body().(*Ping))
		case FrameGoAway:
			println(
				fr.Body().(*GoAway).Code().Error())
			c.c.Close()
			return
		}

		ReleaseFrameHeader(fr)
	}
}

func (c *Client) handleState(fr *FrameHeader, strm *Stream) {
	switch strm.State() {
	case StreamStateIdle:
		if fr.Type() == FrameHeaders {
			strm.SetState(StreamStateOpen)
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

			ReleaseFrameHeader(fr)

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
	var strms Streams

	defer func() {
		for _, strm := range strms.All() {
			strm.Data().(Adaptor).Close()
		}
	}()

	defer func() {
		close(c.adptCh)
	}()

	for {
		select {
		case adpt := <-c.adptCh: // request from the user
			strm := NewStream(
				c.nextID, int(c.serverS.MaxWindowSize()))
			strm.SetData(adpt)

			c.nextID += 2

			adpt.SerializeRequest(strm, c.enc, c.writer)

			// https://datatracker.ietf.org/doc/html/rfc7540#section-8.1
			// writing the headers and/or the data makes the stream to become half-closed
			strm.SetState(StreamStateHalfClosed)

			strms.Insert(strm)
		case fr, ok := <-c.inData: // response from the server
			if !ok {
				return
			}

			strm := strms.Get(fr.Stream())
			if strm == nil {
				panic("not found")
			}

			adapt := strm.Data().(Adaptor)

			atomic.AddInt32(&c.currentWindow, -int32(fr.Len()))

			switch fr.Type() {
			case FrameHeaders, FrameContinuation:
				err := adapt.DeserializeResponse(fr, c.dec)
				if err != nil {
					c.writeError(strm, err)
				}
			case FrameData:
				data := fr.Body().(*Data)
				if data.Len() > 0 {
					adapt.AppendBody(data.Data())

					c.updateWindow(fr.Stream(), fr.Len())
				}

				myWin := atomic.LoadInt32(&c.currentWindow)
				if myWin < c.maxWindow/2 {
					nValue := c.maxWindow - myWin

					atomic.StoreInt32(&c.currentWindow, c.maxWindow)

					c.updateWindow(0, int(nValue))
				}
			case FrameResetStream:
				adapt.Error(fr.Body().(*RstStream).Error())
			}

			c.handleState(fr, strm)

			if strm.State() == StreamStateClosed {
				adapt.Close()
				strms.Del(strm.ID())
			}

			ReleaseFrameHeader(fr)
		}
	}
}

func (c *Client) updateWindow(streamID uint32, n int) {
	fr := AcquireFrameHeader()
	fr.SetStream(streamID)

	wu := AcquireFrame(FrameWindowUpdate).(*WindowUpdate)
	wu.SetIncrement(n)

	fr.SetBody(wu)

	c.writer <- fr
}

func (c *Client) sendPing() {
	fr := AcquireFrameHeader()

	ping := AcquireFrame(FramePing).(*Ping)
	ping.SetCurrentTime()

	fr.SetBody(ping)

	c.writer <- fr
}

func (c *Client) handleSettings(st *Settings) {
	st.CopyTo(&c.serverS)

	atomic.StoreInt32(&c.serverStreamWindow, int32(c.serverS.MaxWindowSize()))

	fr := AcquireFrameHeader()

	stRes := AcquireFrame(FrameSettings).(*Settings)
	stRes.SetAck(true)

	fr.SetBody(stRes)

	c.writer <- fr
}

func (c *Client) handlePing(p *Ping) {
	if p.IsAck() {
		println(
			time.Now().Sub(p.DataAsTime()).String())
	} else {
		// TODO: reply back
	}
}

func (c *Client) writeError(strm *Stream, err error) {
	r := AcquireFrame(FrameResetStream).(*RstStream)

	fr := AcquireFrameHeader()
	fr.SetStream(strm.ID())
	fr.SetBody(r)

	if errors.Is(err, Error{}) {
		r.SetCode(err.(Error).Code())
	} else {
		r.SetCode(InternalError)
	}

	c.writer <- fr
}
