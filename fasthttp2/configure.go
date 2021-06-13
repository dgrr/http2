package fasthttp2

import (
	"bytes"
	"crypto/tls"
	"net"
	"strconv"

	"github.com/dgrr/http2"
	"github.com/valyala/fasthttp"
)

// TODO: checkout https://github.com/golang/net/blob/4acb7895a057/http2/transport.go#L570
func ConfigureClient(c *fasthttp.HostClient) error {
	tlsConfig := c.TLSConfig

	if tlsConfig == nil {
		tlsConfig = &tls.Config{
			MaxVersion: tls.VersionTLS13,
			MinVersion: tls.VersionTLS12,
		}
	}

	emptyServerName := len(tlsConfig.ServerName) == 0
	if emptyServerName {
		host, _, err := net.SplitHostPort(c.Addr)
		if err != nil {
			host = c.Addr
		}

		tlsConfig.ServerName = host
	}

	tlsConfig.NextProtos = append(tlsConfig.NextProtos, "h2")

	c2, err := http2.Dial(c.Addr, tlsConfig)
	if err != nil {
		if err == http2.ErrServerSupport && c.TLSConfig != nil { // remove added config settings
			tlsConfig.NextProtos = tlsConfig.NextProtos[:len(tlsConfig.NextProtos)-1]
			if emptyServerName {
				tlsConfig.ServerName = ""
			}
		}

		return err
	}

	c.TLSConfig = tlsConfig

	c.Transport = Do(c2)

	return nil
}

func Do(c *http2.Client) fasthttp.TransportFunc {
	return func(req *fasthttp.Request, res *fasthttp.Response) error {
		ch := make(chan error, 1)
		c.Register(&reqRes{
			req: req,
			res: res,
			ch:  ch,
		})

		var err error
		select {
		case err = <-ch:
		}

		return err
	}
}

type reqRes struct {
	req *fasthttp.Request
	res *fasthttp.Response
	ch  chan error
}

func (rr *reqRes) Error(err error) {
	rr.ch <- err
}

func (rr *reqRes) Close() {
	close(rr.ch)
}

func (rr *reqRes) SerializeRequest(strm *http2.Stream, enc *http2.HPACK, writer chan<- *http2.FrameHeader) {
	req := rr.req

	hasBody := len(req.Body()) != 0

	fr := http2.AcquireFrameHeader()
	fr.SetStream(strm.ID())

	h := http2.AcquireFrame(http2.FrameHeaders).(*http2.Headers)
	fr.SetBody(h)

	hf := http2.AcquireHeaderField()

	hf.SetBytes(http2.StringAuthority, req.URI().Host())
	enc.AppendHeaderField(h, hf, true)

	hf.SetBytes(http2.StringMethod, req.Header.Method())
	enc.AppendHeaderField(h, hf, true)

	hf.SetBytes(http2.StringPath, req.URI().RequestURI())
	enc.AppendHeaderField(h, hf, true)

	hf.SetBytes(http2.StringScheme, req.URI().Scheme())
	enc.AppendHeaderField(h, hf, true)

	hf.SetBytes(http2.StringUserAgent, req.Header.UserAgent())
	enc.AppendHeaderField(h, hf, true)

	req.Header.VisitAll(func(k, v []byte) {
		if bytes.EqualFold(k, http2.StringUserAgent) {
			return
		}

		hf.SetBytes(http2.ToLower(k), v)
		enc.AppendHeaderField(h, hf, false)
	})

	h.SetPadding(false)
	h.SetEndStream(!hasBody)
	h.SetEndHeaders(true)

	writer <- fr

	if hasBody { // has body
		fr = http2.AcquireFrameHeader()
		fr.SetStream(strm.ID())

		data := http2.AcquireFrame(http2.FrameData).(*http2.Data)

		// TODO: max length
		data.SetEndStream(true)
		data.SetData(req.Body())

		writer <- fr
	}
}

func (rr *reqRes) DeserializeResponse(fr *http2.FrameHeader, dec *http2.HPACK) error {
	var err error
	res := rr.res

	h := fr.Body().(http2.FrameWithHeaders)
	b := h.Headers()

	hf := http2.AcquireHeaderField()
	defer http2.ReleaseHeaderField(hf)

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

		if bytes.Equal(hf.KeyBytes(), http2.StringContentLength) {
			n, _ := strconv.Atoi(hf.Value())
			res.Header.SetContentLength(n)
		} else {
			res.Header.AddBytesKV(hf.KeyBytes(), hf.ValueBytes())
		}
	}

	return nil
}

func (rr *reqRes) AppendBody(body []byte) {
	rr.res.AppendBody(body)
}
