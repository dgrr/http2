package fasthttp2

import (
	"bytes"
	"strconv"

	"github.com/dgrr/http2"
	"github.com/valyala/fasthttp"
)

// ConfigureServer configures the fasthttp's server to handle
// HTTP/2 connections. The HTTP/2 connection can be only
// established if the fasthttp server is using TLS.
//
// Future implementations may support HTTP/2 through plain TCP.
func ConfigureServer(s *fasthttp.Server) *http2.Server {
	s2 := &http2.Server{
		Adaptor: NewServerAdaptor(s),
	}

	s.NextProto(http2.H2TLSProto, s2.ServeConn)

	return s2
}

type ServerAdaptor struct {
	s *fasthttp.Server
}

func NewServerAdaptor(s *fasthttp.Server) *ServerAdaptor {
	return &ServerAdaptor{
		s: s,
	}
}

func (sa *ServerAdaptor) OnNewStream(strm *http2.Stream) {
	strm.SetData(&fasthttp.RequestCtx{})
}

func (sa *ServerAdaptor) OnFrame(
	strm *http2.Stream, fr *http2.FrameHeader, dec *http2.HPACK,
) error {
	var err error
	ctx := strm.Data().(*fasthttp.RequestCtx)

	switch fr.Type() {
	case http2.FrameHeaders, http2.FrameContinuation:
		b := fr.Body().(http2.FrameWithHeaders).Headers()
		hf := http2.AcquireHeaderField()

		for len(b) > 0 {
			b, err = dec.Next(hf, b)
			if err != nil {
				break
			}

			fasthttpRequestHeaders(hf, &ctx.Request)
		}
	case http2.FrameData:
		ctx.Request.AppendBody(
			fr.Body().(*http2.Data).Data())
	}

	return err
}

func (sa *ServerAdaptor) OnRequestFinished(
	strm *http2.Stream, enc *http2.HPACK, writer chan<- *http2.FrameHeader,
) {
	ctx := strm.Data().(*fasthttp.RequestCtx)

	sa.s.Handler(ctx)

	fr := http2.AcquireFrameHeader()
	fr.SetStream(strm.ID())

	h := http2.AcquireFrame(http2.FrameHeaders).(*http2.Headers)
	h.SetEndHeaders(true)
	h.SetEndStream(false)

	fr.SetBody(h)

	fasthttpResponseHeaders(h, enc, &ctx.Response)

	writer <- fr

	writeData(strm, ctx.Response.Body(), writer)
}

func writeData(
	strm *http2.Stream, body []byte,
	writer chan<- *http2.FrameHeader,
) {
	step := strm.Window()

	for i := 0; i < len(body); i += step {
		if i+step >= len(body) {
			step = len(body) - i
		}

		fr := http2.AcquireFrameHeader()
		fr.SetStream(strm.ID())

		data := http2.AcquireFrame(http2.FrameData).(*http2.Data)
		data.SetEndStream(i+step == len(body))
		data.SetPadding(false)
		data.SetData(body[i : step+i])

		fr.SetBody(data)
		writer <- fr
	}
}

func (sa *ServerAdaptor) OnStreamEnd(strm *http2.Stream) {
	// TODO: release...
}

func fasthttpRequestHeaders(hf *http2.HeaderField, req *fasthttp.Request) {
	k, v := hf.KeyBytes(), hf.ValueBytes()
	if !hf.IsPseudo() &&
		!(bytes.Equal(k, http2.StringUserAgent) ||
			bytes.Equal(k, http2.StringContentType)) {
		req.Header.AddBytesKV(k, v)
		return
	}

	if hf.IsPseudo() {
		k = k[1:]
	}

	switch k[0] {
	case 'm': // method
		req.Header.SetMethodBytes(v)
	case 'p': // path
		req.URI().SetPathBytes(v)
	case 's': // scheme
		req.URI().SetSchemeBytes(v)
	case 'a': // authority
		req.URI().SetHostBytes(v)
		req.Header.AddBytesV("Host", v)
	case 'u': // user-agent
		req.Header.SetUserAgentBytes(v)
	case 'c': // content-type
		req.Header.SetContentTypeBytes(v)
	}
}

func fasthttpResponseHeaders(dst *http2.Headers, hp *http2.HPACK, res *fasthttp.Response) {
	hf := http2.AcquireHeaderField()
	defer http2.ReleaseHeaderField(hf)

	hf.SetKeyBytes(http2.StringStatus)
	hf.SetValue(
		strconv.FormatInt(
			int64(res.Header.StatusCode()), 10,
		),
	)

	dst.AppendHeaderField(hp, hf, true)

	res.Header.SetContentLength(len(res.Body()))
	res.Header.VisitAll(func(k, v []byte) {
		hf.SetBytes(http2.ToLower(k), v)
		dst.AppendHeaderField(hp, hf, false)
	})
}