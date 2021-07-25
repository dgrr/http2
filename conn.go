package http2

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/valyala/fasthttp"
)

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

type Conn struct {
	c net.Conn

	br *bufio.Reader
	bw *bufio.Writer

	enc *HPACK
	dec *HPACK

	nextID uint32

	serverWindow       int32
	serverStreamWindow int32

	maxWindow     int32
	currentWindow int32

	openStreams int32

	current Settings
	serverS Settings

	openResps sync.Map

	closed uint64
}

type Dialer struct {
	Addr string

	TLSConfig *tls.Config
}

func (d *Dialer) tryDial() (net.Conn, error) {
	if d.TLSConfig == nil || !func() bool {
		for _, proto := range d.TLSConfig.NextProtos {
			if proto == "h2" {
				return true
			}
		}

		return false
	}() {
		configureDialer(d)
	}

	tcpAddr, err := net.ResolveTCPAddr("tcp", d.Addr)
	if err != nil {
		return nil, err
	}

	c, err := net.DialTCP("tcp", nil, tcpAddr)
	if err != nil {
		return nil, err
	}

	tlsConn := tls.Client(c, d.TLSConfig)

	if err := tlsConn.Handshake(); err != nil {
		c.Close()
		return nil, err
	}

	if tlsConn.ConnectionState().NegotiatedProtocol != "h2" {
		c.Close()
		return nil, ErrServerSupport
	}

	return tlsConn, nil
}

func (d *Dialer) Dial() (*Conn, error) {
	c, err := d.tryDial()
	if err != nil {
		return nil, err
	}

	nc := &Conn{
		c:             c,
		br:            bufio.NewReaderSize(c, 4096),
		bw:            bufio.NewWriterSize(c, maxFrameSize),
		enc:           AcquireHPACK(),
		dec:           AcquireHPACK(),
		nextID:        1,
		maxWindow:     1 << 20,
		currentWindow: 1 << 20,
	}

	nc.current.SetMaxWindowSize(1 << 20)
	nc.current.SetPush(false)

	if err = Handshake(true, nc.bw, &nc.current, nc.maxWindow-65535); err != nil {
		c.Close()
		return nil, err
	}

	var fr *FrameHeader

	if fr, err = ReadFrameFrom(nc.br); err == nil && fr.Type() != FrameSettings {
		return nil, fmt.Errorf("unexpected frame, expected settings, got %s", fr.Type())
	} else if err == nil {
		st := fr.Body().(*Settings)
		if !st.IsAck() {
			err = nc.handleSettings(st)
		}
	}

	if err != nil {
		c.Close()
	} else {
		ReleaseFrameHeader(fr)
	}

	return nc, err
}

func (c *Conn) Closed() bool {
	return atomic.LoadUint64(&c.closed) == 1
}

func (c *Conn) Close() error {
	if !atomic.CompareAndSwapUint64(&c.closed, 0, 1) {
		return io.EOF
	}

	fr := AcquireFrameHeader()
	defer ReleaseFrameHeader(fr)

	ga := AcquireFrame(FrameGoAway).(*GoAway)
	ga.SetStream(0)
	ga.SetCode(NoError)

	fr.SetBody(ga)

	_, err := fr.WriteTo(c.bw)
	if err == nil {
		err = c.bw.Flush()
	}

	c.c.Close()

	return err
}

func (c *Conn) Write(req *fasthttp.Request) (uint32, error) {
	if atomic.LoadUint64(&c.closed) == 1 {
		return 0, io.EOF
	}

	if c.openStreams == int32(c.serverS.maxStreams) {
		return 0, ErrNotAvailableStreams
	}

	hasBody := len(req.Body()) != 0

	enc := c.enc

	id := c.nextID
	c.nextID += 2

	fr := AcquireFrameHeader()
	defer ReleaseFrameHeader(fr)

	fr.SetStream(id)

	h := AcquireFrame(FrameHeaders).(*Headers)
	defer ReleaseFrame(h)

	fr.SetBody(h)

	hf := AcquireHeaderField()

	hf.SetBytes(StringAuthority, req.URI().Host())
	enc.AppendHeaderField(h, hf, true)

	hf.SetBytes(StringMethod, req.Header.Method())
	enc.AppendHeaderField(h, hf, true)

	hf.SetBytes(StringPath, req.URI().RequestURI())
	enc.AppendHeaderField(h, hf, true)

	hf.SetBytes(StringScheme, req.URI().Scheme())
	enc.AppendHeaderField(h, hf, true)

	hf.SetBytes(StringUserAgent, req.Header.UserAgent())
	enc.AppendHeaderField(h, hf, true)

	req.Header.VisitAll(func(k, v []byte) {
		if bytes.EqualFold(k, StringUserAgent) {
			return
		}

		hf.SetBytes(ToLower(k), v)
		enc.AppendHeaderField(h, hf, false)
	})

	h.SetPadding(false)
	h.SetEndStream(!hasBody)
	h.SetEndHeaders(true)

	_, err := fr.WriteTo(c.bw)
	if err == nil && hasBody {
		err = writeData(c.bw, fr, req.Body())
	}

	if err == nil {
		err = c.bw.Flush()
		if err == nil {
			c.openResps.Store(id, fasthttp.AcquireResponse())
			c.openStreams++
		}
	}

	return id, err
}

func writeData(bw *bufio.Writer, fh *FrameHeader, body []byte) (err error) {
	step := 1 << 14

	data := AcquireFrame(FrameData).(*Data)
	fh.SetBody(data)

	for i := 0; err == nil && i < len(body); i += step {
		data.SetEndStream(i+step == len(body))
		data.SetPadding(false)
		data.SetData(body[i : step+i])

		_, err = fh.WriteTo(bw)
	}

	ReleaseFrame(data)

	return err
}

func (c *Conn) Next() (fr *FrameHeader, err error) {
	for err == nil {
		fr, err = ReadFrameFrom(c.br)
		if err != nil {
			break
		}

		if fr.Stream() != 0 {
			if fr.Flags().Has(FlagEndStream) {
				c.openStreams--
			}

			break
		}

		switch fr.Type() {
		case FrameSettings:
			st := fr.Body().(*Settings)
			if !st.IsAck() { // if has ack, just ignore
				err = c.handleSettings(st)
			}
		case FrameWindowUpdate:
			win := int32(fr.Body().(*WindowUpdate).Increment())

			if !atomic.CompareAndSwapInt32(&c.serverWindow, 0, win) {
				atomic.AddInt32(&c.serverWindow, win)
			}
		case FramePing:
			ping := fr.Body().(*Ping)
			if !ping.IsAck() {
				err = c.handlePing(ping)
			}
		case FrameGoAway:
			c.c.Close()
			err = io.EOF
		}

		ReleaseFrameHeader(fr)
	}

	return
}

func (c *Conn) handleSettings(st *Settings) error {
	st.CopyTo(&c.serverS)

	c.serverStreamWindow += int32(c.serverS.MaxWindowSize())

	// reply back
	fr := AcquireFrameHeader()
	defer ReleaseFrameHeader(fr)

	stRes := AcquireFrame(FrameSettings).(*Settings)
	stRes.SetAck(true)

	fr.SetBody(stRes)

	if _, err := fr.WriteTo(c.bw); err == nil {
		return c.bw.Flush()
	} else {
		return err
	}
}

func (c *Conn) handlePing(ping *Ping) error {
	// reply back
	fr := AcquireFrameHeader()
	defer ReleaseFrameHeader(fr)

	ping.SetAck(true)

	fr.SetBody(ping)

	if _, err := fr.WriteTo(c.bw); err == nil {
		return c.bw.Flush()
	} else {
		return err
	}
}

func (c *Conn) readStream(fr *FrameHeader, res *fasthttp.Response) (err error) {
	switch fr.Type() {
	case FrameHeaders, FrameContinuation:
		h := fr.Body().(FrameWithHeaders)
		err = c.readHeader(h.Headers(), res)
	case FrameData:
		c.currentWindow -= int32(fr.Len())
		currentWin := c.currentWindow

		c.serverWindow -= int32(fr.Len())

		data := fr.Body().(*Data)
		if data.Len() != 0 {
			res.AppendBody(data.Data())

			// let's imm send the window update
			err = c.updateWindow(fr.Stream(), fr.Len())
		}

		if err == nil && currentWin < c.maxWindow/2 {
			nValue := c.maxWindow - currentWin

			c.currentWindow = c.maxWindow

			err = c.updateWindow(0, int(nValue))
		}
	}

	return
}

func (c *Conn) updateWindow(streamID uint32, size int) error {
	fr := AcquireFrameHeader()
	defer ReleaseFrameHeader(fr)

	fr.SetStream(streamID)

	wu := AcquireFrame(FrameWindowUpdate).(*WindowUpdate)
	wu.SetIncrement(size)

	fr.SetBody(wu)

	_, err := fr.WriteTo(c.bw)
	if err == nil {
		err = c.bw.Flush()
	}

	return err
}

func (c *Conn) readHeader(b []byte, res *fasthttp.Response) (err error) {
	hf := AcquireHeaderField()
	defer ReleaseHeaderField(hf)

	dec := c.dec

	for len(b) > 0 {
		b, err = dec.Next(hf, b)
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

		if bytes.Equal(hf.KeyBytes(), StringContentLength) {
			n, _ := strconv.Atoi(hf.Value())
			res.Header.SetContentLength(n)
		} else {
			res.Header.AddBytesKV(hf.KeyBytes(), hf.ValueBytes())
		}
	}

	return
}
