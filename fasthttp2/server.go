package fasthttp2

import (
	"bytes"
	"crypto/tls"
	"log"
	"net"
	"os"
	"strconv"
	"sync"

	"github.com/dgrr/http2"
	"github.com/valyala/fasthttp"
)

// ConfigureServer configures the fasthttp server to handle
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

// ConfigureServerAndConfig configures the fasthttp server to handle HTTP/2 connections
// and your own tlsConfig file. If you are NOT using your own tls config, you may want to use ConfigureServer.
func ConfigureServerAndConfig(s *fasthttp.Server, tlsConfig *tls.Config) *http2.Server {
	s2 := &http2.Server{
		Adaptor: NewServerAdaptor(s),
	}

	s.NextProto(http2.H2TLSProto, s2.ServeConn)
	tlsConfig.NextProtos = append(tlsConfig.NextProtos, http2.H2TLSProto)

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

var ctxPool = sync.Pool{
	New: func() interface{} {
		return &fasthttp.RequestCtx{}
	},
}

var logger = log.New(os.Stdout, "", log.LstdFlags)

func (sa *ServerAdaptor) OnNewStream(c net.Conn, strm *http2.Stream) {
	ctx := ctxPool.Get().(*fasthttp.RequestCtx)
	ctx.Request.Reset()
	ctx.Response.Reset()

	ctx.Init2(c, logger, false)

	// ctx.Request.Header.DisableNormalizing()
	// ctx.Request.URI().DisablePathNormalizing = true

	strm.SetData(ctx)
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
		scheme := []byte("https")
		req := &ctx.Request

		for len(b) > 0 {
			b, err = dec.Next(hf, b)
			if err != nil {
				break
			}

			k, v := hf.KeyBytes(), hf.ValueBytes()
			if !hf.IsPseudo() &&
				!(bytes.Equal(k, http2.StringUserAgent) ||
					bytes.Equal(k, http2.StringContentType)) {
				req.Header.AddBytesKV(k, v)
				continue
			}

			if hf.IsPseudo() {
				k = k[1:]
			}

			switch k[0] {
			case 'm': // method
				req.Header.SetMethodBytes(v)
			case 'p': // path
				req.Header.SetRequestURIBytes(v)
			case 's': // scheme
				scheme = append(scheme[:0], v...)
			case 'a': // authority
				req.Header.SetHostBytes(v)
				req.Header.AddBytesV("Host", v)
			case 'u': // user-agent
				req.Header.SetUserAgentBytes(v)
			case 'c': // content-type
				req.Header.SetContentTypeBytes(v)
			}
		}

		// calling req.URI() triggers a URL parsing, so because of that we need to delay the URL parsing.
		req.URI().SetSchemeBytes(scheme)
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
	ctx.Request.Header.SetProtocolBytes(http2.StringHTTP2)

	sa.s.Handler(ctx)

	hasBody := len(ctx.Response.Body()) != 0

	fr := http2.AcquireFrameHeader()
	fr.SetStream(strm.ID())

	h := http2.AcquireFrame(http2.FrameHeaders).(*http2.Headers)
	h.SetEndHeaders(true)
	h.SetEndStream(!hasBody)

	fr.SetBody(h)

	fasthttpResponseHeaders(h, enc, &ctx.Response)

	writer <- fr

	if hasBody {
		writeData(strm, ctx.Response.Body(), writer)
	}
}

func writeData(
	strm *http2.Stream, body []byte,
	writer chan<- *http2.FrameHeader,
) {
	step := 1 << 14 // max frame size 16384

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
	ctxPool.Put(strm.Data())
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
