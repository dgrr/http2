package http2

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"net"
	"strconv"

	"github.com/valyala/fasthttp"
)

type Client struct {
	hp     *HPACK
	nextID uint32
	strms  []*Stream
	c      net.Conn
	st     *Settings
}

func NewClient() *Client {
	return &Client{
		nextID: 1,
		hp:     AcquireHPACK(),
		st:     AcquireSettings(),
	}
}

// TODO: checkout https://github.com/golang/net/blob/4acb7895a057/http2/transport.go#L570
func ConfigureClient(c *fasthttp.HostClient) error {
	c2 := NewClient()
	err := c2.Dial(c.Addr, &tls.Config{
		MinVersion: tls.VersionTLS12,
		MaxVersion: tls.VersionTLS13,
		NextProtos: []string{"h2"},
	})
	if err != nil {
		return err
	}

	// TODO: Checkout the tlsconfig....

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

	err = c.Handshake()
	if err != nil {
		conn.Close()
		return err
	}

	return err
}

func (c *Client) Handshake() error {
	conn := c.c

	_, err := conn.Write(http2Preface)
	if err != nil {
		return err
	}

	fr := AcquireFrame()
	defer ReleaseFrame(fr)

	st := AcquireSettings()
	st.WriteFrame(fr)

	_, err = fr.WriteTo(conn)
	if err != nil {
		return err
	}

	return c.writeWindowUpdate(st.MaxWindowSize())
}

func (c *Client) Do(req *fasthttp.Request, res *fasthttp.Response) error {
	err := c.Write(req)
	if err == nil {
		err = c.Read(res)
	}

	return err
}

func (c *Client) Write(req *fasthttp.Request) error {
	// TODO: GET requests can have body too
	// TODO: Send continuation if needed
	noBody := req.Header.IsHead() || req.Header.IsGet()

	strm := acquireStream(c.nextID)
	c.nextID += 2

	fr := AcquireFrame()
	h := AcquireHeaders()
	hf := AcquireHeaderField()

	defer ReleaseFrame(fr)
	defer ReleaseHeaders(h)
	defer ReleaseHeaderField(hf)

	hf.SetBytes(strAuthority, req.Host())
	h.rawHeaders = c.hp.AppendHeader(h.rawHeaders, hf)

	hf.SetBytes(strMethod, req.Header.Method())
	h.rawHeaders = c.hp.AppendHeader(h.rawHeaders, hf)

	hf.SetBytes(strScheme, req.URI().Scheme())
	h.rawHeaders = c.hp.AppendHeader(h.rawHeaders, hf)

	hf.SetBytes(strPath, req.URI().Path())
	h.rawHeaders = c.hp.AppendHeader(h.rawHeaders, hf)

	req.Header.VisitAll(func(k, v []byte) {
		hf.SetBytes(bytes.ToLower(k), v)
		h.rawHeaders = c.hp.AppendHeader(h.rawHeaders, hf)
	})

	h.SetPadding(false)
	h.SetEndStream(!noBody)
	h.SetEndHeaders(true)

	h.WriteFrame(fr)
	fr.SetStream(strm.id)

	_, err := fr.WriteTo(c.c)
	if err == nil {
		strm.state = StateOpen
		c.strms = append(c.strms, strm)
	}

	c.nextID += 2

	return err
}

func (c *Client) Read(res *fasthttp.Response) error {
	var err error

	fr := AcquireFrame()
	defer ReleaseFrame(fr)

	for {
		_, err = fr.ReadFrom(c.c)
		if err != nil {
			return err
		}

		fmt.Println(fr.Stream(), fr.Type())

		var strm *Stream
		strmID := fr.Stream()
		if strmID == 0 {
			continue
		}

		for _, s := range c.strms {
			println(s.id, strmID)
			if s.id == strmID {
				strm = s
				break
			}
		}
		println(strm)
		if strm == nil {
			return fmt.Errorf("stream with id not found: %d", strmID)
		}

		var isEnd bool
		switch fr.Type() {
		case FrameHeaders:
			isEnd, err = c.handleHeaders(fr, res)
		case FrameData:
			isEnd = true
			err = c.handleData(fr, res)
		}

		if isEnd {
			strm.state++
			break
		}
	}

	return err
}

func (c *Client) writeWindowUpdate(update uint32) error {
	fr := AcquireFrame()
	wu := AcquireWindowUpdate()
	wu.SetIncrement(update)

	wu.WriteFrame(fr)
	_, err := fr.WriteTo(c.c)

	ReleaseFrame(fr)
	ReleaseWindowUpdate(wu)

	return err
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

		res.Header.AddBytesKV(hf.KeyBytes(), hf.ValueBytes())
	}

	return h.EndStream(), err
}

func (c *Client) handleData(fr *Frame, res *fasthttp.Response) error {
	dfr := AcquireData()
	defer ReleaseData(dfr)

	err := dfr.ReadFrame(fr)
	if err != nil {
		return err
	}

	res.SetBody(dfr.Data())

	return nil
}
