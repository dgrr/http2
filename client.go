package http2

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"time"
)

var (
	ErrServerSupport = errors.New("server doesn't support HTTP/2")
)

// ClientAdaptor ...
type ClientAdaptor interface {
	// Write ...
	Write(uint32, *HPACK, chan<- *FrameHeader)
	// Read ...
	Read(fr *FrameHeader, dec *HPACK) error
	// AppendBody ...
	AppendBody(body []byte)
	// Error ...
	Error(err error)
	// Close ...
	Close()
}

// Client ...
type Client struct {
	c net.Conn

	br *bufio.Reader
	bw *bufio.Writer

	enc *HPACK // encoding hpack (ours)
	dec *HPACK // decoding hpack (server's)

	nextID uint32

	// server's window
	serverWindow       int32
	serverStreamWindow int32
	// my window
	maxWindow     int32
	currentWindow int32

	adptCh chan ClientAdaptor

	writer chan *FrameHeader

	inData chan *FrameHeader

	openStreams int32

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
		adptCh: make(chan ClientAdaptor, 128),
		inData: make(chan *FrameHeader, 128),
		enc:    AcquireHPACK(),
		dec:    AcquireHPACK(),
		nextID: 1,
	}

	cl.maxWindow = 1 << 20
	cl.currentWindow = cl.maxWindow

	// TODO: Make an option
	cl.st.SetMaxWindowSize(1 << 20) // 1MB
	cl.st.SetPush(false)            // do not support push promises

	if err := Handshake(true, cl.bw, &cl.st, cl.maxWindow-65536); err != nil {
		c.Close()
		return nil, err
	}

	fr, err := ReadFrameFrom(cl.br)
	if fr.Type() != FrameSettings {
		c.Close()
		return nil, fmt.Errorf("unexpected frame, expected settings, got %s", fr.Type())
	}

	go cl.writeLoop()

	st := fr.Body().(*Settings)
	if !st.IsAck() { // if has ack, just ignore
		cl.handleSettings(st)
	}

	go cl.readLoop()
	go cl.handleStreams()

	return cl, nil
}

func Handshake(preface bool, bw *bufio.Writer, st *Settings, maxWin int32) error {
	if preface {
		err := WritePreface(bw)
		if err != nil {
			return err
		}
	}

	fr := AcquireFrameHeader()
	defer ReleaseFrameHeader(fr)

	// write the settings
	st2 := &Settings{}
	st.CopyTo(st2)

	fr.SetBody(st2)

	_, err := fr.WriteTo(bw)
	if err == nil {
		// then send a window update
		fr = AcquireFrameHeader()
		wu := AcquireFrame(FrameWindowUpdate).(*WindowUpdate)
		wu.SetIncrement(int(maxWin))

		fr.SetBody(wu)

		_, err = fr.WriteTo(bw)
		if err == nil {
			err = bw.Flush()
		}
	}

	return err
}

func (c *Client) Register(adaptr ClientAdaptor) {
	c.adptCh <- adaptr
}

func (c *Client) CanOpenStream() bool {
	return atomic.LoadInt32(&c.openStreams) < int32(c.serverS.maxStreams)
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
			c.inData <- fr
			c.c.Close()
			return
		}

		ReleaseFrameHeader(fr)
	}
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
			if err == nil || len(c.writer) == 0 {
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
	var strms = make(map[uint32]*Stream)

	defer func() {
		close(c.adptCh)
	}()

	var lastErr error

	defer func() {
		for _, strm := range strms {
			if lastErr != nil {
				strm.Data().(ClientAdaptor).Error(lastErr)
			}
			strm.Data().(ClientAdaptor).Close()
		}
	}()

loop:
	for {
		select {
		case adpt := <-c.adptCh: // request from the user
			atomic.AddInt32(&c.openStreams, 1)

			strm := NewStream(
				c.nextID, int32(c.serverS.MaxWindowSize()))
			strm.SetData(adpt)

			c.nextID += 2

			adpt.Write(strm.ID(), c.enc, c.writer)

			// https://datatracker.ietf.org/doc/html/rfc7540#section-8.1
			// writing the headers and/or the data makes the stream to become half-closed
			strm.SetState(StreamStateHalfClosed)

			strms[strm.ID()] = strm
		case fr, ok := <-c.inData: // response from the server
			if !ok {
				return
			}

			if fr.Type() == FrameGoAway {
				lastErr = fr.Body().(*GoAway).Code()
				break loop
			}

			strm, ok := strms[fr.Stream()]
			if !ok {
				writeReset(
					fr.Stream(), ProtocolError, c.writer)
				continue
			}

			adapt := strm.Data().(ClientAdaptor)

			switch fr.Type() {
			case FrameHeaders, FrameContinuation:
				err := adapt.Read(fr, c.dec)
				if err != nil {
					writeError(strm, err, c.writer)
				}
			case FrameData:
				// it's safe to modify the win here
				c.currentWindow -= int32(fr.Len())
				currentWin := c.currentWindow

				atomic.AddInt32(&c.serverWindow, -int32(fr.Len()))
				// TODO: per stream

				data := fr.Body().(*Data)
				if data.Len() > 0 {
					adapt.AppendBody(data.Data())

					// let's imm send the window update
					updateWindow(fr.Stream(), fr.Len(), c.writer)
				}

				if currentWin < c.maxWindow/2 {
					nValue := c.maxWindow - currentWin

					c.currentWindow = c.maxWindow

					updateWindow(0, int(nValue), c.writer)
				}
			case FrameResetStream:
				adapt.Error(fr.Body().(*RstStream).Error())
			}

			handleState(fr, strm)

			if strm.State() == StreamStateClosed {
				adapt.Close()
				delete(strms, strm.ID())
				atomic.AddInt32(&c.openStreams, -1)
			}

			ReleaseFrameHeader(fr)
		}
	}
}

func updateWindow(streamID uint32, n int, writer chan<- *FrameHeader) {
	fr := AcquireFrameHeader()
	fr.SetStream(streamID)

	wu := AcquireFrame(FrameWindowUpdate).(*WindowUpdate)
	wu.SetIncrement(n)

	fr.SetBody(wu)

	writer <- fr
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
	} else {
		// TODO: reply back
	}
}

func writeReset(strm uint32, code ErrorCode, writer chan<- *FrameHeader) {
	r := AcquireFrame(FrameResetStream).(*RstStream)

	fr := AcquireFrameHeader()
	fr.SetStream(strm)
	fr.SetBody(r)

	r.SetCode(code)

	writer <- fr
}

func writeError(strm *Stream, err error, writer chan<- *FrameHeader) {
	code := ErrorCode(InternalError)
	if errors.Is(err, Error{}) {
		code = err.(Error).Code()
	}

	writeReset(strm.ID(), code, writer)
	strm.SetState(StreamStateClosed)
}
